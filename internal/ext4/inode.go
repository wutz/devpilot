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
	"io/fs"
	"time"
)

// i_mode file type bits.
const (
	sIFMT   = 0xF000
	sIFSOCK = 0xC000
	sIFLNK  = 0xA000
	sIFREG  = 0x8000
	sIFBLK  = 0x6000
	sIFDIR  = 0x4000
	sIFCHR  = 0x2000
	sIFIFO  = 0x1000
	sISUID  = 0x800
	sISGID  = 0x400
	sISVTX  = 0x200
)

// Inode flags (i_flags).
const (
	flagEncrypt    = 0x800
	flagIndex      = 0x1000
	flagHugeFile   = 0x40000
	flagExtents    = 0x80000
	flagEAInode    = 0x200000
	flagInlineData = 0x10000000
)

// Inode is a decoded ext4 inode.
type Inode struct {
	Number              uint32
	RawMode             uint16 // i_mode as on disk (S_IFMT bits + perms + suid/sgid/sticky)
	UID, GID            uint32
	Size                int64
	Links               uint32
	Atime, Mtime, Ctime time.Time
	Flags               uint32
	// DevMajor and DevMinor are set for character and block devices.
	DevMajor, DevMinor uint32

	iblock     [60]byte
	fileACL    uint64
	blocks512  uint64 // i_blocks in 512-byte units
	extraIsize uint32
	ibody      []byte // in-inode extended attribute area (after i_extra_isize), nil if none
}

func decodeTime(base uint32, extra uint32, hasExtra bool) time.Time {
	sec := int64(int32(base))
	var nsec int64
	if hasExtra {
		sec += int64(extra&3) << 32
		nsec = int64(extra >> 2)
	}
	return time.Unix(sec, nsec)
}

// Inode reads inode number num.
func (img *Image) Inode(num uint32) (*Inode, error) {
	if num == 0 || num > img.sb.inodesCount {
		return nil, corruptf("inode number %d out of range (1..%d)", num, img.sb.inodesCount)
	}
	ipg := uint64(img.sb.inodesPerGroup)
	g := uint64(num-1) / ipg
	idx := uint64(num-1) % ipg
	table, err := img.inodeTable(g)
	if err != nil {
		return nil, err
	}
	isz := uint64(img.sb.inodeSize)
	off := idx * isz
	bs := uint64(img.blockSize)
	b, err := img.readBlock(table + off/bs)
	if err != nil {
		return nil, fmt.Errorf("ext4: inode %d: %w", num, err)
	}
	raw := b[off%bs : off%bs+isz]
	return img.parseInode(num, raw)
}

func (img *Image) parseInode(num uint32, raw []byte) (*Inode, error) {
	le := binary.LittleEndian
	ino := &Inode{Number: num}
	ino.RawMode = le.Uint16(raw[0x0:])
	ino.UID = uint32(le.Uint16(raw[0x2:])) | uint32(le.Uint16(raw[0x78:]))<<16
	ino.GID = uint32(le.Uint16(raw[0x18:])) | uint32(le.Uint16(raw[0x7A:]))<<16
	size := uint64(le.Uint32(raw[0x4:])) | uint64(le.Uint32(raw[0x6C:]))<<32
	if size > 1<<62 {
		return nil, corruptf("inode %d: size %d", num, size)
	}
	ino.Size = int64(size)
	ino.Links = uint32(le.Uint16(raw[0x1A:]))
	ino.Flags = le.Uint32(raw[0x20:])
	copy(ino.iblock[:], raw[0x28:0x64])
	ino.fileACL = uint64(le.Uint32(raw[0x68:]))
	if img.has64Bit() {
		ino.fileACL |= uint64(le.Uint16(raw[0x76:])) << 32
	}
	blocks := uint64(le.Uint32(raw[0x1C:]))
	if img.sb.roCompat&roCompatHugeFile != 0 {
		blocks |= uint64(le.Uint16(raw[0x74:])) << 32
		if ino.Flags&flagHugeFile != 0 {
			blocks *= uint64(img.blockSize / 512)
		}
	}
	ino.blocks512 = blocks

	if len(raw) > goodOldInodeSize {
		ino.extraIsize = uint32(le.Uint16(raw[0x80:]))
		if ino.extraIsize&3 != 0 || goodOldInodeSize+int(ino.extraIsize) > len(raw) {
			return nil, corruptf("inode %d: bad i_extra_isize %d", num, ino.extraIsize)
		}
		start := goodOldInodeSize + int(ino.extraIsize)
		if len(raw)-start >= 4 {
			ino.ibody = append([]byte(nil), raw[start:]...)
		}
	}
	fits := func(off int) bool { return goodOldInodeSize+int(ino.extraIsize) >= off+4 }
	extra := func(off int) (uint32, bool) {
		if fits(off) {
			return le.Uint32(raw[off:]), true
		}
		return 0, false
	}
	e, ok := extra(0x8C)
	ino.Atime = decodeTime(le.Uint32(raw[0x8:]), e, ok)
	e, ok = extra(0x84)
	ino.Ctime = decodeTime(le.Uint32(raw[0xC:]), e, ok)
	e, ok = extra(0x88)
	ino.Mtime = decodeTime(le.Uint32(raw[0x10:]), e, ok)

	switch ino.RawMode & sIFMT {
	case sIFCHR, sIFBLK:
		// Linux stores old-style (8:8) numbers in i_block[0] when they fit,
		// otherwise new-style (12:20) numbers in i_block[1].
		if old := le.Uint32(ino.iblock[0:]); old != 0 {
			ino.DevMajor = (old >> 8) & 0xff
			ino.DevMinor = old & 0xff
		} else {
			dev := le.Uint32(ino.iblock[4:])
			ino.DevMajor = (dev & 0xfff00) >> 8
			ino.DevMinor = (dev & 0xff) | ((dev >> 12) & 0xfff00)
		}
	}
	return ino, nil
}

// Mode returns the inode's type and permission bits as an fs.FileMode.
func (ino *Inode) Mode() fs.FileMode {
	m := fs.FileMode(ino.RawMode & 0o777)
	if ino.RawMode&sISUID != 0 {
		m |= fs.ModeSetuid
	}
	if ino.RawMode&sISGID != 0 {
		m |= fs.ModeSetgid
	}
	if ino.RawMode&sISVTX != 0 {
		m |= fs.ModeSticky
	}
	switch ino.RawMode & sIFMT {
	case sIFREG:
	case sIFDIR:
		m |= fs.ModeDir
	case sIFLNK:
		m |= fs.ModeSymlink
	case sIFCHR:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case sIFBLK:
		m |= fs.ModeDevice
	case sIFIFO:
		m |= fs.ModeNamedPipe
	case sIFSOCK:
		m |= fs.ModeSocket
	default:
		m |= fs.ModeIrregular
	}
	return m
}

// IsDir reports whether the inode is a directory.
func (ino *Inode) IsDir() bool { return ino.RawMode&sIFMT == sIFDIR }

// IsRegular reports whether the inode is a regular file.
func (ino *Inode) IsRegular() bool { return ino.RawMode&sIFMT == sIFREG }

// IsSymlink reports whether the inode is a symbolic link.
func (ino *Inode) IsSymlink() bool { return ino.RawMode&sIFMT == sIFLNK }

func (ino *Inode) encrypted() bool { return ino.Flags&flagEncrypt != 0 }

func (ino *Inode) inline() bool { return ino.Flags&flagInlineData != 0 }

// FileInfo returns an fs.FileInfo for the inode under the given name. Its
// Sys method returns the *Inode.
func (ino *Inode) FileInfo(name string) fs.FileInfo { return fileInfo{name: name, ino: ino} }

type fileInfo struct {
	name string
	ino  *Inode
}

func (fi fileInfo) Name() string       { return fi.name }
func (fi fileInfo) Size() int64        { return fi.ino.Size }
func (fi fileInfo) Mode() fs.FileMode  { return fi.ino.Mode() }
func (fi fileInfo) ModTime() time.Time { return fi.ino.Mtime }
func (fi fileInfo) IsDir() bool        { return fi.ino.IsDir() }
func (fi fileInfo) Sys() any           { return fi.ino }
