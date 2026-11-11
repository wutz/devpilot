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
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSnapshot records what Boot asks the commit-side metadata to publish.
type fakeSnapshot struct {
	mu          sync.Mutex
	refreshes   int
	generations []GenerationMeta
	closed      bool
}

func (s *fakeSnapshot) Refresh() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshes++
	return []string{testOpaqueDir}, nil
}

func (s *fakeSnapshot) WriteGeneration(meta GenerationMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generations = append(s.generations, meta)
	return nil
}

func (s *fakeSnapshot) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *fakeSnapshot) lastGeneration() (GenerationMeta, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.generations) == 0 {
		return GenerationMeta{}, false
	}
	return s.generations[len(s.generations)-1], true
}

type bootHarness struct {
	snapshot *fakeSnapshot
	setups   []Paths
	child    *fakeProcess
}

func newBootHarness() *bootHarness {
	return &bootHarness{snapshot: &fakeSnapshot{}, child: newFakeProcess()}
}

func (h *bootHarness) options(root string) BootOptions {
	return BootOptions{
		Root:        root,
		ImageRef:    testImageRefA,
		ImageDigest: testImageDigestA,
		Lower:       BaseLower,
		GracePeriod: time.Second,
		Command:     Command{Path: "/usr/bin/sleep", Args: []string{"infinity"}, Env: []string{"PATH=/usr/bin"}},
		SetupFn: func(p Paths) error {
			h.setups = append(h.setups, p)
			return nil
		},
		Starter:      func(Command) (Process, error) { return h.child, nil },
		OpenSnapshot: func(string) (OpaqueFlusher, error) { return h.snapshot, nil },
		// Tests must never sync the host filesystem: syncfs is unbounded and
		// would make the suite depend on runner disk pressure.
		SyncFn: func() error { return nil },
	}
}

func TestBootPinsBaseImageAndRunsCommand(t *testing.T) {
	root := newRootDisk(t)
	h := newBootHarness()

	done := make(chan runResult, 1)
	go func() {
		code, err := Boot(h.options(root))
		done <- runResult{code: code, err: err}
	}()

	h.child.exits <- 0
	got := waitResult(t, done)
	if got.err != nil || got.code != 0 {
		t.Fatalf("Boot() = (%d, %v), want (0, nil)", got.code, got.err)
	}

	meta, err := ReadBaseImageMeta(root)
	if err != nil {
		t.Fatalf("ReadBaseImageMeta() error = %v", err)
	}
	if meta.ImageDigest != testImageDigestA {
		t.Errorf("pinned digest = %q, want %q", meta.ImageDigest, testImageDigestA)
	}

	if len(h.setups) != 1 {
		t.Fatalf("Setup ran %d times, want 1", len(h.setups))
	}
	if h.setups[0].Root != root || h.setups[0].Lower != BaseLower {
		t.Errorf("Setup paths = %+v, want root %q lower %q", h.setups[0], root, BaseLower)
	}

	last, ok := h.snapshot.lastGeneration()
	if !ok {
		t.Fatal("no generation was ever written")
	}
	if !last.CleanUnmount {
		t.Error("clean unmount flag was not set before Boot returned")
	}
	if !h.snapshot.closed {
		t.Error("snapshot handle was not closed")
	}
	if h.snapshot.refreshes == 0 {
		t.Error("opaque index was never refreshed")
	}
}

func TestBootRecordsGenerationSequence(t *testing.T) {
	root := newRootDisk(t)
	if err := WriteGenerationMeta(root, GenerationMeta{Generation: 41}); err != nil {
		t.Fatal(err)
	}

	h := newBootHarness()
	done := make(chan runResult, 1)
	go func() {
		code, err := Boot(h.options(root))
		done <- runResult{code: code, err: err}
	}()
	h.child.exits <- 0
	waitResult(t, done)

	if h.snapshot.generations[0].Generation != 42 {
		t.Errorf("first generation written = %d, want 42", h.snapshot.generations[0].Generation)
	}
	if h.snapshot.generations[0].CleanUnmount {
		t.Error("generation was written as clean before the command even ran")
	}
}

func TestBootPropagatesCommandExitCode(t *testing.T) {
	root := newRootDisk(t)
	h := newBootHarness()

	done := make(chan runResult, 1)
	go func() {
		code, err := Boot(h.options(root))
		done <- runResult{code: code, err: err}
	}()
	h.child.exits <- 9

	got := waitResult(t, done)
	if got.code != 9 {
		t.Errorf("Boot() = %d, want the command's exit code 9", got.code)
	}
}

func TestBootRefusesDigestMismatch(t *testing.T) {
	root := newRootDisk(t)
	if err := os.WriteFile(root+"/"+UpperDir+"/marker", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteBaseImageMeta(root, BaseImageMeta{ImageRef: testImageRefB, ImageDigest: testImageDigestB}); err != nil {
		t.Fatal(err)
	}

	h := newBootHarness()
	code, err := Boot(h.options(root))
	if err == nil || code != 1 {
		t.Fatalf("Boot() = (%d, %v), want (1, error)", code, err)
	}
	if len(h.setups) != 0 {
		t.Error("Setup ran even though the digest did not match")
	}
}

// A rebase starts from a different base image than the one the disk was
// created on, which is exactly what the regular pin refuses.
func TestBootRebaseClearsThePreviousBaseImage(t *testing.T) {
	root := newRootDisk(t)
	if err := os.WriteFile(filepath.Join(root, UpperDir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteBaseImageMeta(root, BaseImageMeta{
		ImageRef:    testImageRefB,
		ImageDigest: testImageDigestB,
		RebaseCount: 1,
	}); err != nil {
		t.Fatal(err)
	}

	h := newBootHarness()
	opts := h.options(root)
	opts.Rebase = true

	done := make(chan runResult, 1)
	go func() {
		code, err := Boot(opts)
		done <- runResult{code: code, err: err}
	}()
	h.child.exits <- 0
	if got := waitResult(t, done); got.err != nil {
		t.Fatalf("Boot() error = %v, want a rebase onto a different image to succeed", got.err)
	}

	empty, err := UpperIsEmpty(root)
	if err != nil || !empty {
		t.Errorf("UpperIsEmpty() = (%v, %v), want (true, nil)", empty, err)
	}
	meta, err := ReadBaseImageMeta(root)
	if err != nil {
		t.Fatalf("ReadBaseImageMeta() error = %v", err)
	}
	if meta.ImageDigest != testImageDigestA {
		t.Errorf("pinned digest = %q, want the rebased image %q", meta.ImageDigest, testImageDigestA)
	}
	if meta.RebaseCount != 2 {
		t.Errorf("RebaseCount = %d, want 2", meta.RebaseCount)
	}
}

func TestBootStopsWhenSetupFails(t *testing.T) {
	root := newRootDisk(t)
	h := newBootHarness()
	opts := h.options(root)
	opts.SetupFn = func(Paths) error { return errors.New("mount denied") }

	code, err := Boot(opts)
	if err == nil || code != 1 {
		t.Fatalf("Boot() = (%d, %v), want (1, error)", code, err)
	}
	if !strings.Contains(err.Error(), "mount denied") {
		t.Errorf("error = %v, want the setup failure", err)
	}
	if !h.snapshot.closed {
		t.Error("snapshot handle leaked after a setup failure")
	}
}

// A fresh PVC has no upper/ or .meta/ yet. The snapshot is opened before Setup
// runs, so Boot must create the layout itself; otherwise the very first start
// of every DevBox fails with "open upper dir: no such file or directory".
func TestBootCreatesLayoutBeforeOpeningSnapshot(t *testing.T) {
	root := t.TempDir()
	h := newBootHarness()
	opts := h.options(root)
	opts.OpenSnapshot = func(path string) (OpaqueFlusher, error) {
		for _, dir := range []string{UpperDir, WorkDir, MergedDir, MetaDir} {
			if _, err := os.Stat(filepath.Join(path, dir)); err != nil {
				return nil, err
			}
		}
		return h.snapshot, nil
	}

	done := make(chan runResult, 1)
	go func() {
		code, err := Boot(opts)
		done <- runResult{code: code, err: err}
	}()
	h.child.exits <- 0

	if got := waitResult(t, done); got.err != nil {
		t.Fatalf("Boot() error = %v, want the layout to exist before the snapshot", got.err)
	}
}

func TestBootValidatesOptions(t *testing.T) {
	h := newBootHarness()
	cases := map[string]func(*BootOptions){
		"relative root":  func(o *BootOptions) { o.Root = "root" },
		"empty image":    func(o *BootOptions) { o.ImageRef = "" },
		"empty digest":   func(o *BootOptions) { o.ImageDigest = "" },
		"empty lowerdir": func(o *BootOptions) { o.Lower = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			opts := h.options(t.TempDir())
			mutate(&opts)
			if code, err := Boot(opts); err == nil || code != 1 {
				t.Errorf("Boot() = (%d, %v), want (1, error)", code, err)
			}
		})
	}
}

func TestBootDefaultsCommandEnvironment(t *testing.T) {
	root := newRootDisk(t)
	h := newBootHarness()
	opts := h.options(root)
	opts.Logf = t.Logf
	var started Command
	opts.Starter = func(cmd Command) (Process, error) {
		started = cmd
		return h.child, nil
	}

	done := make(chan runResult, 1)
	go func() {
		code, err := Boot(opts)
		done <- runResult{code: code, err: err}
	}()
	h.child.exits <- 0
	waitResult(t, done)

	if !slices.Equal(started.Env, []string{"PATH=/usr/bin"}) {
		t.Errorf("command env = %v, want the configured environment", started.Env)
	}
}
