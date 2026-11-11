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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// XattrReader reads one extended attribute; a missing attribute reports
// fs.ErrNotExist. It is injectable so the scanner runs on any platform.
type XattrReader func(path, attr string) ([]byte, error)

// TrustedOpaqueFirst is what devpilot-init uses: it runs with CAP_SYS_ADMIN
// before pivot_root and can therefore see trusted.overlay.opaque. The user
// namespace is tried as a fallback for overlay variants that use it.
var TrustedOpaqueFirst XattrReader = readOpaqueXattr

func readOpaqueXattr(path, _ string) ([]byte, error) {
	var firstErr error
	for _, attr := range []string{"trusted.overlay.opaque", "user.overlay.opaque"} {
		value, err := lgetxattr(path, attr)
		if err == nil {
			return value, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// ScanOpaqueDirs walks upper/ and returns every directory carrying
// overlay.opaque=y, as paths relative to upper/, sorted.
//
// devpilot-commit runs unprivileged and cannot read trusted.* xattrs itself,
// so this snapshot is taken here, while CAP_SYS_ADMIN is still held, and
// persisted for it.
func ScanOpaqueDirs(upperDir string, reader XattrReader) ([]string, error) {
	if reader == nil {
		reader = TrustedOpaqueFirst
	}
	var opaque []string
	err := filepath.WalkDir(upperDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A vanished entry is not worth failing the snapshot for.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() || path == upperDir {
			return nil
		}
		value, err := reader(path, "trusted.overlay.opaque")
		if err != nil || len(value) == 0 {
			return nil
		}
		if strings.TrimSpace(string(value)) != "y" {
			return nil
		}
		rel, err := filepath.Rel(upperDir, path)
		if err != nil {
			return fmt.Errorf("compute relative path of %s: %w", path, err)
		}
		opaque = append(opaque, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan opaque dirs: %w", err)
	}
	slices.Sort(opaque)
	return opaque, nil
}

// WriteOpaqueIndex refreshes .meta/opaque-dirs.json for devpilot-commit.
func WriteOpaqueIndex(root string, reader XattrReader) ([]string, error) {
	dirs, err := ScanOpaqueDirs(filepath.Join(root, UpperDir), reader)
	if err != nil {
		return nil, err
	}
	if err := writeJSONAtomic(filepath.Join(root, MetaDir, OpaqueIndexFile), dirs); err != nil {
		return nil, err
	}
	return dirs, nil
}

// ReadOpaqueIndex loads the snapshot written by devpilot-init. A missing file
// yields an empty list: the commit builder then falls back to the user xattr.
func ReadOpaqueIndex(root string) ([]string, error) {
	var dirs []string
	err := readJSON(filepath.Join(root, MetaDir, OpaqueIndexFile), &dirs)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return dirs, nil
}
