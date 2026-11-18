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
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// freezeThenThaw is the call order of a session that froze and thawed once.
const freezeThenThaw = "freeze,thaw"

// fakeFS records freeze and thaw calls in order.
type fakeFS struct {
	mu        sync.Mutex
	calls     []string
	freezeErr error
	thawErrs  int
}

func (f *fakeFS) freeze() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "freeze")
	return f.freezeErr
}

func (f *fakeFS) thaw() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "thaw")
	if f.thawErrs > 0 {
		f.thawErrs--
		return errors.New("thaw refused")
	}
	return nil
}

func (f *fakeFS) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// freezeHarness runs RunFreeze in the background with pipes for stdin and
// stdout, the way pods/exec wires the helper.
type freezeHarness struct {
	t      *testing.T
	fs     *fakeFS
	stdin  *io.PipeWriter
	stdout *bufio.Reader
	sigs   chan os.Signal
	done   chan int
}

func startFreeze(t *testing.T, fs *fakeFS, timeout time.Duration) *freezeHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &freezeHarness{
		t: t, fs: fs, stdin: inW, stdout: bufio.NewReader(outR),
		sigs: make(chan os.Signal, 1), done: make(chan int, 1),
	}
	go func() {
		code := RunFreeze(FreezeOptions{
			Timeout: timeout,
			Stdin:   inR,
			Stdout:  outW,
			Logf:    func(msg string, kv ...any) { t.Log(FormatMessage(msg, kv...)) },
			Signals: h.sigs,
			Freeze:  fs.freeze,
			Thaw:    fs.thaw,
		})
		_ = outW.Close()
		h.done <- code
	}()
	t.Cleanup(func() { _ = inW.Close() })
	return h
}

func (h *freezeHarness) readLine() string {
	h.t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := h.stdout.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil && r.line == "" {
			return ""
		}
		return r.line
	case <-time.After(5 * time.Second):
		h.t.Fatal("timed out waiting for helper output")
		return ""
	}
}

func (h *freezeHarness) wait() int {
	h.t.Helper()
	select {
	case code := <-h.done:
		return code
	case <-time.After(5 * time.Second):
		h.t.Fatal("helper did not exit")
		return -1
	}
}

func TestRunFreezeReleasedByStdinEOF(t *testing.T) {
	fs := &fakeFS{}
	h := startFreeze(t, fs, time.Minute)
	if got := h.readLine(); got != FrozenLine+"\n" {
		t.Fatalf("first line = %q, want %q", got, FrozenLine)
	}
	if calls := fs.snapshot(); len(calls) != 1 || calls[0] != "freeze" {
		t.Fatalf("calls before release = %v, want only freeze", calls)
	}
	_ = h.stdin.Close()
	if got := h.readLine(); got != ThawedLine+"\n" {
		t.Errorf("second line = %q, want %q", got, ThawedLine)
	}
	if code := h.wait(); code != FreezeExitReleased {
		t.Errorf("exit code = %d, want %d", code, FreezeExitReleased)
	}
	if calls := fs.snapshot(); strings.Join(calls, ",") != freezeThenThaw {
		t.Errorf("calls = %v, want freeze,thaw", calls)
	}
}

func TestRunFreezeReleasedByLine(t *testing.T) {
	fs := &fakeFS{}
	h := startFreeze(t, fs, time.Minute)
	h.readLine()
	if _, err := h.stdin.Write([]byte("thaw\n")); err != nil {
		t.Fatal(err)
	}
	if got := h.readLine(); got != ThawedLine+"\n" {
		t.Errorf("second line = %q, want %q", got, ThawedLine)
	}
	if code := h.wait(); code != FreezeExitReleased {
		t.Errorf("exit code = %d, want %d", code, FreezeExitReleased)
	}
}

func TestRunFreezeTimeoutForcesThaw(t *testing.T) {
	fs := &fakeFS{}
	h := startFreeze(t, fs, 50*time.Millisecond)
	h.readLine()
	if got := h.readLine(); got != ThawedLine+"\n" {
		t.Errorf("second line = %q, want %q", got, ThawedLine)
	}
	if code := h.wait(); code != FreezeExitForced {
		t.Errorf("exit code = %d, want %d after the timeout", code, FreezeExitForced)
	}
	if calls := fs.snapshot(); strings.Join(calls, ",") != freezeThenThaw {
		t.Errorf("calls = %v, want freeze,thaw", calls)
	}
}

func TestRunFreezeSignalForcesThaw(t *testing.T) {
	fs := &fakeFS{}
	h := startFreeze(t, fs, time.Minute)
	h.readLine()
	h.sigs <- syscall.SIGTERM
	if got := h.readLine(); got != ThawedLine+"\n" {
		t.Errorf("second line = %q, want %q", got, ThawedLine)
	}
	if code := h.wait(); code != FreezeExitForced {
		t.Errorf("exit code = %d, want %d after a signal", code, FreezeExitForced)
	}
}

func TestRunFreezeFailureNeverThaws(t *testing.T) {
	fs := &fakeFS{freezeErr: errors.New("device or resource busy")}
	h := startFreeze(t, fs, time.Minute)
	if got := h.readLine(); got != "" {
		t.Errorf("stdout = %q, want nothing when the freeze fails", got)
	}
	if code := h.wait(); code != FreezeExitError {
		t.Errorf("exit code = %d, want %d", code, FreezeExitError)
	}
	if calls := fs.snapshot(); strings.Join(calls, ",") != "freeze" {
		t.Errorf("calls = %v, want only freeze", calls)
	}
}

func TestRunFreezeRetriesThaw(t *testing.T) {
	fs := &fakeFS{thawErrs: 2}
	h := startFreeze(t, fs, time.Minute)
	h.readLine()
	_ = h.stdin.Close()
	h.readLine()
	if code := h.wait(); code != FreezeExitReleased {
		t.Errorf("exit code = %d, want %d once a retry thaws", code, FreezeExitReleased)
	}
	if calls := fs.snapshot(); strings.Join(calls, ",") != "freeze,thaw,thaw,thaw" {
		t.Errorf("calls = %v, want three thaw attempts", calls)
	}
}

func TestRunFreezeReportsFailedThaw(t *testing.T) {
	fs := &fakeFS{thawErrs: 10}
	h := startFreeze(t, fs, time.Minute)
	h.readLine()
	_ = h.stdin.Close()
	if code := h.wait(); code != FreezeExitThawFailed {
		t.Errorf("exit code = %d, want %d", code, FreezeExitThawFailed)
	}
}

func TestRunFreezeReleaseDuringFreezeStillThaws(t *testing.T) {
	fs := &fakeFS{}
	inR, inW := io.Pipe()
	var out strings.Builder
	code := RunFreeze(FreezeOptions{
		Timeout: time.Minute,
		Stdin:   inR,
		Stdout:  &out,
		Signals: make(chan os.Signal),
		// The caller releases while FIFREEZE is still in progress; the
		// release must be honoured right after the announcement instead of
		// holding the freeze until the timeout.
		Freeze: func() error {
			_ = inW.Close()
			time.Sleep(50 * time.Millisecond)
			return fs.freeze()
		},
		Thaw: fs.thaw,
	})
	if code != FreezeExitReleased {
		t.Errorf("exit code = %d, want %d", code, FreezeExitReleased)
	}
	if calls := strings.Join(fs.snapshot(), ","); calls != freezeThenThaw {
		t.Errorf("calls = %q, want freeze,thaw", calls)
	}
	if got, want := out.String(), FrozenLine+"\n"+ThawedLine+"\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// fakeInspector treats paths below an "overlay" directory as overlayfs.
func fakeInspector() rootDiskInspector {
	return rootDiskInspector{
		openDir: func(path string) (*os.File, error) {
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			info, err := f.Stat()
			if err != nil || !info.IsDir() {
				_ = f.Close()
				return nil, errors.New("not a directory")
			}
			return f, nil
		},
		fsMagic: func(f *os.File) (int64, error) {
			target, err := filepath.EvalSymlinks(f.Name())
			if err != nil {
				return 0, err
			}
			if strings.Contains(target, string(filepath.Separator)+"overlay"+string(filepath.Separator)) {
				return overlayFSMagic, nil
			}
			return 0xef53, nil
		},
		hasRegular: func(f *os.File, name string) bool {
			info, err := os.Lstat(filepath.Join(f.Name(), name))
			return err == nil && info.Mode().IsRegular()
		},
	}
}

func TestFindRootDiskMetaPicksTheMetaDir(t *testing.T) {
	base := t.TempDir()
	procFD := filepath.Join(base, "proc", "1", "fd")
	mustMkdir(t, procFD)

	// upper/ has no base-image.json; the overlay copy of .meta must be
	// ignored; a regular file and a pipe-like link are skipped too.
	upper := filepath.Join(base, "rootdisk", UpperDir)
	meta := filepath.Join(base, "rootdisk", MetaDir)
	overlayMeta := filepath.Join(base, "overlay", MetaDir)
	for _, dir := range []string{upper, meta, overlayMeta} {
		mustMkdir(t, dir)
	}
	mustWrite(t, filepath.Join(meta, BaseImageFile))
	mustWrite(t, filepath.Join(overlayMeta, BaseImageFile))
	plain := filepath.Join(base, "file")
	mustWrite(t, plain)

	links := map[string]string{
		"0":  "pipe:[1234]",
		"3":  overlayMeta,
		"4":  plain,
		"5":  upper,
		"12": meta,
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(procFD, name)); err != nil {
			t.Fatal(err)
		}
	}

	f, err := findRootDiskMeta(filepath.Join(base, "proc"), fakeInspector())
	if err != nil {
		t.Fatalf("findRootDiskMeta() error = %v", err)
	}
	defer func() { _ = f.Close() }()
	if want := filepath.Join(procFD, "12"); f.Name() != want {
		t.Errorf("findRootDiskMeta() = %s, want %s", f.Name(), want)
	}
}

func TestFindRootDiskMetaFailsClearly(t *testing.T) {
	base := t.TempDir()
	procFD := filepath.Join(base, "proc", "1", "fd")
	mustMkdir(t, procFD)
	upper := filepath.Join(base, UpperDir)
	mustMkdir(t, upper)
	if err := os.Symlink(upper, filepath.Join(procFD, "3")); err != nil {
		t.Fatal(err)
	}

	_, err := findRootDiskMeta(filepath.Join(base, "proc"), fakeInspector())
	if !errors.Is(err, ErrRootDiskNotFound) {
		t.Errorf("findRootDiskMeta() error = %v, want ErrRootDiskNotFound", err)
	}
	if _, err := findRootDiskMeta(filepath.Join(base, "missing"), fakeInspector()); err == nil {
		t.Error("findRootDiskMeta() without procfs succeeded")
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
