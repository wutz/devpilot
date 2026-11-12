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
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	testComment = "devbox-1"
	testEcho    = "echo hello-from-the-devbox"
	testHello   = "hello-from-the-devbox"
	testUser    = "root"
)

// newSigner returns a throwaway ed25519 key, used for both host keys and
// client identities.
func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer
}

// writeAuthorizedKeys stages a key file exactly as the operator's secret
// projection does.
func writeAuthorizedKeys(t *testing.T, keys ...ssh.PublicKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), SSHAuthorizedKeysFile)
	var buf bytes.Buffer
	for _, key := range keys {
		buf.Write(ssh.MarshalAuthorizedKey(key))
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write authorized keys: %v", err)
	}
	return path
}

func TestGenerateHostKeyRoundTrips(t *testing.T) {
	private, public, err := GenerateHostKey(testComment)
	if err != nil {
		t.Fatalf("GenerateHostKey: %v", err)
	}
	if block, _ := pem.Decode(private); block == nil {
		t.Fatal("private key is not PEM encoded")
	}
	signer, err := ParseHostKey(private)
	if err != nil {
		t.Fatalf("ParseHostKey: %v", err)
	}

	line := strings.TrimSpace(string(public))
	if !strings.Contains(line, testComment) {
		t.Errorf("authorized key %q lost its comment", line)
	}
	parsed, err := AuthorizedKeys([]string{line})
	if err != nil {
		t.Fatalf("AuthorizedKeys: %v", err)
	}
	if len(parsed) != 1 || !bytes.Equal(parsed[0].Marshal(), signer.PublicKey().Marshal()) {
		t.Error("the public key does not match the private key it was derived from")
	}
}

func TestAuthorizedKeysRejectsAMalformedLine(t *testing.T) {
	// Silently skipping a bad key would lock the user out of a devbox that
	// reports Running, with nothing in the status to explain why.
	if _, err := AuthorizedKeys([]string{"ssh-ed25519 not-a-key"}); err == nil {
		t.Fatal("expected an error for a malformed key")
	}
}

func TestAuthorizedKeysSkipsBlankLinesAndComments(t *testing.T) {
	signer := newSigner(t)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	keys, err := AuthorizedKeys([]string{"", "  ", "# a note", line})
	if err != nil {
		t.Fatalf("AuthorizedKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("parsed %d keys, want 1", len(keys))
	}
}

// serve starts the server and returns its address plus a stop function.
func serve(t *testing.T, cfg Config) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	cfg.Addr = "127.0.0.1:0"
	cfg.Ready = func(a string) { addr <- a }

	var wg sync.WaitGroup
	wg.Go(func() {
		if err := Serve(ctx, cfg); err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	var bound string
	select {
	case bound = <-addr:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never started listening")
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return bound
}

func TestServeRunsACommandForAnAuthorizedKey(t *testing.T) {
	host := newSigner(t)
	client := newSigner(t)

	addr := serve(t, Config{
		HostKey:            host,
		AuthorizedKeys:     []ssh.PublicKey{client.PublicKey()},
		AuthorizedKeysPath: writeAuthorizedKeys(t, client.PublicKey()),
	})

	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            testUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(client)},
		HostKeyCallback: ssh.FixedHostKey(host.PublicKey()),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	session, err := conn.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer func() { _ = session.Close() }()
	var stderr bytes.Buffer
	session.Stderr = &stderr
	out, err := session.Output(testEcho)
	if err != nil {
		t.Fatalf("run command: %v (stderr %q)", err, stderr.String())
	}
	if got := strings.TrimSpace(string(out)); got != testHello {
		t.Errorf("command output = %q, want %q", got, testHello)
	}
}

// `ssh -tt` from a script asks for a terminal but leaves the size at 0x0, so
// the request has to be honoured on its own. Keying off the size instead
// quietly turned such a session into a pipe, where `tty` fails and full screen
// programs refuse to start.
func TestServeHonoursAPTYRequestOfZeroSize(t *testing.T) {
	host := newSigner(t)
	client := newSigner(t)

	addr := serve(t, Config{
		HostKey:            host,
		AuthorizedKeysPath: writeAuthorizedKeys(t, client.PublicKey()),
	})

	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            testUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(client)},
		HostKeyCallback: ssh.FixedHostKey(host.PublicKey()),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	session, err := conn.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer func() { _ = session.Close() }()
	if err := session.RequestPty("xterm", 0, 0, ssh.TerminalModes{}); err != nil {
		t.Fatalf("request pty: %v", err)
	}
	// The client sends no input, so its stdin closes at once. The shell must
	// still run both commands: closing the PTY master on that EOF kills it with
	// a SIGHUP after the first line, which is what made this fail on Linux.
	out, err := session.Output("tty; echo before; echo after")
	if err != nil {
		t.Fatalf("run tty: %v (output %q)", err, out)
	}
	got := string(out)
	// The driver echoes the end-of-file character the server writes when the
	// client's stdin closes, which shows up as "^D\b\b" in the captured bytes.
	for _, want := range []string{"/dev/", "before", "after"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q is missing %q", got, want)
		}
	}
}

func TestServeRefusesAnUnknownKey(t *testing.T) {
	host := newSigner(t)
	client := newSigner(t)
	stranger := newSigner(t)

	addr := serve(t, Config{
		HostKey:            host,
		AuthorizedKeysPath: writeAuthorizedKeys(t, client.PublicKey()),
	})

	// Password auth is not offered at all, so the handshake fails rather than
	// prompting: a devbox must never answer a password guess.
	_, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            testUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(stranger)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // the test server's key is throwaway
		Timeout:         5 * time.Second,
	})
	if err == nil {
		t.Fatal("an unauthorized key was accepted")
	}
	if !strings.Contains(err.Error(), "unable to authenticate") {
		t.Errorf("dial error = %v, want an authentication failure", err)
	}
}

// The operator adds a key to the mounted secret without restarting the pod, so
// the server has to pick it up on its own.
func TestServePicksUpAnAddedKey(t *testing.T) {
	host := newSigner(t)
	client := newSigner(t)
	path := writeAuthorizedKeys(t, host.PublicKey())

	addr := serve(t, Config{HostKey: host, AuthorizedKeysPath: path})

	// Adding a key file entry after the server is already up.
	if err := os.WriteFile(path, ssh.MarshalAuthorizedKey(client.PublicKey()), 0o600); err != nil {
		t.Fatalf("rewrite authorized keys: %v", err)
	}

	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            testUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(client)},
		HostKeyCallback: ssh.FixedHostKey(host.PublicKey()),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial with the newly authorized key: %v", err)
	}
	_ = conn.Close()
}

func TestServeWithoutKeysFails(t *testing.T) {
	if err := Serve(context.Background(), Config{HostKey: newSigner(t)}); err == nil {
		t.Fatal("expected an error with no authorized keys")
	}
}

func TestParseDims(t *testing.T) {
	// columns=120, rows=40, then the pixel fields a terminal leaves at zero.
	payload := []byte{0, 120, 0, 40, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	dims := parseDims(payload)
	if dims == nil || dims.Cols != 120 || dims.Rows != 40 {
		t.Fatalf("dims = %+v, want 120x40", dims)
	}
	if parseDims([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) != nil {
		t.Error("a zero size must be rejected so the PTY default applies")
	}
	if parseDims(nil) != nil {
		t.Error("a truncated payload must be rejected")
	}
}
