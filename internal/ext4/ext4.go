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

// Package ext4 is a pure-Go, read-only reader for ext4 filesystem images.
//
// It is meant for unprivileged consumers (no loop devices, no mount) that
// need to stream files, directories, metadata and extended attributes out of
// an ext4 image file. The image is never written to: when the filesystem
// needs journal recovery (for example a snapshot of a mounted or crashed
// filesystem) the jbd2 journal is replayed in memory only.
//
// Everything read from the image is validated, so a corrupted or hostile
// image results in errors rather than panics, hangs or unbounded memory use.
//
// Supported: 1–64 KiB blocks, 32/64-byte group descriptors (64bit), meta_bg,
// flex_bg, sparse_super/sparse_super2, extents (any depth, unwritten
// extents), legacy direct/indirect/double/triple block maps, linear and
// htree (dir_index, largedir) directories, inline_data files, directories
// and symlinks, fast and slow symlinks, in-inode and external-block xattrs,
// ea_inode values, nanosecond/extended-epoch timestamps, huge_file, mmp,
// csum_seed and casefold (names are returned as raw bytes). metadata_csum
// checksums are not verified (only the superblock checksum, reported via
// Warnings). Journal recovery (needs_recovery) is done in memory for
// internal jbd2 journals (v1/v2 superblocks, 32/64-bit tags, csum v2/v3,
// revoke records, escaped blocks, wrap-around).
//
// Refused: the compression, journal_dev and dirdata incompat features and
// unknown incompat bits; recovery from an external journal or a journal
// with fast commits; reading contents of encrypted inodes.
package ext4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/bits"
	"os"
)

// ErrCorrupt is wrapped by all errors caused by inconsistent on-disk data.
var ErrCorrupt = errors.New("ext4: corrupt filesystem")

// ErrUnsupported is wrapped by errors about features this reader cannot handle.
var ErrUnsupported = errors.New("ext4: unsupported feature")

func corruptf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, args...)...)
}

func unsupportedf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrUnsupported}, args...)...)
}

const (
	superblockOffset = 1024
	superblockSize   = 1024
	superMagic       = 0xEF53
	rootIno          = 2
	goodOldInodeSize = 128
	maxBlockLog      = 6 // 64 KiB blocks
)

// Superblock feature flags.
const (
	compatHasJournal   = 0x4
	compatSparseSuper2 = 0x200

	incompatCompression = 0x1
	incompatFiletype    = 0x2
	incompatRecover     = 0x4
	incompatJournalDev  = 0x8
	incompatMetaBG      = 0x10
	incompatExtents     = 0x40
	incompat64Bit       = 0x80
	incompatMMP         = 0x100
	incompatFlexBG      = 0x200
	incompatEAInode     = 0x400
	incompatDirData     = 0x1000
	incompatCsumSeed    = 0x2000
	incompatLargeDir    = 0x4000
	incompatInlineData  = 0x8000
	incompatEncrypt     = 0x10000
	incompatCasefold    = 0x20000

	roCompatSparseSuper  = 0x1
	roCompatHugeFile     = 0x8
	roCompatMetadataCsum = 0x400

	supportedIncompat = incompatFiletype | incompatRecover | incompatMetaBG | incompatExtents |
		incompat64Bit | incompatMMP | incompatFlexBG | incompatEAInode | incompatCsumSeed |
		incompatLargeDir | incompatInlineData | incompatEncrypt | incompatCasefold
)

var incompatNames = map[uint32]string{
	incompatCompression: "compression",
	incompatJournalDev:  "journal_dev",
	incompatDirData:     "dirdata",
}

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// crc32cRaw is the Linux kernel's crc32c(): no pre/post inversion.
func crc32cRaw(crc uint32, p []byte) uint32 {
	return ^crc32.Update(^crc, crc32cTable, p)
}

type superblock struct {
	inodesCount    uint32
	blocksCount    uint64
	firstDataBlock uint32
	logBlockSize   uint32
	blocksPerGroup uint32
	inodesPerGroup uint32
	revLevel       uint32
	inodeSize      uint32
	compat         uint32
	incompat       uint32
	roCompat       uint32
	journalInum    uint32
	journalDev     uint32
	descSize       uint32
	firstMetaBG    uint32
	backupBGs      [2]uint32
	checksum       uint32
}

func parseSuperblock(b []byte) (superblock, error) {
	le := binary.LittleEndian
	var sb superblock
	if len(b) < superblockSize {
		return sb, corruptf("short superblock")
	}
	if le.Uint16(b[0x38:]) != superMagic {
		return sb, fmt.Errorf("ext4: bad superblock magic %#x (not an ext2/3/4 filesystem)", le.Uint16(b[0x38:]))
	}
	sb.inodesCount = le.Uint32(b[0x0:])
	sb.blocksCount = uint64(le.Uint32(b[0x4:]))
	sb.firstDataBlock = le.Uint32(b[0x14:])
	sb.logBlockSize = le.Uint32(b[0x18:])
	sb.blocksPerGroup = le.Uint32(b[0x20:])
	sb.inodesPerGroup = le.Uint32(b[0x28:])
	sb.revLevel = le.Uint32(b[0x4C:])
	sb.inodeSize = goodOldInodeSize
	if sb.revLevel > 0 {
		sb.inodeSize = uint32(le.Uint16(b[0x58:]))
		sb.compat = le.Uint32(b[0x5C:])
		sb.incompat = le.Uint32(b[0x60:])
		sb.roCompat = le.Uint32(b[0x64:])
	}
	sb.journalInum = le.Uint32(b[0xE0:])
	sb.journalDev = le.Uint32(b[0xE4:])
	sb.descSize = 32
	if sb.incompat&incompat64Bit != 0 {
		sb.blocksCount |= uint64(le.Uint32(b[0x150:])) << 32
		sb.descSize = uint32(le.Uint16(b[0xFE:]))
	}
	sb.firstMetaBG = le.Uint32(b[0x104:])
	sb.backupBGs[0] = le.Uint32(b[0x24C:])
	sb.backupBGs[1] = le.Uint32(b[0x250:])
	sb.checksum = le.Uint32(b[0x3FC:])
	return sb, nil
}

// Image is an opened, read-only ext4 filesystem image. It is safe for
// concurrent use.
type Image struct {
	r      io.ReaderAt
	size   int64
	closer io.Closer

	sb           superblock
	blockSize    int64
	groups       uint64
	descPerBlock uint64

	// journal replay overlay: fs block -> journal block holding its
	// latest committed contents. Immutable after Open.
	overlay       map[uint64]overlayBlock
	replayed      int
	needsRecovery bool
	warnings      []string

	cache *blockCache
}

// Options tune Open. The zero value is the default.
type Options struct {
	// SkipJournalReplay opens the image as-is even when the journal needs
	// recovery. Only useful for debugging and tests.
	SkipJournalReplay bool
	// CacheBytes is the size of the metadata block cache (default 8 MiB).
	CacheBytes int
}

// Open parses the superblock and group descriptors of the ext4 image read
// from r (size bytes long). When the journal needs recovery the jbd2 journal
// is replayed in memory; nothing is ever written to r.
func Open(r io.ReaderAt, size int64) (*Image, error) {
	return OpenWithOptions(r, size, Options{})
}

// OpenFile opens the ext4 image stored in the file at path. Image.Close
// closes the file.
func OpenFile(path string) (*Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	img, err := Open(f, st.Size())
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	img.closer = f
	return img, nil
}

// OpenWithOptions is Open with tunables.
func OpenWithOptions(r io.ReaderAt, size int64, opts Options) (*Image, error) {
	if size < superblockOffset+superblockSize {
		return nil, corruptf("image too small (%d bytes)", size)
	}
	img := &Image{r: r, size: size}
	raw := make([]byte, superblockSize)
	if err := img.readRaw(raw, superblockOffset); err != nil {
		return nil, err
	}
	sb, err := parseSuperblock(raw)
	if err != nil {
		return nil, err
	}
	if err := img.setSuperblock(sb); err != nil {
		return nil, err
	}
	img.needsRecovery = sb.incompat&incompatRecover != 0
	if sb.roCompat&roCompatMetadataCsum != 0 {
		if crc32cRaw(^uint32(0), raw[:0x3FC]) != sb.checksum {
			img.warnings = append(img.warnings, "superblock checksum mismatch")
		}
	}
	cacheBytes := opts.CacheBytes
	if cacheBytes <= 0 {
		cacheBytes = 8 << 20
	}
	nblocks := max(cacheBytes/int(img.blockSize), 16)
	img.cache = newBlockCache(nblocks)

	if sb.incompat&incompatRecover != 0 && !opts.SkipJournalReplay {
		if err := img.replayJournal(); err != nil {
			return nil, fmt.Errorf("ext4: journal replay: %w", err)
		}
		if len(img.overlay) > 0 {
			img.cache.reset()
			// The superblock itself is journaled; use the replayed copy.
			sbBlk := uint64(superblockOffset / img.blockSize)
			blk, err := img.readBlock(sbBlk)
			if err != nil {
				return nil, err
			}
			off := superblockOffset % img.blockSize
			nsb, err := parseSuperblock(blk[off : off+superblockSize])
			if err != nil {
				return nil, fmt.Errorf("ext4: replayed superblock: %w", err)
			}
			if err := img.setSuperblock(nsb); err != nil {
				return nil, fmt.Errorf("ext4: replayed superblock: %w", err)
			}
		}
	}
	if _, err := img.Inode(rootIno); err != nil {
		return nil, fmt.Errorf("ext4: root inode: %w", err)
	}
	return img, nil
}

func (img *Image) setSuperblock(sb superblock) error {
	if sb.logBlockSize > maxBlockLog {
		return corruptf("block size log %d too large", sb.logBlockSize)
	}
	bs := int64(1024) << sb.logBlockSize
	if unknown := sb.incompat &^ supportedIncompat; unknown != 0 {
		var names []string
		for bit := uint32(1); bit != 0; bit <<= 1 {
			if unknown&bit != 0 {
				if n, ok := incompatNames[bit]; ok {
					names = append(names, n)
				} else {
					names = append(names, fmt.Sprintf("%#x", bit))
				}
			}
		}
		return unsupportedf("incompatible features %v", names)
	}
	if sb.blocksPerGroup < 8 || int64(sb.blocksPerGroup) > 8*bs {
		return corruptf("blocks per group %d", sb.blocksPerGroup)
	}
	if sb.inodesPerGroup == 0 || int64(sb.inodesPerGroup) > 8*bs {
		return corruptf("inodes per group %d", sb.inodesPerGroup)
	}
	if sb.inodeSize < goodOldInodeSize || int64(sb.inodeSize) > bs || bits.OnesCount32(sb.inodeSize) != 1 {
		return corruptf("inode size %d", sb.inodeSize)
	}
	if sb.blocksCount <= uint64(sb.firstDataBlock) || sb.blocksCount > (1<<62)/uint64(bs) {
		return corruptf("blocks count %d", sb.blocksCount)
	}
	if sb.firstDataBlock > 1 {
		return corruptf("first data block %d", sb.firstDataBlock)
	}
	if sb.incompat&incompat64Bit != 0 {
		if sb.descSize < 64 || sb.descSize > 1024 || bits.OnesCount32(sb.descSize) != 1 {
			return corruptf("group descriptor size %d", sb.descSize)
		}
	}
	groups := (sb.blocksCount - uint64(sb.firstDataBlock) + uint64(sb.blocksPerGroup) - 1) / uint64(sb.blocksPerGroup)
	if sb.inodesCount == 0 || uint64(sb.inodesCount) > groups*uint64(sb.inodesPerGroup) {
		return corruptf("inodes count %d for %d groups", sb.inodesCount, groups)
	}
	if sb.inodesCount < rootIno {
		return corruptf("inodes count %d", sb.inodesCount)
	}
	if sb.incompat&incompatRecover != 0 {
		if sb.compat&compatHasJournal == 0 {
			return corruptf("filesystem needs recovery but has no journal")
		}
		if sb.journalInum == 0 {
			return unsupportedf("needs recovery from an external journal device")
		}
	}
	img.sb = sb
	img.blockSize = bs
	img.groups = groups
	img.descPerBlock = uint64(bs) / uint64(sb.descSize)
	return nil
}

// Close releases the underlying file when the image was opened with OpenFile.
func (img *Image) Close() error {
	if img.closer != nil {
		return img.closer.Close()
	}
	return nil
}

// Replayed returns the number of journal transactions replayed in memory
// (0 for a cleanly unmounted filesystem).
func (img *Image) Replayed() int { return img.replayed }

// NeedsRecovery reports whether the on-disk superblock had the
// needs_recovery (INCOMPAT_RECOVER) flag set, i.e. the image is a snapshot
// of a mounted or crashed filesystem.
func (img *Image) NeedsRecovery() bool { return img.needsRecovery }

// Warnings returns non-fatal problems noticed while opening the image (for
// example checksum mismatches, which are never treated as errors).
func (img *Image) Warnings() []string { return append([]string(nil), img.warnings...) }

// BlockSize returns the filesystem block size in bytes.
func (img *Image) BlockSize() int { return int(img.blockSize) }

func (img *Image) has64Bit() bool { return img.sb.incompat&incompat64Bit != 0 }

// readRaw reads exactly len(p) bytes at byte offset off of the image,
// bypassing the journal overlay.
func (img *Image) readRaw(p []byte, off int64) error {
	if off < 0 || off > img.size || int64(len(p)) > img.size-off {
		return corruptf("read [%d,+%d) beyond end of image (%d bytes)", off, len(p), img.size)
	}
	n, err := img.r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("ext4: read at %d: %w", off, err)
}

// readPhys reads len(p) bytes starting at fs block blk, byte offset inBlk
// within it, honoring the journal overlay. Contiguous blocks are read with
// as few ReadAt calls as possible.
func (img *Image) readPhys(p []byte, blk uint64, inBlk int64) error {
	bs := img.blockSize
	nblk := (inBlk + int64(len(p)) + bs - 1) / bs
	if blk >= img.sb.blocksCount || uint64(nblk) > img.sb.blocksCount-blk {
		return corruptf("block range %d+%d beyond end of filesystem (%d blocks)", blk, nblk, img.sb.blocksCount)
	}
	if len(img.overlay) == 0 {
		return img.readRaw(p, int64(blk)*bs+inBlk)
	}
	// Split around journal overlay blocks.
	done := 0
	pendStart := -1 // start index in p of pending raw range
	var pendOff int64
	flush := func(end int) error {
		if pendStart < 0 {
			return nil
		}
		err := img.readRaw(p[pendStart:end], pendOff)
		pendStart = -1
		return err
	}
	for done < len(p) {
		cur := blk + uint64((inBlk+int64(done))/bs)
		within := (inBlk + int64(done)) % bs
		n := int(min(bs-within, int64(len(p)-done)))
		if ob, ok := img.overlay[cur]; ok {
			if err := flush(done); err != nil {
				return err
			}
			data, err := img.readOverlay(ob)
			if err != nil {
				return err
			}
			copy(p[done:done+n], data[within:])
		} else if pendStart < 0 {
			pendStart = done
			pendOff = int64(cur)*bs + within
		}
		done += n
	}
	return flush(len(p))
}

// readBlock returns the (possibly cached) contents of fs block blk. The
// returned slice must not be modified.
func (img *Image) readBlock(blk uint64) ([]byte, error) {
	if b := img.cache.get(blk); b != nil {
		return b, nil
	}
	b := make([]byte, img.blockSize)
	if err := img.readPhys(b, blk, 0); err != nil {
		return nil, err
	}
	img.cache.put(blk, b)
	return b, nil
}

func (img *Image) groupFirstBlock(g uint64) uint64 {
	return uint64(img.sb.firstDataBlock) + g*uint64(img.sb.blocksPerGroup)
}

func isPowerOf(n, base uint64) bool {
	for n > 1 && n%base == 0 {
		n /= base
	}
	return n == 1
}

// groupHasSuper mirrors ext4_bg_has_super.
func (img *Image) groupHasSuper(g uint64) bool {
	if g == 0 {
		return true
	}
	if img.sb.compat&compatSparseSuper2 != 0 {
		return g == uint64(img.sb.backupBGs[0]) || g == uint64(img.sb.backupBGs[1])
	}
	if g <= 1 || img.sb.roCompat&roCompatSparseSuper == 0 {
		return true
	}
	if g&1 == 0 {
		return false
	}
	return isPowerOf(g, 3) || isPowerOf(g, 5) || isPowerOf(g, 7)
}

// descriptorBlock mirrors the kernel's descriptor_loc: the location of the
// nr-th group descriptor block, honoring meta_bg.
func (img *Image) descriptorBlock(nr uint64) uint64 {
	sbBlock := uint64(superblockOffset / img.blockSize)
	if img.sb.incompat&incompatMetaBG == 0 || nr < uint64(img.sb.firstMetaBG) {
		return sbBlock + nr + 1
	}
	bg := img.descPerBlock * nr
	var hasSuper uint64
	if img.groupHasSuper(bg) {
		hasSuper = 1
	}
	if img.blockSize == 1024 && nr == 0 && img.sb.firstDataBlock == 0 {
		hasSuper++
	}
	return img.groupFirstBlock(bg) + hasSuper
}

// inodeTable returns the first block of the inode table of group g.
func (img *Image) inodeTable(g uint64) (uint64, error) {
	if g >= img.groups {
		return 0, corruptf("group %d out of range", g)
	}
	blk := img.descriptorBlock(g / img.descPerBlock)
	b, err := img.readBlock(blk)
	if err != nil {
		return 0, fmt.Errorf("ext4: group descriptor %d: %w", g, err)
	}
	off := (g % img.descPerBlock) * uint64(img.sb.descSize)
	d := b[off : off+uint64(img.sb.descSize)]
	table := uint64(binary.LittleEndian.Uint32(d[0x8:]))
	if img.has64Bit() && len(d) >= 0x2C {
		table |= uint64(binary.LittleEndian.Uint32(d[0x28:])) << 32
	}
	tableBlocks := (uint64(img.sb.inodesPerGroup)*uint64(img.sb.inodeSize) + uint64(img.blockSize) - 1) / uint64(img.blockSize)
	if table == 0 || table >= img.sb.blocksCount || tableBlocks > img.sb.blocksCount-table {
		return 0, corruptf("group %d inode table at block %d out of range", g, table)
	}
	return table, nil
}
