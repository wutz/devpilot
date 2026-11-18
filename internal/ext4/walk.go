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

package ext4

import (
	"errors"
	"fmt"
	"io/fs"
)

const maxWalkDepth = 4096

// Walk walks the tree rooted at root (resolved with Lookup) in lexical
// order, parents before children. fn is called first with rel "." for root
// itself, then with slash-separated paths relative to root. Returning
// fs.SkipDir from fn for a directory skips its contents (for a
// non-directory it skips the remaining entries of the containing
// directory); fs.SkipAll stops the walk without error. Any other error
// aborts the walk and is returned.
//
// Directory hard links / cycles in a corrupted image are reported as errors
// rather than walked repeatedly.
func (img *Image) Walk(root string, fn func(rel string, ino *Inode) error) error {
	ino, err := img.Lookup(root)
	if err != nil {
		return err
	}
	w := walker{img: img, fn: fn, seen: map[uint32]bool{}}
	err = w.visit(".", ino, 0)
	if errors.Is(err, fs.SkipDir) || errors.Is(err, fs.SkipAll) {
		return nil
	}
	return err
}

type walker struct {
	img  *Image
	fn   func(string, *Inode) error
	seen map[uint32]bool
}

func (w *walker) visit(rel string, ino *Inode, depth int) error {
	if err := w.fn(rel, ino); err != nil {
		return err
	}
	if !ino.IsDir() {
		return nil
	}
	if depth >= maxWalkDepth {
		return corruptf("directory tree deeper than %d at %q", maxWalkDepth, rel)
	}
	if w.seen[ino.Number] {
		return corruptf("directory inode %d reached twice (at %q)", ino.Number, rel)
	}
	w.seen[ino.Number] = true
	ents, err := w.img.ReadDir(ino)
	if err != nil {
		return fmt.Errorf("ext4: %s: %w", rel, err)
	}
	for _, e := range ents {
		child, err := w.img.Inode(e.Inode)
		if err != nil {
			return fmt.Errorf("ext4: %s/%s: %w", rel, e.Name, err)
		}
		crel := e.Name
		if rel != "." {
			crel = rel + "/" + e.Name
		}
		err = w.visit(crel, child, depth+1)
		if err == nil {
			continue
		}
		if errors.Is(err, fs.SkipDir) {
			if child.IsDir() {
				continue
			}
			return nil // skip the rest of this directory
		}
		return err
	}
	return nil
}
