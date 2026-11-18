//go:build linux

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
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestFindRootDiskMetaThroughProcfs runs the real lookup against this test
// process's own descriptors, standing in for PID 1's.
func TestFindRootDiskMetaThroughProcfs(t *testing.T) {
	base := t.TempDir()
	var st unix.Statfs_t
	if err := unix.Statfs(base, &st); err != nil {
		t.Fatal(err)
	}
	if int64(st.Type) == overlayFSMagic { //nolint:unconvert // Type is int32 on 32-bit architectures
		t.Skip("the temp dir is on overlayfs, which the lookup deliberately ignores")
	}

	meta := filepath.Join(base, MetaDir)
	mustMkdir(t, meta)
	mustWrite(t, filepath.Join(meta, BaseImageFile))
	held, err := os.Open(meta)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	procRoot := filepath.Join(base, "proc")
	mustMkdir(t, procRoot)
	if err := os.Symlink("/proc/self", filepath.Join(procRoot, "1")); err != nil {
		t.Fatal(err)
	}

	f, err := FindRootDiskMeta(procRoot)
	if err != nil {
		t.Fatalf("FindRootDiskMeta() error = %v", err)
	}
	defer func() { _ = f.Close() }()

	var got, want unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &got); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fstat(int(held.Fd()), &want); err != nil {
		t.Fatal(err)
	}
	if got.Dev != want.Dev || got.Ino != want.Ino {
		t.Errorf("FindRootDiskMeta() opened %s, want the held .meta directory", f.Name())
	}
}
