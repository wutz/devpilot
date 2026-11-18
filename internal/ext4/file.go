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
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
)

const (
	extentMagic    = 0xF30A
	maxExtentDepth = 5
	// maxLogicalBlock bounds holes "to infinity": extents address 32-bit
	// logical blocks.
	maxLogicalBlock = uint64(1) << 32
)

type runKind uint8

const (
	runHole runKind = iota
	runData
	runUnwritten
)

// blockRun describes count logical blocks starting at the requested block:
// a hole, an unwritten extent (both read as zeros) or data at pblk.
type blockRun struct {
	kind  runKind
	pblk  uint64
	count uint64
}

type blockMapper interface {
	mapRun(lblk uint64) (blockRun, error)
}

// ErrEncrypted is returned when the contents of an encrypted inode are accessed.
var ErrEncrypted = errors.New("ext4: inode is encrypted")

func (img *Image) newMapper(ino *Inode) blockMapper {
	if ino.Flags&flagExtents != 0 {
		return &extentMapper{img: img, ino: ino.Number, root: ino.iblock}
	}
	m := &indirectMapper{img: img, ino: ino.Number}
	for i := range m.ptrs {
		m.ptrs[i] = binary.LittleEndian.Uint32(ino.iblock[4*i:])
	}
	return m
}

// --- extents ----------------------------------------------------------------

type extent struct {
	lblk      uint64
	count     uint64
	pblk      uint64
	unwritten bool
}

type extentMapper struct {
	img  *Image
	ino  uint32
	root [60]byte

	// cached leaf covering logical blocks [leafLo, leafHi)
	leaf           []extent
	leafLo, leafHi uint64
	haveLeaf       bool
}

type extentHeader struct {
	entries, max, depth int
}

func parseExtentHeader(b []byte, ino uint32) (extentHeader, error) {
	le := binary.LittleEndian
	if le.Uint16(b[0:]) != extentMagic {
		return extentHeader{}, corruptf("inode %d: bad extent header magic %#x", ino, le.Uint16(b[0:]))
	}
	h := extentHeader{
		entries: int(le.Uint16(b[2:])),
		max:     int(le.Uint16(b[4:])),
		depth:   int(le.Uint16(b[6:])),
	}
	if h.max > (len(b)-12)/12 || h.entries > h.max {
		return h, corruptf("inode %d: bad extent header entries=%d max=%d", ino, h.entries, h.max)
	}
	if h.depth > maxExtentDepth {
		return h, corruptf("inode %d: extent tree depth %d", ino, h.depth)
	}
	return h, nil
}

func (m *extentMapper) mapRun(lblk uint64) (blockRun, error) {
	if lblk >= maxLogicalBlock {
		return blockRun{kind: runHole, count: 1 << 40}, nil
	}
	if !m.haveLeaf || lblk < m.leafLo || lblk >= m.leafHi {
		if err := m.loadLeaf(lblk); err != nil {
			return blockRun{}, err
		}
	}
	// last extent starting at or before lblk
	i, found := slices.BinarySearchFunc(m.leaf, lblk, func(e extent, target uint64) int {
		return cmp.Compare(e.lblk, target)
	})
	if !found {
		i--
	}
	if i >= 0 {
		e := m.leaf[i]
		if lblk < e.lblk+e.count {
			r := blockRun{kind: runData, pblk: e.pblk + (lblk - e.lblk), count: e.lblk + e.count - lblk}
			if e.unwritten {
				r.kind = runUnwritten
			}
			return r, nil
		}
	}
	end := m.leafHi
	if i+1 < len(m.leaf) {
		end = min(end, m.leaf[i+1].lblk)
	}
	return blockRun{kind: runHole, count: end - lblk}, nil
}

func (m *extentMapper) loadLeaf(lblk uint64) error {
	le := binary.LittleEndian
	img := m.img
	node := m.root[:]
	h, err := parseExtentHeader(node, m.ino)
	if err != nil {
		return err
	}
	lo, hi := uint64(0), maxLogicalBlock
	for h.depth > 0 {
		if h.entries == 0 {
			// empty index node: everything below is a hole
			m.leaf, m.leafLo, m.leafHi, m.haveLeaf = nil, lo, hi, true
			return nil
		}
		// Choose the last index whose ei_block <= lblk (the first one when
		// lblk precedes all of them), like ext4_ext_binsearch_idx.
		chosen := 0
		var prev uint64
		for i := 0; i < h.entries; i++ {
			ib := uint64(le.Uint32(node[12+12*i:]))
			if i > 0 && ib <= prev {
				return corruptf("inode %d: extent index not sorted", m.ino)
			}
			prev = ib
			if ib <= lblk {
				chosen = i
			}
		}
		e := node[12+12*chosen:]
		if chosen > 0 {
			lo = max(lo, uint64(le.Uint32(e[0:])))
		}
		if chosen+1 < h.entries {
			hi = min(hi, uint64(le.Uint32(node[12+12*(chosen+1):])))
		}
		child := uint64(le.Uint32(e[4:])) | uint64(le.Uint16(e[8:]))<<32
		if child == 0 || child >= img.sb.blocksCount {
			return corruptf("inode %d: extent index points to block %d", m.ino, child)
		}
		b, err := img.readBlock(child)
		if err != nil {
			return err
		}
		ch, err := parseExtentHeader(b, m.ino)
		if err != nil {
			return err
		}
		if ch.depth != h.depth-1 {
			return corruptf("inode %d: extent node depth %d under depth %d", m.ino, ch.depth, h.depth)
		}
		node, h = b, ch
	}
	leaf := make([]extent, 0, h.entries)
	var prevEnd uint64
	for i := 0; i < h.entries; i++ {
		e := node[12+12*i:]
		x := extent{lblk: uint64(le.Uint32(e[0:]))}
		l := uint64(le.Uint16(e[4:]))
		if l > 32768 {
			x.unwritten = true
			l -= 32768
		}
		if l == 0 {
			return corruptf("inode %d: zero-length extent", m.ino)
		}
		x.count = l
		x.pblk = uint64(le.Uint16(e[6:]))<<32 | uint64(le.Uint32(e[8:]))
		if x.lblk < prevEnd {
			return corruptf("inode %d: overlapping or unsorted extents", m.ino)
		}
		if x.pblk == 0 || x.pblk >= img.sb.blocksCount || x.count > img.sb.blocksCount-x.pblk {
			return corruptf("inode %d: extent [%d,+%d) beyond end of filesystem", m.ino, x.pblk, x.count)
		}
		prevEnd = x.lblk + x.count
		leaf = append(leaf, x)
	}
	m.leaf, m.leafLo, m.leafHi, m.haveLeaf = leaf, lo, hi, true
	return nil
}

// --- indirect block maps ---------------------------------------------------

type indirectMapper struct {
	img  *Image
	ino  uint32
	ptrs [15]uint32
}

func (m *indirectMapper) checkPtr(p uint32) error {
	if uint64(p) >= m.img.sb.blocksCount {
		return corruptf("inode %d: block pointer %d beyond end of filesystem", m.ino, p)
	}
	return nil
}

// scan returns the run starting at ptrs[i] (data if contiguous physical
// blocks, hole if consecutive zero pointers), bounded by maxCount.
func (m *indirectMapper) scan(get func(int) uint32, i, n int, maxCount uint64) (blockRun, error) {
	first := get(i)
	if err := m.checkPtr(first); err != nil {
		return blockRun{}, err
	}
	count := uint64(1)
	for j := i + 1; j < n && count < maxCount; j++ {
		p := get(j)
		if first == 0 {
			if p != 0 {
				break
			}
		} else if uint64(p) != uint64(first)+count {
			break
		}
		count++
	}
	if first == 0 {
		return blockRun{kind: runHole, count: count}, nil
	}
	return blockRun{kind: runData, pblk: uint64(first), count: count}, nil
}

func (m *indirectMapper) mapRun(lblk uint64) (blockRun, error) {
	const maxRun = 1 << 16
	if lblk < 12 {
		return m.scan(func(i int) uint32 { return m.ptrs[i] }, int(lblk), 12, maxRun)
	}
	per := uint64(m.img.blockSize / 4)
	lblk -= 12
	span := per // blocks covered by one pointer slot at the current level
	for level := 1; level <= 3; level++ {
		if lblk < span {
			return m.walk(m.ptrs[11+level], level, lblk, per)
		}
		lblk -= span
		span *= per
	}
	return blockRun{kind: runHole, count: 1 << 40}, nil
}

// walk resolves logical offset off below an indirect block of the given
// level (1 = single indirect).
func (m *indirectMapper) walk(ptr uint32, level int, off, per uint64) (blockRun, error) {
	for {
		sub := uint64(1)
		for i := 1; i < level; i++ {
			sub *= per
		}
		if ptr == 0 {
			// hole covering the remainder of this subtree
			return blockRun{kind: runHole, count: sub*per - off}, nil
		}
		if err := m.checkPtr(ptr); err != nil {
			return blockRun{}, err
		}
		b, err := m.img.readBlock(uint64(ptr))
		if err != nil {
			return blockRun{}, err
		}
		get := func(i int) uint32 { return binary.LittleEndian.Uint32(b[4*i:]) }
		idx := off / sub
		if level == 1 {
			return m.scan(get, int(idx), int(per), 1<<16)
		}
		ptr = get(int(idx))
		off %= sub
		level--
	}
}

// --- file reader -------------------------------------------------------------

// fileReader implements io.ReaderAt over an inode's contents.
type fileReader struct {
	img    *Image
	size   int64
	inline []byte // contents for inline_data inodes

	mu sync.Mutex
	m  blockMapper
}

// Open returns a reader over the contents of a regular file, directory or
// symlink inode, limited to ino.Size. Holes and unwritten extents read as
// zeros.
func (img *Image) Open(ino *Inode) (io.ReaderAt, error) {
	return img.openReader(ino)
}

// OpenSection is like Open but returns an *io.SectionReader, which also
// implements io.Reader and io.Seeker.
func (img *Image) OpenSection(ino *Inode) (*io.SectionReader, error) {
	r, err := img.openReader(ino)
	if err != nil {
		return nil, err
	}
	return io.NewSectionReader(r, 0, ino.Size), nil
}

func (img *Image) openReader(ino *Inode) (*fileReader, error) {
	switch ino.RawMode & sIFMT {
	case sIFREG, sIFDIR, sIFLNK:
	default:
		if ino.Flags&flagEAInode == 0 {
			return nil, fmt.Errorf("ext4: inode %d: cannot open file of mode %#o", ino.Number, ino.RawMode)
		}
	}
	if ino.encrypted() {
		return nil, fmt.Errorf("inode %d: %w", ino.Number, ErrEncrypted)
	}
	f := &fileReader{img: img, size: ino.Size}
	if ino.inline() {
		data, err := img.inlineData(ino)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) < ino.Size {
			return nil, corruptf("inode %d: inline data (%d bytes) shorter than size %d", ino.Number, len(data), ino.Size)
		}
		f.inline = data[:ino.Size]
		return f, nil
	}
	f.m = img.newMapper(ino)
	return f, nil
}

func (f *fileReader) mapRun(lblk uint64) (blockRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m.mapRun(lblk)
}

// ReadAt implements io.ReaderAt.
func (f *fileReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("ext4: negative offset")
	}
	if off >= f.size {
		return 0, io.EOF
	}
	var eof error
	if int64(len(p)) > f.size-off {
		p = p[:f.size-off]
		eof = io.EOF
	}
	if f.inline != nil {
		return copy(p, f.inline[off:]), eof
	}
	bs := f.img.blockSize
	// pending physically contiguous range: p[pendStart:n] <- block pendBlk+pendIn
	pendStart := -1
	var pendBlk uint64
	var pendIn int64
	flush := func(end int) error {
		if pendStart < 0 {
			return nil
		}
		err := f.img.readPhys(p[pendStart:end], pendBlk, pendIn)
		pendStart = -1
		return err
	}
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		lblk := uint64(pos / bs)
		inBlk := pos % bs
		run, err := f.mapRun(lblk)
		if err != nil {
			return 0, err
		}
		if run.count == 0 {
			return 0, corruptf("empty block run")
		}
		need := uint64((inBlk + int64(len(p)-n) + bs - 1) / bs)
		cnt := min(run.count, need)
		chunk := int(min(int64(cnt)*bs-inBlk, int64(len(p)-n)))
		switch run.kind {
		case runData:
			if pendStart >= 0 {
				// contiguous with the pending range?
				pendEnd := pendIn + int64(n-pendStart)
				if pendEnd%bs != 0 || pendBlk+uint64(pendEnd/bs) != run.pblk || inBlk != 0 {
					if err := flush(n); err != nil {
						return 0, err
					}
				}
			}
			if pendStart < 0 {
				pendStart, pendBlk, pendIn = n, run.pblk, inBlk
			}
		default:
			if err := flush(n); err != nil {
				return 0, err
			}
			clear(p[n : n+chunk])
		}
		n += chunk
	}
	if err := flush(n); err != nil {
		return 0, err
	}
	return n, eof
}

// forEachRun calls fn for every block run of the first nblocks logical
// blocks. Used by directory iteration to skip holes cheaply.
func (f *fileReader) forEachRun(nblocks uint64, fn func(lblk uint64, r blockRun) error) error {
	for lblk := uint64(0); lblk < nblocks; {
		r, err := f.mapRun(lblk)
		if err != nil {
			return err
		}
		if r.count == 0 {
			return corruptf("empty block run")
		}
		r.count = min(r.count, nblocks-lblk)
		if err := fn(lblk, r); err != nil {
			return err
		}
		lblk += r.count
	}
	return nil
}

// Readlink returns the target of a symlink inode: fast symlinks (stored in
// i_block), slow symlinks (data blocks) and inline_data symlinks.
func (img *Image) Readlink(ino *Inode) (string, error) {
	if !ino.IsSymlink() {
		return "", fmt.Errorf("ext4: inode %d is not a symlink", ino.Number)
	}
	if ino.encrypted() {
		return "", fmt.Errorf("inode %d: %w", ino.Number, ErrEncrypted)
	}
	if ino.Size > 4096 || ino.Size > img.blockSize {
		return "", corruptf("inode %d: symlink size %d", ino.Number, ino.Size)
	}
	if img.isFastSymlink(ino) {
		return string(ino.iblock[:ino.Size]), nil
	}
	r, err := img.openReader(ino)
	if err != nil {
		return "", err
	}
	buf := make([]byte, ino.Size)
	if _, err := r.ReadAt(buf, 0); err != nil && err != io.EOF {
		return "", err
	}
	return string(buf), nil
}

// isFastSymlink mirrors ext4_inode_is_fast_symlink.
func (img *Image) isFastSymlink(ino *Inode) bool {
	if ino.inline() || ino.Size >= 60 {
		return false
	}
	if ino.Flags&flagEAInode != 0 {
		return ino.Size > 0
	}
	if ino.Flags&flagExtents != 0 {
		return false
	}
	var eaBlocks uint64
	if ino.fileACL != 0 {
		eaBlocks = uint64(img.blockSize / 512)
	}
	return ino.blocks512 <= eaBlocks
}
