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
	"encoding/binary"
	"fmt"
	"io"
)

// jbd2 on-disk constants (all jbd2 structures are big-endian).
const (
	jbd2Magic = 0xC03B3998

	jbd2Descriptor = 1
	jbd2Commit     = 2
	jbd2SuperV1    = 3
	jbd2SuperV2    = 4
	jbd2Revoke     = 5

	jbd2IncompatRevoke      = 0x1
	jbd2Incompat64Bit       = 0x2
	jbd2IncompatAsyncCommit = 0x4
	jbd2IncompatCsumV2      = 0x8
	jbd2IncompatCsumV3      = 0x10
	jbd2IncompatFastCommit  = 0x20
	jbd2KnownIncompat       = jbd2IncompatRevoke | jbd2Incompat64Bit | jbd2IncompatAsyncCommit |
		jbd2IncompatCsumV2 | jbd2IncompatCsumV3 | jbd2IncompatFastCommit

	jbd2FlagEscape   = 0x1
	jbd2FlagSameUUID = 0x2
	jbd2FlagLastTag  = 0x8
)

// overlayBlock locates the replayed contents of a filesystem block: the
// physical (fs) block number of the journal block that holds it.
type overlayBlock struct {
	pblk   uint64
	escape bool
}

func (img *Image) readOverlay(ob overlayBlock) ([]byte, error) {
	b := make([]byte, img.blockSize)
	if err := img.readRaw(b, int64(ob.pblk)*img.blockSize); err != nil {
		return nil, fmt.Errorf("ext4: journal block %d: %w", ob.pblk, err)
	}
	if ob.escape {
		binary.BigEndian.PutUint32(b, jbd2Magic)
	}
	return b, nil
}

type journalSuper struct {
	blockSize uint32
	maxLen    uint32
	first     uint32
	sequence  uint32
	start     uint32
	incompat  uint32
	uuid      [16]byte
	numFC     uint32
}

func parseJournalSuper(b []byte) (journalSuper, error) {
	be := binary.BigEndian
	var js journalSuper
	if len(b) < 1024 {
		return js, corruptf("short journal superblock")
	}
	if be.Uint32(b[0:]) != jbd2Magic {
		return js, corruptf("bad journal superblock magic %#x", be.Uint32(b[0:]))
	}
	btype := be.Uint32(b[4:])
	if btype != jbd2SuperV1 && btype != jbd2SuperV2 {
		return js, corruptf("bad journal superblock type %d", btype)
	}
	js.blockSize = be.Uint32(b[0xC:])
	js.maxLen = be.Uint32(b[0x10:])
	js.first = be.Uint32(b[0x14:])
	js.sequence = be.Uint32(b[0x18:])
	js.start = be.Uint32(b[0x1C:])
	if btype == jbd2SuperV2 {
		js.incompat = be.Uint32(b[0x28:])
		copy(js.uuid[:], b[0x30:0x40])
		js.numFC = be.Uint32(b[0x54:])
	}
	if unknown := js.incompat &^ jbd2KnownIncompat; unknown != 0 {
		return js, unsupportedf("journal incompatible features %#x", unknown)
	}
	return js, nil
}

func (js *journalSuper) csum() bool {
	return js.incompat&(jbd2IncompatCsumV2|jbd2IncompatCsumV3) != 0
}

func (js *journalSuper) tagBytes() int {
	if js.incompat&jbd2IncompatCsumV3 != 0 {
		return 16
	}
	sz := 12
	if js.incompat&jbd2IncompatCsumV2 != 0 {
		sz += 2
	}
	if js.incompat&jbd2Incompat64Bit != 0 {
		return sz
	}
	return sz - 4
}

// journalSource reads journal blocks by logical journal block number.
type journalSource interface {
	read(i uint64) ([]byte, error)
}

type journalTag struct {
	fsblk  uint64
	jblk   uint64 // logical journal block holding the data
	escape bool
}

type journalTxn struct {
	seq  uint32
	tags []journalTag
}

type replayPlan struct {
	txns    int
	revoked int                   // tags skipped because of revoke records
	blocks  map[uint64]journalTag // fs block -> latest committed, non-revoked copy
}

func tidGT(x, y uint32) bool { return int32(x-y) > 0 }

type replayScanner struct {
	src      journalSource
	bs       int
	first    uint64
	last     uint64
	logLen   uint64
	seed     uint32
	csum     bool
	is64     bool
	is3      bool
	tagBytes int
	tailSize int

	committed  []journalTxn
	revoked    map[uint64]uint32
	seq        uint32
	cur        journalTxn
	curRevokes []uint64
}

func newReplayScanner(js journalSuper, src journalSource) *replayScanner {
	csum := js.csum()
	tailSize := 0
	if csum {
		tailSize = 4
	}
	return &replayScanner{
		src:      src,
		bs:       int(js.blockSize),
		first:    uint64(js.first),
		last:     uint64(js.maxLen),
		logLen:   uint64(js.maxLen) - uint64(js.first),
		seed:     crc32cRaw(^uint32(0), js.uuid[:]),
		csum:     csum,
		is64:     js.incompat&jbd2Incompat64Bit != 0,
		is3:      js.incompat&jbd2IncompatCsumV3 != 0,
		tagBytes: js.tagBytes(),
		tailSize: tailSize,
		revoked:  map[uint64]uint32{},
		seq:      js.sequence,
		cur:      journalTxn{seq: js.sequence},
	}
}

func (s *replayScanner) wrap(i uint64) uint64 {
	if i >= s.last {
		i -= s.logLen
	}
	return i
}

func (s *replayScanner) verifyTail(b []byte) bool {
	tmp := append([]byte(nil), b...)
	want := binary.BigEndian.Uint32(tmp[s.bs-4:])
	binary.BigEndian.PutUint32(tmp[s.bs-4:], 0)
	return crc32cRaw(s.seed, tmp) == want
}

func (s *replayScanner) verifyCommit(b []byte) bool {
	tmp := append([]byte(nil), b...)
	want := binary.BigEndian.Uint32(tmp[16:])
	binary.BigEndian.PutUint32(tmp[16:], 0)
	return crc32cRaw(s.seed, tmp) == want
}

func (s *replayScanner) verifyData(b []byte, seq, want uint32) bool {
	var sb [4]byte
	binary.BigEndian.PutUint32(sb[:], seq)
	c := crc32cRaw(crc32cRaw(s.seed, sb[:]), b)
	if s.is3 {
		return c == want
	}
	return c&0xFFFF == want
}

func (s *replayScanner) decodeTag(raw []byte) (fsblk uint64, flags, tsum uint32) {
	be := binary.BigEndian
	fsblk = uint64(be.Uint32(raw[0:]))
	if s.is3 {
		flags = be.Uint32(raw[4:])
		tsum = be.Uint32(raw[12:])
	} else {
		tsum = uint32(be.Uint16(raw[4:]))
		flags = uint32(be.Uint16(raw[6:]))
	}
	if s.is64 {
		fsblk |= uint64(be.Uint32(raw[8:])) << 32
	}
	return fsblk, flags, tsum
}

func (s *replayScanner) scanDescriptor(b []byte, next, visited uint64) (advance uint64, ok bool, err error) {
	if s.csum && !s.verifyTail(b) {
		return 0, false, nil
	}
	advance = 1
	var tags []journalTag
	for off := 12; off+s.tagBytes <= s.bs-s.tailSize; {
		fsblk, flags, tsum := s.decodeTag(b[off:])
		jblk := s.wrap(next + advance)
		advance++
		if visited+advance > s.logLen {
			return 0, false, nil // descriptor claims more blocks than the log holds
		}
		if s.csum {
			data, err := s.src.read(jblk)
			if err != nil {
				return 0, false, err
			}
			if len(data) != s.bs || !s.verifyData(data, s.seq, tsum) {
				return 0, false, nil
			}
		}
		tags = append(tags, journalTag{fsblk: fsblk, jblk: jblk, escape: flags&jbd2FlagEscape != 0})
		off += s.tagBytes
		if flags&jbd2FlagSameUUID == 0 {
			off += 16
		}
		if flags&jbd2FlagLastTag != 0 {
			break
		}
	}
	s.cur.tags = append(s.cur.tags, tags...)
	return advance, true, nil
}

func (s *replayScanner) handleCommit(b []byte) bool {
	if s.csum && !s.verifyCommit(b) {
		return false
	}
	s.committed = append(s.committed, s.cur)
	for _, r := range s.curRevokes {
		if old, ok := s.revoked[r]; !ok || tidGT(s.seq, old) {
			s.revoked[r] = s.seq
		}
	}
	s.curRevokes = nil
	s.seq++
	s.cur = journalTxn{seq: s.seq}
	return true
}

func (s *replayScanner) handleRevoke(b []byte) bool {
	if s.csum && !s.verifyTail(b) {
		return false
	}
	be := binary.BigEndian
	rcount := int(be.Uint32(b[12:]))
	if rcount < 16 || rcount > s.bs-s.tailSize {
		return false
	}
	rec := 4
	if s.is64 {
		rec = 8
	}
	for off := 16; off+rec <= rcount; off += rec {
		if s.is64 {
			s.curRevokes = append(s.curRevokes, be.Uint64(b[off:]))
		} else {
			s.curRevokes = append(s.curRevokes, uint64(be.Uint32(b[off:])))
		}
	}
	return true
}

// planReplay implements the jbd2 recovery passes (scan, revoke, replay)
// without writing anything: it returns which journal block holds the
// latest committed contents of each filesystem block.
//
// The scan stops at the first block whose magic or sequence number does not
// match, at an unknown block type, or at a descriptor/revoke/commit block or
// tagged data block whose checksum (journal csum v2/v3) does not verify; the
// transaction in progress at that point is not replayed. The kernel is more
// lenient with bad data-block checksums inside committed transactions (it
// skips just those blocks); stopping instead keeps a torn snapshot
// consistent with some earlier point in time.
func planReplay(js journalSuper, src journalSource) (replayPlan, error) {
	plan := replayPlan{blocks: map[uint64]journalTag{}}
	if js.start == 0 {
		return plan, nil // clean journal
	}
	if js.incompat&jbd2IncompatFastCommit != 0 {
		return plan, unsupportedf("journal fast commit replay")
	}
	if js.first == 0 || js.first >= js.maxLen || js.start < js.first || js.start >= js.maxLen {
		return plan, corruptf("journal geometry first=%d start=%d maxlen=%d", js.first, js.start, js.maxLen)
	}

	s := newReplayScanner(js, src)
	be := binary.BigEndian
	next := uint64(js.start)

scan:
	for visited := uint64(0); visited < s.logLen; {
		b, err := src.read(next)
		if err != nil {
			return plan, err
		}
		if len(b) != s.bs || be.Uint32(b[0:]) != jbd2Magic || be.Uint32(b[8:]) != s.seq {
			break
		}
		advance := uint64(1)
		switch be.Uint32(b[4:]) {
		case jbd2Descriptor:
			var ok bool
			advance, ok, err = s.scanDescriptor(b, next, visited)
			if err != nil {
				return plan, err
			}
			if !ok {
				break scan
			}
		case jbd2Commit:
			if !s.handleCommit(b) {
				break scan
			}
		case jbd2Revoke:
			if !s.handleRevoke(b) {
				break scan
			}
		default:
			break scan
		}
		visited += advance
		next = s.wrap(next + advance)
	}

	for _, t := range s.committed {
		for _, tag := range t.tags {
			if rs, ok := s.revoked[tag.fsblk]; ok && !tidGT(t.seq, rs) {
				plan.revoked++
				continue
			}
			plan.blocks[tag.fsblk] = tag
		}
	}
	plan.txns = len(s.committed)
	return plan, nil
}

// inodeJournalSource reads the internal journal through the journal inode's
// block map (resolved on the raw, un-replayed image).
type inodeJournalSource struct {
	f  *fileReader
	bs int64
}

func (s *inodeJournalSource) read(i uint64) ([]byte, error) {
	b := make([]byte, s.bs)
	if _, err := s.f.ReadAt(b, int64(i)*s.bs); err != nil && err != io.EOF {
		return nil, err
	}
	return b, nil
}

func (s *inodeJournalSource) phys(i uint64) (uint64, error) {
	r, err := s.f.mapRun(i)
	if err != nil {
		return 0, err
	}
	if r.kind != runData {
		return 0, corruptf("journal block %d is not allocated", i)
	}
	return r.pblk, nil
}

// replayJournal computes the in-memory overlay for a filesystem that needs
// recovery.
func (img *Image) replayJournal() error {
	jino, err := img.Inode(img.sb.journalInum)
	if err != nil {
		return fmt.Errorf("journal inode: %w", err)
	}
	if !jino.IsRegular() || jino.inline() {
		return corruptf("journal inode %d is not a regular file", jino.Number)
	}
	f, err := img.openReader(jino)
	if err != nil {
		return err
	}
	src := &inodeJournalSource{f: f, bs: img.blockSize}
	if jino.Size < img.blockSize {
		return corruptf("journal inode too small (%d bytes)", jino.Size)
	}
	b, err := src.read(0)
	if err != nil {
		return err
	}
	js, err := parseJournalSuper(b)
	if err != nil {
		return err
	}
	if int64(js.blockSize) != img.blockSize {
		return corruptf("journal block size %d != filesystem block size %d", js.blockSize, img.blockSize)
	}
	// Bounding the log by the image size also bounds the scan's work and
	// memory when a hostile block map repeats the same journal block.
	if uint64(js.maxLen) > uint64(jino.Size/img.blockSize) || int64(js.maxLen) > img.size/img.blockSize {
		return corruptf("journal length %d exceeds journal inode (%d bytes) or image", js.maxLen, jino.Size)
	}
	plan, err := planReplay(js, src)
	if err != nil {
		return err
	}
	overlay := make(map[uint64]overlayBlock, len(plan.blocks))
	for fsblk, tag := range plan.blocks {
		if fsblk >= img.sb.blocksCount {
			return corruptf("journal tag for block %d beyond end of filesystem", fsblk)
		}
		p, err := src.phys(tag.jblk)
		if err != nil {
			return err
		}
		overlay[fsblk] = overlayBlock{pblk: p, escape: tag.escape}
	}
	img.overlay = overlay
	img.replayed = plan.txns
	return nil
}
