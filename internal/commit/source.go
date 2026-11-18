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

package commit

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
)

// Source is the read-only overlay upper tree WriteLayer packages. The
// directory source reads a mounted root disk; the ext4 image source parses a
// disk.img from a shared-filesystem root disk without mounting it.
type Source interface {
	// Walk calls fn for every entry below the root, parents before
	// children and siblings in lexical order. Returning fs.SkipDir for a
	// directory skips its contents. Entries that vanish during the walk are
	// skipped.
	Walk(fn func(rel string, info fs.FileInfo) error) error
	// Readlink returns the target of a symlink found by Walk.
	Readlink(rel string, info fs.FileInfo) (string, error)
	// Open returns the content of a regular file found by Walk.
	Open(rel string, info fs.FileInfo) (io.ReadCloser, error)
	// Inode identifies the file for hardlink detection.
	Inode(info fs.FileInfo) (uint64, bool)
	// IsWhiteout reports an OverlayFS deletion marker.
	IsWhiteout(info fs.FileInfo) bool
	// Opaque reports whether a directory carries the overlay.opaque xattr.
	// Sources that cannot read trusted.* xattrs return false and rely on
	// Options.OpaqueDirs.
	Opaque(rel string, info fs.FileInfo) bool
}

// DirSource reads upper/ from a mounted directory.
type DirSource struct {
	Root string
}

// Check validates the root.
func (d DirSource) Check() error {
	info, err := osStat(d.Root)
	if err != nil {
		return fmt.Errorf("stat upper dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("upper dir %s is not a directory", d.Root)
	}
	return nil
}

// Walk walks the directory without following symlinks.
func (d DirSource) Walk(fn func(rel string, info fs.FileInfo) error) error {
	err := filepath.WalkDir(d.Root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			// Entries deleted while the scan runs (tmp files, logs) are not
			// worth failing a commit over.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if p == d.Root {
			return nil
		}
		rel, err := filepath.Rel(d.Root, p)
		if err != nil {
			return fmt.Errorf("compute relative path of %s: %w", p, err)
		}
		info, err := de.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		return fn(filepath.ToSlash(rel), info)
	})
	if err != nil {
		return fmt.Errorf("walk upper dir: %w", err)
	}
	return nil
}

// Readlink reads a symlink.
func (d DirSource) Readlink(rel string, _ fs.FileInfo) (string, error) {
	return readlink(filepath.Join(d.Root, filepath.FromSlash(rel)))
}

// Open opens a regular file without following symlinks.
func (d DirSource) Open(rel string, _ fs.FileInfo) (io.ReadCloser, error) {
	return openRegular(filepath.Join(d.Root, filepath.FromSlash(rel)))
}

// Inode returns the inode number from the stat data.
func (d DirSource) Inode(info fs.FileInfo) (uint64, bool) {
	if info.Sys() == nil {
		return 0, false
	}
	return inodeOf(info)
}

// IsWhiteout reports a 0:0 character device.
func (d DirSource) IsWhiteout(info fs.FileInfo) bool {
	return info.Mode()&fs.ModeCharDevice != 0 && isZeroDevice(info)
}

// Opaque is always false: an unprivileged process cannot read
// trusted.overlay.opaque, so the index devpilot-init wrote is used instead.
func (d DirSource) Opaque(string, fs.FileInfo) bool { return false }
