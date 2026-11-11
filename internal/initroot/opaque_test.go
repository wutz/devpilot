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
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const testOpaqueDir = "opt/conda"

// fakeXattrs marks the given absolute paths as overlay.opaque=y.
func fakeXattrs(opaque ...string) XattrReader {
	return func(path, _ string) ([]byte, error) {
		if slices.Contains(opaque, path) {
			return []byte("y"), nil
		}
		return nil, fs.ErrNotExist
	}
}

// buildUpper creates upper/ with the directories the commit builder cares
// about: an opaque replacement of /opt/conda, an opaque subtree and a few
// plain directories and files that must be ignored.
func buildUpper(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	upper := filepath.Join(root, UpperDir)
	for _, dir := range []string{
		testOpaqueDir + "/bin",
		"var/lib/apt/lists",
		"usr/local/bin",
	} {
		if err := os.MkdirAll(filepath.Join(upper, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(upper, "usr/local/bin/uv"), []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, MetaDir), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestScanOpaqueDirsFindsOpaqueDirectories(t *testing.T) {
	root := buildUpper(t)
	upper := filepath.Join(root, UpperDir)

	dirs, err := ScanOpaqueDirs(upper, fakeXattrs(
		filepath.Join(upper, testOpaqueDir),
		filepath.Join(upper, "var/lib/apt/lists"),
	))
	if err != nil {
		t.Fatalf("ScanOpaqueDirs() error = %v", err)
	}
	want := []string{testOpaqueDir, "var/lib/apt/lists"}
	if !slices.Equal(dirs, want) {
		t.Errorf("ScanOpaqueDirs() = %v, want %v", dirs, want)
	}
}

func TestScanOpaqueDirsIgnoresNonOpaqueValues(t *testing.T) {
	root := buildUpper(t)
	dir := filepath.Join(root, UpperDir, testOpaqueDir)

	reader := func(path, _ string) ([]byte, error) {
		if path == dir {
			return []byte("n"), nil
		}
		return nil, fs.ErrNotExist
	}
	dirs, err := ScanOpaqueDirs(filepath.Join(root, UpperDir), reader)
	if err != nil {
		t.Fatalf("ScanOpaqueDirs() error = %v", err)
	}
	if len(dirs) != 0 {
		t.Errorf("ScanOpaqueDirs() = %v, want none", dirs)
	}
}

// A first boot scans before the overlay ever existed, so a missing upper/ is
// an empty index rather than a failure.
func TestScanOpaqueDirsMissingUpperIsEmpty(t *testing.T) {
	dirs, err := ScanOpaqueDirs(filepath.Join(t.TempDir(), "absent"), fakeXattrs())
	if err != nil {
		t.Fatalf("ScanOpaqueDirs() error = %v", err)
	}
	if len(dirs) != 0 {
		t.Errorf("ScanOpaqueDirs() = %v, want none", dirs)
	}
}

func TestWriteOpaqueIndexRoundTrip(t *testing.T) {
	root := buildUpper(t)
	upper := filepath.Join(root, UpperDir)

	dirs, err := WriteOpaqueIndex(root, fakeXattrs(filepath.Join(upper, testOpaqueDir)))
	if err != nil {
		t.Fatalf("WriteOpaqueIndex() error = %v", err)
	}
	if !slices.Equal(dirs, []string{testOpaqueDir}) {
		t.Errorf("WriteOpaqueIndex() = %v, want [opt/conda]", dirs)
	}

	got, err := ReadOpaqueIndex(root)
	if err != nil {
		t.Fatalf("ReadOpaqueIndex() error = %v", err)
	}
	if !slices.Equal(got, []string{testOpaqueDir}) {
		t.Errorf("ReadOpaqueIndex() = %v, want [opt/conda]", got)
	}
}

func TestReadOpaqueIndexMissingIsEmpty(t *testing.T) {
	got, err := ReadOpaqueIndex(t.TempDir())
	if err != nil {
		t.Fatalf("ReadOpaqueIndex() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ReadOpaqueIndex() = %v, want nil", got)
	}
}

func TestReadOpaqueIndexRejectsCorruptFile(t *testing.T) {
	root := buildUpper(t)
	if err := os.WriteFile(filepath.Join(root, MetaDir, OpaqueIndexFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOpaqueIndex(root); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadOpaqueIndex() error = %v, want a parse error", err)
	}
}
