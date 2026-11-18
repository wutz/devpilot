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
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/wutz/devpilot/internal/ext4"
)

// Ext4Source reads the overlay upper tree directly from an ext4 filesystem
// image (disk.img) without mounting it on the host. This lets the unprivileged
// devpilot-commit Job package root disks stored as disk.img on shared storage.
type Ext4Source struct {
	Img *ext4.Image
	// Root is the directory inside the ext4 image to walk, typically "upper".
	// Empty or "." walks from the filesystem root.
	Root string
}

func (s *Ext4Source) rootPath() string {
	if s.Root == "" {
		return "."
	}
	return s.Root
}

// Walk walks the tree below Root, parents before children, in lexical order.
func (s *Ext4Source) Walk(fn func(rel string, info fs.FileInfo) error) error {
	if s.Img == nil {
		return fmt.Errorf("ext4 image is required")
	}
	root := s.rootPath()
	err := s.Img.Walk(root, func(rel string, ino *ext4.Inode) error {
		if rel == "." {
			return nil
		}
		return fn(rel, ino.FileInfo(path.Base(rel)))
	})
	if err != nil {
		return fmt.Errorf("walk ext4 %s: %w", root, err)
	}
	return nil
}

// Readlink returns the target of a symlink entry.
func (s *Ext4Source) Readlink(rel string, info fs.FileInfo) (string, error) {
	ino, err := s.inodeFor(rel, info)
	if err != nil {
		return "", err
	}
	return s.Img.Readlink(ino)
}

// Open returns the content of a regular file entry.
func (s *Ext4Source) Open(rel string, info fs.FileInfo) (io.ReadCloser, error) {
	ino, err := s.inodeFor(rel, info)
	if err != nil {
		return nil, err
	}
	sr, err := s.Img.OpenSection(ino)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(sr), nil
}

// Inode returns the ext4 inode number for hardlink tracking.
func (s *Ext4Source) Inode(info fs.FileInfo) (uint64, bool) {
	if info == nil {
		return 0, false
	}
	ino, ok := info.Sys().(*ext4.Inode)
	if !ok || ino == nil {
		return 0, false
	}
	return uint64(ino.Number), true
}

// IsWhiteout reports an OverlayFS deletion marker: a 0:0 character device or
// a zero-byte regular file carrying an overlay.whiteout extended attribute.
func (s *Ext4Source) IsWhiteout(info fs.FileInfo) bool {
	if info == nil {
		return false
	}
	ino, ok := info.Sys().(*ext4.Inode)
	if !ok || ino == nil {
		return false
	}
	if info.Mode()&fs.ModeCharDevice != 0 && ino.DevMajor == 0 && ino.DevMinor == 0 {
		return true
	}
	if info.Mode().IsRegular() && ino.Size == 0 && s.Img != nil {
		xattrs, err := s.Img.Xattrs(ino)
		if err != nil {
			return false
		}
		if _, ok := xattrs["trusted.overlay.whiteout"]; ok {
			return true
		}
		if _, ok := xattrs["user.overlay.whiteout"]; ok {
			return true
		}
	}
	return false
}

// Opaque reports whether the directory carries trusted.overlay.opaque=y or
// user.overlay.opaque=y directly on its ext4 inode.
func (s *Ext4Source) Opaque(rel string, info fs.FileInfo) bool {
	if s.Img == nil {
		return false
	}
	ino, err := s.inodeFor(rel, info)
	if err != nil {
		return false
	}
	xattrs, err := s.Img.Xattrs(ino)
	if err != nil {
		return false
	}
	for _, attr := range []string{"trusted.overlay.opaque", "user.overlay.opaque"} {
		if val, ok := xattrs[attr]; ok && strings.TrimSpace(string(val)) == "y" {
			return true
		}
	}
	return false
}

func (s *Ext4Source) inodeFor(rel string, info fs.FileInfo) (*ext4.Inode, error) {
	if info != nil {
		if ino, ok := info.Sys().(*ext4.Inode); ok && ino != nil {
			return ino, nil
		}
	}
	if s.Img == nil {
		return nil, fmt.Errorf("ext4 image is required")
	}
	p := rel
	if root := s.rootPath(); root != "." {
		p = path.Join(root, rel)
	}
	return s.Img.Lookup(p)
}
