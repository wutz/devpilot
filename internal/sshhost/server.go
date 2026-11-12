/*
Copyright 2026 wutz.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sshhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

// ptyEOF is the terminal driver's end-of-file character (Ctrl-D). Writing it is
// how a client that has nothing more to send says so without hanging up.
const ptyEOF = 0x04

// Config describes the built-in server. It exists because a devbox image may
// carry no sshd at all (the NVIDIA PyTorch images ship none), and the user must
// still be able to open a session.
type Config struct {
	// Addr is the listen address, e.g. ":22".
	Addr string
	// HostKey signs the server side of the handshake.
	HostKey ssh.Signer
	// AuthorizedKeys are the only identities that may log in.
	AuthorizedKeys []ssh.PublicKey
	// AuthorizedKeysPath, when set, is re-read on every authentication attempt.
	// That is what lets the operator add a key to the mounted secret without
	// replacing a running pod, which would drop every GPU process with it.
	AuthorizedKeysPath string
	// Shell is the interactive login shell, /bin/bash by default.
	Shell string
	// Env is the environment handed to sessions. Callers pass the init
	// process's own environment so the image's PATH is inherited.
	Env []string
	// Logf receives lifecycle messages.
	Logf func(msg string, kv ...any)
	// Ready, when set, receives the bound address once the listener is up.
	// Tests use it to connect to port 0 without racing the accept loop.
	Ready func(addr string)
}

// Load fills the key material from disk. The operator writes both files into a
// Secret it owns, so the format is the one sshd itself reads: an OpenSSH private
// key and one authorized key per line.
func (c *Config) Load(privateKeyPath, authorizedKeysPath string) error {
	private, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return fmt.Errorf("read host key %s: %w", privateKeyPath, err)
	}
	signer, err := ParseHostKey(private)
	if err != nil {
		return err
	}
	c.HostKey = signer

	raw, err := os.ReadFile(authorizedKeysPath)
	if err != nil {
		return fmt.Errorf("read authorized keys %s: %w", authorizedKeysPath, err)
	}
	keys, err := AuthorizedKeys(strings.Split(string(raw), "\n"))
	if err != nil {
		return err
	}
	c.AuthorizedKeys = keys
	return nil
}

// allowedKeys returns the keys a connection may authenticate with. A path wins
// over the in-memory list, so a rotated key file takes effect at the next login
// rather than at the next pod restart. A read failure falls back to the copy
// loaded at start: a transient mount problem should not lock the user out.
func (c Config) allowedKeys() ([]ssh.PublicKey, error) {
	if c.AuthorizedKeysPath == "" {
		return c.AuthorizedKeys, nil
	}
	raw, err := os.ReadFile(c.AuthorizedKeysPath)
	if err != nil {
		if len(c.AuthorizedKeys) > 0 {
			return c.AuthorizedKeys, nil
		}
		return nil, fmt.Errorf("read authorized keys: %w", err)
	}
	keys, err := AuthorizedKeys(strings.Split(string(raw), "\n"))
	if err != nil {
		if len(c.AuthorizedKeys) > 0 {
			return c.AuthorizedKeys, nil
		}
		return nil, err
	}
	// An empty file is the operator having removed every key, and refusing every
	// login is the right answer: reviving the keys the pod started with would
	// keep access open after the user asked for it to be closed.
	return keys, nil
}

// Serve accepts connections until ctx is cancelled. Every session gets its own
// PTY-backed shell, so line editing, job control and curses programs work the
// way they do over a normal SSH connection.
func Serve(ctx context.Context, cfg Config) error {
	if cfg.HostKey == nil {
		return errors.New("no host key")
	}
	if len(cfg.AuthorizedKeys) == 0 && cfg.AuthorizedKeysPath == "" {
		// Reporting a running server that nobody can log in to is worse than a
		// visible failure: the user would find out only when their key is refused.
		return errors.New("no authorized keys; set spec.access.ssh.authorizedKeys first")
	}
	if cfg.Shell == "" {
		cfg.Shell = "/bin/bash"
	}
	logf := logger(cfg)

	server := &ssh.ServerConfig{
		// Key only: a password prompt turns the devbox into an unthrottled brute
		// force target, and the operator already distributes public keys.
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			allowed, err := cfg.allowedKeys()
			if err != nil {
				logf("Could not read authorized keys", "err", err.Error())
				return nil, err
			}
			for _, candidate := range allowed {
				if keysEqual(candidate, key) {
					logf("Accepted SSH key", "user", conn.User(), "fingerprint", ssh.FingerprintSHA256(key))
					return nil, nil
				}
			}
			return nil, fmt.Errorf("key %s is not authorized", ssh.FingerprintSHA256(key))
		},
	}
	server.AddHostKey(cfg.HostKey)

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	defer func() { _ = listener.Close() }()
	logf("Serving SSH", "addr", listener.Addr().String())
	if cfg.Ready != nil {
		cfg.Ready(listener.Addr().String())
	}

	// Cancellation has to unblock the accept loop; nothing else closes the
	// listener while it waits in Accept.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stop:
		}
	}()

	var wg sync.WaitGroup
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil || errors.Is(acceptErr, net.ErrClosed) {
				wg.Wait()
				return nil
			}
			return fmt.Errorf("accept: %w", acceptErr)
		}
		wg.Go(func() {
			handle(ctx, conn, server, cfg)
		})
	}
}

// handle runs the handshake and then one goroutine per session channel until
// the client goes away.
func handle(ctx context.Context, conn net.Conn, server *ssh.ServerConfig, cfg Config) {
	logf := logger(cfg)
	defer func() { _ = conn.Close() }()

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, server)
	if err != nil {
		// Common and not actionable: port scanners, probes and clients that gave
		// up during the handshake all land here.
		logf("SSH handshake failed", "remote", conn.RemoteAddr().String(), "err", err.Error())
		return
	}
	defer func() { _ = sshConn.Close() }()
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only interactive sessions are supported")
			continue
		}
		channel, requests, acceptErr := newChannel.Accept()
		if acceptErr != nil {
			logf("Could not accept SSH channel", "err", acceptErr.Error())
			continue
		}
		go session(ctx, channel, requests, sshConn.User(), cfg)
	}
}

// session serves one channel. It keeps reading requests after the shell starts,
// because window-change arrives for the whole life of the session; a session
// that stopped reading would leave resized terminals at their original size.
func session(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request, user string, cfg Config) {
	logf := logger(cfg)
	defer func() { _ = channel.Close() }()

	var (
		term     string
		dims     *pty.Winsize
		ptyReq   bool
		spec     sessionSpec
		started  bool
		finished = make(chan struct{})
		termFile = &terminal{}
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-finished:
			// The shell already sent its exit status. Closing the channel here
			// is what the client reads as EOF: without it `ssh host cmd` blocks
			// even though the command is long gone.
			return
		case req, ok := <-requests:
			if !ok {
				return
			}
			switch req.Type {
			case "pty-req":
				// The request itself, not the size, is what asks for a terminal:
				// `ssh -tt` from a script sends 0x0, and treating that as "no
				// PTY" silently downgrades the session to a pipe.
				term, dims = parsePTYRequest(req.Payload)
				ptyReq = true
				_ = req.Reply(true, nil)
			case "env":
				// Accepted so clients do not fail; the server's environment is
				// authoritative and shared by every session.
				_ = req.Reply(true, nil)
			case "window-change":
				if newDims := parseWindowChange(req.Payload); newDims != nil {
					termFile.resize(newDims)
				}
				_ = req.Reply(true, nil)
			case "shell", "exec":
				if started {
					_ = req.Reply(false, nil)
					continue
				}
				started = true
				_ = req.Reply(true, nil)
				spec = sessionSpec{term: term, dims: dims, user: user, pty: ptyReq}
				if req.Type == "exec" {
					spec.command = parseString(req.Payload)
				}
				go func() {
					defer close(finished)
					code := runShell(ctx, channel, termFile, spec, cfg, logf)
					_ = sendExitStatus(channel, uint32(code))
				}()
			default:
				// subsystem (sftp), agent forwarding and X11 are not offered.
				_ = req.Reply(false, nil)
			}
		}
	}
}

// sessionSpec is what the client asked for before the shell started.
type sessionSpec struct {
	command string
	term    string
	dims    *pty.Winsize
	user    string
	// pty records that the client sent pty-req, which is what makes a session
	// interactive even when it left the size at zero.
	pty bool
}

// runShell starts the shell, pumps bytes both ways and returns the exit status,
// which is what makes `ssh host cmd; echo $?` meaningful.
func runShell(
	ctx context.Context, channel ssh.Channel, termFile *terminal,
	spec sessionSpec, cfg Config, logf func(string, ...any),
) int {
	shell, args := cfg.Shell, []string(nil)
	if spec.command != "" {
		shell, args = "/bin/sh", []string{"-c", spec.command}
	}

	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Env = append([]string{
		"TERM=" + termOrDefault(spec.term),
		"USER=" + spec.user,
		"LOGNAME=" + spec.user,
		"HOME=/root",
	}, cfg.Env...)
	// Sessions start in the user's home, which is what a login shell does. A
	// missing directory (a distroless image, or the test host) leaves the
	// inherited working directory rather than failing every command.
	cmd.Dir = workDir(cfg.Env)

	if !spec.pty && spec.command != "" {
		// No PTY: a plain command run, where stderr stays separate from stdout.
		cmd.Stdin, cmd.Stdout, cmd.Stderr = channel, channel, channel.Stderr()
		return exitCode(cmd.Run(), logf, spec)
	}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		logf("Could not start a PTY", "user", spec.user, "err", err.Error())
		return 1
	}
	// A zero size from the client would leave the kernel default of 0x0, which
	// makes the shell's line editor unusable, so fall back to a normal terminal.
	dims := spec.dims
	if dims == nil {
		dims = &pty.Winsize{Cols: 80, Rows: 24}
	}
	termFile.attach(ptmx, dims)
	defer func() {
		termFile.attach(nil, nil)
		_ = ptmx.Close()
	}()

	// Client to shell. End of input is the terminal's own end-of-file character
	// rather than closing the master: closing it sends the shell a SIGHUP, which
	// kills a command that has already produced its output — `ssh -tt host cmd`
	// from a script closes its stdin immediately, so the shell must be allowed to
	// run to completion.
	go func() {
		_, _ = io.Copy(ptmx, channel)
		_, _ = ptmx.Write([]byte{ptyEOF})
	}()
	_, _ = io.Copy(channel, ptmx)

	return exitCode(cmd.Wait(), logf, spec)
}

// terminal owns the PTY of a running session so a window-change that arrives
// after the shell started still reaches it.
type terminal struct {
	mu   sync.Mutex
	ptmx *os.File
}

func (t *terminal) attach(ptmx *os.File, dims *pty.Winsize) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ptmx = ptmx
	if ptmx != nil && dims != nil {
		_ = pty.Setsize(ptmx, dims)
	}
}

func (t *terminal) resize(dims *pty.Winsize) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ptmx != nil {
		_ = pty.Setsize(t.ptmx, dims)
	}
}

// keysEqual compares two public keys by their wire encoding. Comparing the
// marshalled form rather than a string is what makes a certificate differ from
// the plain key it wraps, which is the behaviour we want: the operator lists
// plain keys, and an accepted certificate must name one of them explicitly.
func keysEqual(a, b ssh.PublicKey) bool {
	if a == nil || b == nil {
		return false
	}
	return bytes.Equal(a.Marshal(), b.Marshal())
}

// workDir resolves the directory a session starts in: HOME when it exists, as
// a login shell would use, and "" otherwise so the inherited directory stands.
func workDir(env []string) string {
	for _, entry := range env {
		value, found := strings.CutPrefix(entry, "HOME=")
		if !found || value == "" {
			continue
		}
		if info, err := os.Stat(value); err == nil && info.IsDir() {
			return value
		}
		return ""
	}
	return ""
}

// logger returns a non-nil sink so call sites never repeat the nil check.
func logger(cfg Config) func(string, ...any) {
	if cfg.Logf != nil {
		return cfg.Logf
	}
	return func(string, ...any) {}
}

// exitCode turns a Wait result into a status. A session killed by a signal has
// no exit code, and reporting 1 for it is the convention clients expect.
func exitCode(err error, logf func(string, ...any), spec sessionSpec) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		if code := exitErr.ExitCode(); code > 0 {
			return code
		}
		return 1
	}
	logf("SSH session failed", "user", spec.user, "command", spec.command, "err", err.Error())
	return 1
}

func sendExitStatus(channel ssh.Channel, code uint32) error {
	_, err := channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
	return err
}

func termOrDefault(term string) string {
	if term == "" {
		return "xterm-256color"
	}
	return term
}

// parsePTYRequest reads the terminal name and the initial size from a pty-req.
func parsePTYRequest(payload []byte) (string, *pty.Winsize) {
	term, rest := parseStringWithRest(payload)
	return strings.TrimSpace(term), parseDims(rest)
}

// parseWindowChange reads a new size from a window-change: columns and rows
// first, then the pixel dimensions that terminals normally leave at zero.
func parseWindowChange(payload []byte) *pty.Winsize {
	return parseDims(payload)
}

// parseDims decodes the four big-endian uint32 fields that follow the terminal
// name in both pty-req and window-change.
func parseDims(b []byte) *pty.Winsize {
	if len(b) < 16 {
		return nil
	}
	read := func(i int) uint16 {
		return uint16(b[i])<<8 | uint16(b[i+1])
	}
	dims := &pty.Winsize{Cols: read(0), Rows: read(2), X: read(4), Y: read(6)}
	if dims.Cols == 0 || dims.Rows == 0 {
		// A zero size makes the shell's line editor unusable; the PTY default is
		// better than nothing.
		return nil
	}
	return dims
}

// parseString reads a length-prefixed string at the start of a payload.
func parseString(payload []byte) string {
	s, _ := parseStringWithRest(payload)
	return strings.TrimSpace(s)
}

// parseStringWithRest splits an SSH string off the front of a payload.
func parseStringWithRest(payload []byte) (string, []byte) {
	if len(payload) < 4 {
		return "", nil
	}
	length := int(payload[0])<<24 | int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	if length < 0 || 4+length > len(payload) {
		return "", nil
	}
	return string(payload[4 : 4+length]), payload[4+length:]
}
