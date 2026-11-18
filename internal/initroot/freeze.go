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

package initroot

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The freeze helper protocol. The operator execs "/proc/1/exe --freeze" with
// stdin attached, waits for FrozenLine on stdout, takes the volume snapshot and
// then closes stdin (or writes any line) to release the filesystem.
const (
	// FrozenLine is printed once FIFREEZE succeeded.
	FrozenLine = "frozen"
	// ThawedLine is printed once FITHAW returned, whatever released it.
	ThawedLine = "thawed"

	// FreezeExitReleased means the caller released the freeze through stdin
	// while the filesystem was still frozen; a snapshot cut in between is
	// consistent.
	FreezeExitReleased = 0
	// FreezeExitError means the filesystem was never frozen: the root disk
	// could not be located or FIFREEZE was refused (for example EBUSY when a
	// previous freeze is still active, or EOPNOTSUPP).
	FreezeExitError = 2
	// FreezeExitForced means the timeout or a signal thawed the filesystem
	// before the caller released it. Any snapshot taken during the session
	// must be treated as invalid.
	FreezeExitForced = 3
	// FreezeExitThawFailed means FITHAW itself failed and the filesystem may
	// still be frozen; "--thaw" is the recovery.
	FreezeExitThawFailed = 4

	// DefaultFreezeTimeout bounds a freeze when the caller does not choose.
	DefaultFreezeTimeout = 60 * time.Second
	// DefaultProcRoot is where procfs is mounted.
	DefaultProcRoot = "/proc"

	// overlayFSMagic is OVERLAYFS_SUPER_MAGIC. The merged root and anything
	// below it reports it, and freezing the overlay is neither supported nor
	// what the snapshot needs: the root disk below it is.
	overlayFSMagic = 0x794c7630
)

// ErrRootDiskNotFound is returned when no descriptor of PID 1 resolves to the
// root disk's .meta directory.
var ErrRootDiskNotFound = errors.New(
	"no descriptor of PID 1 points at the root disk .meta directory; " +
		"is PID 1 devpilot-init and does this process have CAP_SYS_ADMIN?")

// FreezeOptions drives one freeze session. Freeze and Thaw act on the root
// disk filesystem; they are injectable so the session logic is testable on
// any platform without a real filesystem to freeze.
type FreezeOptions struct {
	// Timeout forces a thaw if the caller never releases the freeze. Zero or
	// negative means DefaultFreezeTimeout.
	Timeout time.Duration
	// Stdin carries the release: EOF or any line.
	Stdin io.Reader
	// Stdout receives FrozenLine and ThawedLine.
	Stdout io.Writer
	// Logf receives lifecycle messages; nil discards them. It must not write
	// to the frozen filesystem.
	Logf LogFunc
	// Signals overrides the OS signal source; tests inject a channel.
	Signals <-chan os.Signal
	// Freeze flushes and freezes the filesystem.
	Freeze func() error
	// Thaw releases it.
	Thaw func() error
}

func (o FreezeOptions) logf(msg string, kv ...any) {
	if o.Logf != nil {
		o.Logf(msg, kv...)
	}
}

// RunFreeze freezes the filesystem, announces it, waits for the release and
// thaws. The thaw is deferred, so it also runs when anything in between
// panics. It returns the process exit code (FreezeExit*).
func RunFreeze(opts FreezeOptions) (code int) {
	if opts.Freeze == nil || opts.Thaw == nil {
		opts.logf("Freeze helper is not configured")
		return FreezeExitError
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultFreezeTimeout
	}

	sigs := opts.Signals
	if sigs == nil {
		// A write to a closed stdout must return EPIPE rather than kill the
		// process with SIGPIPE: dying here would leave the filesystem frozen.
		signal.Ignore(syscall.SIGPIPE)
		osSigs := make(chan os.Signal, 4)
		signal.Notify(osSigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
		defer signal.Stop(osSigs)
		sigs = osSigs
	}

	// Listen for the release before the filesystem is frozen, so nothing that
	// can block runs while it is.
	released := make(chan struct{})
	if opts.Stdin != nil {
		go func() {
			// Any outcome counts as a release: a line, EOF, or a broken
			// stream when the caller went away.
			_, _ = bufio.NewReader(opts.Stdin).ReadString('\n')
			close(released)
		}()
	}

	// A caller that already hung up cannot cut a snapshot; freezing now would
	// only stall the DevBox until the release is noticed.
	select {
	case <-released:
		opts.logf("Freeze released before it started; not freezing")
		return FreezeExitError
	default:
	}

	if err := opts.Freeze(); err != nil {
		opts.logf("Could not freeze the root filesystem", "err", err.Error())
		return FreezeExitError
	}

	forced := true
	thawed := false
	defer func() {
		if thawed {
			return
		}
		thawed = true
		if err := thawWithRetry(opts.Thaw); err != nil {
			opts.logf("Could not thaw the root filesystem; run --thaw to recover", "err", err.Error())
			code = FreezeExitThawFailed
			return
		}
		_, _ = fmt.Fprintln(opts.Stdout, ThawedLine)
		flushWriter(opts.Stdout)
		if forced {
			code = FreezeExitForced
		} else {
			code = FreezeExitReleased
		}
		opts.logf("Thawed the root filesystem", "released", !forced)
	}()

	if _, err := fmt.Fprintln(opts.Stdout, FrozenLine); err != nil {
		// Nobody can release a freeze it was never told about.
		opts.logf("Could not announce the freeze; thawing", "err", err.Error())
		return FreezeExitForced
	}
	flushWriter(opts.Stdout)
	opts.logf("Froze the root filesystem", "timeout", timeout.String())

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-released:
		forced = false
	case <-timer.C:
		opts.logf("Freeze timeout expired; thawing before the release", "timeout", timeout.String())
	case sig := <-sigs:
		opts.logf("Received signal while frozen; thawing", "signal", sig.String())
	}
	// The deferred thaw sets the exit code.
	return FreezeExitForced
}

// thawWithRetry retries FITHAW briefly: a failed thaw leaves every writer in
// the DevBox blocked, which is worse than a short delay.
func thawWithRetry(thaw func() error) error {
	var err error
	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		if err = thaw(); err == nil {
			return nil
		}
	}
	return err
}

func flushWriter(w io.Writer) {
	switch f := w.(type) {
	case interface{ Flush() error }:
		_ = f.Flush()
	case interface{ Flush() }:
		f.Flush()
	}
}

// rootDiskInspector holds the filesystem queries the root disk lookup needs.
// Linux implements them with fstatfs and fstatat; tests fake them.
type rootDiskInspector struct {
	// openDir opens path as a directory, failing for anything else.
	openDir func(path string) (*os.File, error)
	// fsMagic returns the f_type of the filesystem behind f.
	fsMagic func(f *os.File) (int64, error)
	// hasRegular reports whether name is a regular file inside f.
	hasRegular func(f *os.File, name string) bool
}

// findRootDiskMeta walks the descriptors of PID 1 and returns the first one
// that is the root disk's .meta directory: a directory, not on the overlay,
// holding base-image.json. devpilot-init opens it before pivot_root and keeps
// it for its whole life, which makes it the only path to the root disk
// filesystem once the overlay is /.
func findRootDiskMeta(procRoot string, in rootDiskInspector) (*os.File, error) {
	fdDir := filepath.Join(procRoot, "1", "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil, fmt.Errorf("list descriptors of PID 1: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.SortFunc(names, func(x, y string) int {
		a, errA := strconv.Atoi(x)
		b, errB := strconv.Atoi(y)
		if errA != nil || errB != nil {
			return strings.Compare(x, y)
		}
		return a - b
	})

	for _, name := range names {
		path := filepath.Join(fdDir, name)
		// Pipes, sockets and anonymous inodes link to "pipe:[...]" and the
		// like; only a path can be the directory we want, and skipping the
		// rest avoids opening a FIFO at all.
		if target, err := os.Readlink(path); err != nil || !strings.HasPrefix(target, "/") {
			continue
		}
		f, err := in.openDir(path)
		if err != nil {
			continue
		}
		if magic, err := in.fsMagic(f); err != nil || magic == overlayFSMagic {
			_ = f.Close()
			continue
		}
		if !in.hasRegular(f, BaseImageFile) {
			_ = f.Close()
			continue
		}
		return f, nil
	}
	return nil, ErrRootDiskNotFound
}

// FreezeRootDisk is the "--freeze" entry point: it locates the root disk
// through PID 1, freezes it and runs the release protocol on stdin/stdout.
func FreezeRootDisk(opts FreezeOptions) int {
	dir, err := FindRootDiskMeta(DefaultProcRoot)
	if err != nil {
		opts.logf("Could not locate the root disk", "err", err.Error())
		return FreezeExitError
	}
	defer func() { _ = dir.Close() }()
	opts.Freeze = func() error { return freezeFS(dir) }
	opts.Thaw = func() error { return thawFS(dir) }
	return RunFreeze(opts)
}

// ThawRootDisk is the "--thaw" entry point. A filesystem that is not frozen
// counts as thawed, so it is safe as an unconditional recovery step.
func ThawRootDisk() error {
	dir, err := FindRootDiskMeta(DefaultProcRoot)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return thawFS(dir)
}
