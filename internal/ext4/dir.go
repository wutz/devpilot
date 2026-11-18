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
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

// Directory entry file types (ext4_dir_entry_2.file_type).
const (
	FileTypeUnknown  = 0
	FileTypeRegular  = 1
	FileTypeDir      = 2
	FileTypeCharDev  = 3
	FileTypeBlockDev = 4
	FileTypeFIFO     = 5
	FileTypeSocket   = 6
	FileTypeSymlink  = 7
)

const (
	// Limits that keep ReadDir memory bounded on hostile images (indirect
	// maps can reference the same directory block many times).
	maxDirEntries   = 1 << 22
	maxDirNameBytes = 256 << 20
	maxDirBlocks    = 1 << 22
	dirReadChunk    = 64 // blocks per ReadAt when scanning directories
)

// ErrNotDir is returned when a directory operation is applied to a non-directory.
var ErrNotDir = errors.New("not a directory")

// DirEntry is a directory entry.
type DirEntry struct {
	Name  string
	Inode uint32
	Type  uint8 // ext4 dirent file_type (FileType* constants), 0 without the filetype feature
}

// direntVisitor is called for every live entry (including "." and "..");
// returning stop=true ends the iteration.
type direntVisitor func(name []byte, ino uint32, typ uint8) (stop bool, err error)

// parseDirBlock walks the ext4_dir_entry_2 records of one directory block
// (or inline-data region). Entries with inode 0 (deleted entries, htree
// dx_root/dx_node fake entries, metadata_csum tails) are skipped.
func (img *Image) parseDirBlock(dirIno uint32, b []byte, fn direntVisitor) (bool, error) {
	le := binary.LittleEndian
	filetype := img.sb.incompat&incompatFiletype != 0
	bs := img.blockSize
	off := 0
	for off < len(b) {
		if len(b)-off < 8 {
			return false, corruptf("directory inode %d: truncated entry", dirIno)
		}
		ino := le.Uint32(b[off:])
		rl := int(le.Uint16(b[off+4:]))
		if bs >= 65536 {
			if rl == 65535 || rl == 0 {
				rl = int(bs)
			} else {
				rl = (rl & 65532) | ((rl & 3) << 16)
			}
		}
		nameLen := int(b[off+6])
		typ := b[off+7]
		if !filetype {
			nameLen |= int(typ) << 8
			typ = 0
		}
		if rl < 12 || rl%4 != 0 || rl > len(b)-off {
			return false, corruptf("directory inode %d: bad rec_len %d at offset %d", dirIno, rl, off)
		}
		if 8+nameLen > rl {
			return false, corruptf("directory inode %d: name_len %d exceeds rec_len %d", dirIno, nameLen, rl)
		}
		if ino != 0 {
			if ino > img.sb.inodesCount {
				return false, corruptf("directory inode %d: entry refers to inode %d", dirIno, ino)
			}
			name := b[off+8 : off+8+nameLen]
			if nameLen == 0 || bytes.IndexByte(name, '/') >= 0 || bytes.IndexByte(name, 0) >= 0 {
				return false, corruptf("directory inode %d: invalid entry name %q", dirIno, name)
			}
			stop, err := fn(name, ino, typ)
			if err != nil || stop {
				return stop, err
			}
		}
		off += rl
	}
	return false, nil
}

// iterateDir visits all entries of a directory inode, linear or htree (the
// htree index blocks look like empty entries to a linear scan) or inline.
func (img *Image) iterateDir(dir *Inode, fn direntVisitor) error {
	if !dir.IsDir() {
		return fmt.Errorf("ext4: inode %d: %w", dir.Number, ErrNotDir)
	}
	if dir.encrypted() {
		return fmt.Errorf("inode %d: %w", dir.Number, ErrEncrypted)
	}
	if dir.inline() {
		return img.iterateInlineDir(dir, fn)
	}
	f, err := img.openReader(dir)
	if err != nil {
		return err
	}
	bs := img.blockSize
	nblocks := uint64((dir.Size + bs - 1) / bs)
	buf := make([]byte, min(int64(nblocks), dirReadChunk)*bs)
	stopped := false
	errStop := errors.New("stop")
	scanned := uint64(0)
	err = f.forEachRun(nblocks, func(lblk uint64, r blockRun) error {
		if r.kind != runData {
			return nil // holes / unwritten blocks hold no entries
		}
		// A real directory cannot have more data blocks than the image
		// holds; hostile block maps can repeat the same block endlessly.
		scanned += r.count
		if limit := min(maxDirBlocks, uint64(img.size/bs)); scanned > limit {
			return corruptf("directory inode %d: more than %d data blocks", dir.Number, limit)
		}
		for done := uint64(0); done < r.count; {
			n := min(r.count-done, dirReadChunk)
			chunk := buf[:int64(n)*bs]
			if err := img.readPhys(chunk, r.pblk+done, 0); err != nil {
				return err
			}
			for i := range n {
				blk := chunk[int64(i)*bs : int64(i+1)*bs]
				stop, err := img.parseDirBlock(dir.Number, blk, fn)
				if err != nil {
					return err
				}
				if stop {
					stopped = true
					return errStop
				}
			}
			done += n
		}
		return nil
	})
	if stopped {
		return nil
	}
	return err
}

func (img *Image) iterateInlineDir(dir *Inode, fn direntVisitor) error {
	parent := binary.LittleEndian.Uint32(dir.iblock[0:])
	if parent == 0 || parent > img.sb.inodesCount {
		return corruptf("inline directory inode %d: bad parent %d", dir.Number, parent)
	}
	if stop, err := fn([]byte("."), dir.Number, FileTypeDir); err != nil || stop {
		return err
	}
	if stop, err := fn([]byte(".."), parent, FileTypeDir); err != nil || stop {
		return err
	}
	stop, err := img.parseDirBlock(dir.Number, dir.iblock[4:], fn)
	if err != nil || stop {
		return err
	}
	sys, err := img.systemData(dir)
	if err != nil {
		return err
	}
	if len(sys) > 0 {
		_, err = img.parseDirBlock(dir.Number, sys, fn)
	}
	return err
}

// ReadDir returns the entries of a directory inode, excluding "." and "..",
// sorted by name.
func (img *Image) ReadDir(dir *Inode) ([]DirEntry, error) {
	var out []DirEntry
	nameBytes := 0
	err := img.iterateDir(dir, func(name []byte, ino uint32, typ uint8) (bool, error) {
		if string(name) == "." || string(name) == ".." {
			return false, nil
		}
		nameBytes += len(name)
		if len(out) >= maxDirEntries || nameBytes > maxDirNameBytes {
			return false, corruptf("directory inode %d: too many entries", dir.Number)
		}
		out = append(out, DirEntry{Name: string(name), Inode: ino, Type: typ})
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b DirEntry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// lookupChild finds name (which may be "." or "..") in directory dir.
func (img *Image) lookupChild(dir *Inode, name string) (uint32, bool, error) {
	var found uint32
	err := img.iterateDir(dir, func(n []byte, ino uint32, _ uint8) (bool, error) {
		if string(n) == name {
			found = ino
			return true, nil
		}
		return false, nil
	})
	return found, found != 0, err
}

const opLookup = "lookup"

// Lookup resolves a slash-separated path from the root directory ("" or "/"
// is the root) without following symlinks: a symlink as an intermediate
// component is an error, a symlink as the last component is returned as is.
// Missing entries yield an error wrapping fs.ErrNotExist.
func (img *Image) Lookup(path string) (*Inode, error) {
	cur, err := img.Inode(rootIno)
	if err != nil {
		return nil, err
	}
	comps := strings.Split(path, "/")
	last := -1
	for i, c := range comps {
		if c != "" && c != "." {
			last = i
		}
	}
	for i, c := range comps {
		if c == "" || c == "." {
			continue
		}
		if !cur.IsDir() {
			return nil, &fs.PathError{Op: opLookup, Path: path, Err: ErrNotDir}
		}
		num, ok, err := img.lookupChild(cur, c)
		if err != nil {
			return nil, &fs.PathError{Op: opLookup, Path: path, Err: err}
		}
		if !ok {
			return nil, &fs.PathError{Op: opLookup, Path: path, Err: fs.ErrNotExist}
		}
		next, err := img.Inode(num)
		if err != nil {
			return nil, &fs.PathError{Op: opLookup, Path: path, Err: err}
		}
		if i != last && next.IsSymlink() {
			return nil, &fs.PathError{Op: opLookup, Path: path, Err: fmt.Errorf("component %q is a symlink", c)}
		}
		cur = next
	}
	return cur, nil
}
