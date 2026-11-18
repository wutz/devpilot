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

const (
	xattrMagic        = 0xEA020000
	xattrBlockHeader  = 32
	xattrEntryHeader  = 16
	xattrMaxValueSize = 65536 // XATTR_SIZE_MAX
	// xattrMaxTotal bounds the memory a single Xattrs call may allocate.
	xattrMaxTotal = 16 << 20

	xattrIndexUser       = 1
	xattrIndexACLAccess  = 2
	xattrIndexACLDefault = 3
	xattrIndexSystem     = 7
)

// xattrPrefixes is the ext4 name-index table. Index 9 (encryption context)
// is internal to the kernel and never exposed.
var xattrPrefixes = map[uint8]string{
	xattrIndexUser:       "user.",
	xattrIndexACLAccess:  "system.posix_acl_access",
	xattrIndexACLDefault: "system.posix_acl_default",
	4:                    "trusted.",
	5:                    "lustre.",
	6:                    "security.",
	xattrIndexSystem:     "system.",
	8:                    "system.richacl",
	10:                   "gnu.",
}

type rawXattr struct {
	index uint8
	name  string
	value []byte
}

// parseXattrEntries decodes an xattr entry table. Entries start at
// buf[start:]; value offsets are relative to valueBase.
func (img *Image) parseXattrEntries(ino uint32, buf []byte, start int, valueBase []byte, fn func(rawXattr) error) error {
	le := binary.LittleEndian
	off := start
	for {
		if off+4 > len(buf) {
			return corruptf("inode %d: unterminated xattr table", ino)
		}
		if le.Uint32(buf[off:]) == 0 {
			return nil
		}
		if off+xattrEntryHeader > len(buf) {
			return corruptf("inode %d: truncated xattr entry", ino)
		}
		nameLen := int(buf[off])
		index := buf[off+1]
		voff := int(le.Uint16(buf[off+2:]))
		vinum := le.Uint32(buf[off+4:])
		vsize := int64(le.Uint32(buf[off+8:]))
		entLen := (xattrEntryHeader + nameLen + 3) &^ 3
		if off+entLen > len(buf) || off+xattrEntryHeader+nameLen > len(buf) {
			return corruptf("inode %d: xattr entry overflows table", ino)
		}
		name := string(buf[off+xattrEntryHeader : off+xattrEntryHeader+nameLen])
		if vsize > xattrMaxValueSize {
			return corruptf("inode %d: xattr value size %d", ino, vsize)
		}
		var value []byte
		if vinum != 0 {
			v, err := img.readEAInode(ino, vinum, vsize)
			if err != nil {
				return err
			}
			value = v
		} else {
			if int64(voff)+vsize > int64(len(valueBase)) {
				return corruptf("inode %d: xattr value [%d,+%d) out of bounds", ino, voff, vsize)
			}
			value = append([]byte{}, valueBase[voff:int64(voff)+vsize]...)
		}
		if err := fn(rawXattr{index: index, name: name, value: value}); err != nil {
			return err
		}
		off += entLen
	}
}

func (img *Image) readEAInode(owner, num uint32, size int64) ([]byte, error) {
	if img.sb.incompat&incompatEAInode == 0 {
		return nil, corruptf("inode %d: xattr value in inode %d without ea_inode feature", owner, num)
	}
	if num == owner {
		return nil, corruptf("inode %d: xattr value inode refers to itself", owner)
	}
	ea, err := img.Inode(num)
	if err != nil {
		return nil, fmt.Errorf("ext4: inode %d: xattr value inode: %w", owner, err)
	}
	if ea.Flags&flagEAInode == 0 || ea.inline() {
		return nil, corruptf("inode %d: xattr value inode %d is not an EA inode", owner, num)
	}
	if ea.Size < size {
		return nil, corruptf("inode %d: xattr value inode %d too short", owner, num)
	}
	r, err := img.openReader(ea)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	if _, err := r.ReadAt(buf, 0); err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}

// forEachXattr visits in-inode then external-block attributes.
func (img *Image) forEachXattr(ino *Inode, fn func(rawXattr) error) error {
	if err := img.forEachIbodyXattr(ino, fn); err != nil {
		return err
	}
	if ino.fileACL == 0 {
		return nil
	}
	if ino.fileACL >= img.sb.blocksCount {
		return corruptf("inode %d: xattr block %d beyond end of filesystem", ino.Number, ino.fileACL)
	}
	b, err := img.readBlock(ino.fileACL)
	if err != nil {
		return err
	}
	le := binary.LittleEndian
	if le.Uint32(b[0:]) != xattrMagic {
		return corruptf("inode %d: bad xattr block magic %#x", ino.Number, le.Uint32(b[0:]))
	}
	if le.Uint32(b[8:]) != 1 {
		return corruptf("inode %d: xattr block h_blocks=%d", ino.Number, le.Uint32(b[8:]))
	}
	return img.parseXattrEntries(ino.Number, b, xattrBlockHeader, b, fn)
}

func (img *Image) forEachIbodyXattr(ino *Inode, fn func(rawXattr) error) error {
	if len(ino.ibody) < 4 || binary.LittleEndian.Uint32(ino.ibody) != xattrMagic {
		return nil
	}
	region := ino.ibody[4:]
	return img.parseXattrEntries(ino.Number, region, 0, region, fn)
}

// Xattrs returns all extended attributes of the inode keyed by their full
// name ("user.foo", "trusted.overlay.opaque", "security.capability",
// "system.posix_acl_access", "system.data", ...), from the in-inode area and
// the external xattr block; values stored in EA inodes are resolved.
//
// POSIX ACLs are converted from the ext4 on-disk encoding to the
// posix_acl_xattr format returned by getxattr(2) (version 2), so they can
// be used directly (e.g. in tar PAX headers or setxattr).
func (img *Image) Xattrs(ino *Inode) (map[string][]byte, error) {
	out := map[string][]byte{}
	total := 0
	err := img.forEachXattr(ino, func(x rawXattr) error {
		total += len(x.value) + len(x.name)
		if total > xattrMaxTotal {
			return corruptf("inode %d: extended attributes exceed %d bytes", ino.Number, xattrMaxTotal)
		}
		prefix, ok := xattrPrefixes[x.index]
		if !ok {
			return nil
		}
		value := x.value
		if x.index == xattrIndexACLAccess || x.index == xattrIndexACLDefault {
			v, err := aclToXattr(value)
			if err != nil {
				return fmt.Errorf("inode %d: %s: %w", ino.Number, prefix+x.name, err)
			}
			if v == nil {
				return nil // empty ACL: the kernel reports no attribute
			}
			value = v
		}
		name := prefix + x.name
		if _, dup := out[name]; !dup {
			out[name] = value
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ACL tags.
const (
	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclGroup    = 0x08
	aclMask     = 0x10
	aclOther    = 0x20
)

// aclToXattr converts an ext4 on-disk ACL (version 1, short entries for
// *_OBJ/MASK/OTHER) into the posix_acl_xattr format (version 2).
func aclToXattr(raw []byte) ([]byte, error) {
	le := binary.LittleEndian
	if len(raw) < 4 || le.Uint32(raw) != 1 {
		return nil, corruptf("bad ACL header")
	}
	p := raw[4:]
	if len(p) == 0 {
		return nil, nil
	}
	out := make([]byte, 4, 4+2*len(p))
	le.PutUint32(out, 2)
	for len(p) > 0 {
		if len(p) < 4 {
			return nil, corruptf("truncated ACL entry")
		}
		tag := le.Uint16(p)
		perm := le.Uint16(p[2:])
		id := uint32(0xFFFFFFFF)
		switch tag {
		case aclUserObj, aclGroupObj, aclMask, aclOther:
			p = p[4:]
		case aclUser, aclGroup:
			if len(p) < 8 {
				return nil, corruptf("truncated ACL entry")
			}
			id = le.Uint32(p[4:])
			p = p[8:]
		default:
			return nil, corruptf("bad ACL tag %#x", tag)
		}
		out = le.AppendUint16(out, tag)
		out = le.AppendUint16(out, perm)
		out = le.AppendUint32(out, id)
	}
	return out, nil
}

// inlineData returns the inline contents of an inline_data inode: the 60
// bytes of i_block followed by the value of the in-inode "system.data"
// attribute.
func (img *Image) inlineData(ino *Inode) ([]byte, error) {
	sys, err := img.systemData(ino)
	if err != nil {
		return nil, err
	}
	data := make([]byte, 0, 60+len(sys))
	data = append(data, ino.iblock[:]...)
	return append(data, sys...), nil
}

func (img *Image) systemData(ino *Inode) ([]byte, error) {
	var sys []byte
	found := false
	err := img.forEachIbodyXattr(ino, func(x rawXattr) error {
		if !found && x.index == xattrIndexSystem && x.name == "data" {
			sys, found = x.value, true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, corruptf("inode %d: inline data without system.data attribute", ino.Number)
	}
	return sys, nil
}
