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
	"testing"
)

// jbuilder assembles a synthetic jbd2 journal in memory.
type jbuilder struct {
	bs       int
	maxLen   uint32
	incompat uint32
	uuid     [16]byte
	data     []byte
}

func newJBuilder(incompat uint32) *jbuilder {
	b := &jbuilder{bs: 1024, maxLen: 20, incompat: incompat}
	copy(b.uuid[:], "0123456789abcdef")
	b.data = make([]byte, b.bs*int(b.maxLen))
	return b
}

func (b *jbuilder) block(i int) []byte { return b.data[i*b.bs : (i+1)*b.bs] }

func (b *jbuilder) seed() uint32 { return crc32cRaw(^uint32(0), b.uuid[:]) }

func (b *jbuilder) csum() bool { return b.incompat&(jbd2IncompatCsumV2|jbd2IncompatCsumV3) != 0 }

func (b *jbuilder) header(i int, btype, seq uint32) []byte {
	blk := b.block(i)
	clear(blk)
	be := binary.BigEndian
	be.PutUint32(blk[0:], jbd2Magic)
	be.PutUint32(blk[4:], btype)
	be.PutUint32(blk[8:], seq)
	return blk
}

func (b *jbuilder) super(seq, start uint32) {
	blk := b.header(0, jbd2SuperV2, 0)
	be := binary.BigEndian
	be.PutUint32(blk[0xC:], uint32(b.bs))
	be.PutUint32(blk[0x10:], b.maxLen)
	be.PutUint32(blk[0x14:], 1)
	be.PutUint32(blk[0x18:], seq)
	be.PutUint32(blk[0x1C:], start)
	be.PutUint32(blk[0x28:], b.incompat)
	copy(blk[0x30:], b.uuid[:])
}

func (b *jbuilder) tail(blk []byte) {
	if b.csum() {
		binary.BigEndian.PutUint32(blk[b.bs-4:], 0)
		binary.BigEndian.PutUint32(blk[b.bs-4:], crc32cRaw(b.seed(), blk))
	}
}

type jtagSpec struct {
	fsblk uint64
	at    int // journal block holding the data
	data  []byte
}

func (b *jbuilder) descriptor(i int, seq uint32, tags []jtagSpec) {
	blk := b.header(i, jbd2Descriptor, seq)
	be := binary.BigEndian
	js := journalSuper{incompat: b.incompat}
	off := 12
	for n, t := range tags {
		data := b.block(t.at)
		copy(data, t.data)
		var flags uint32
		if be.Uint32(t.data) == jbd2Magic {
			flags |= jbd2FlagEscape
			be.PutUint32(data, 0)
		}
		if n > 0 {
			flags |= jbd2FlagSameUUID
		}
		if n == len(tags)-1 {
			flags |= jbd2FlagLastTag
		}
		var sb [4]byte
		be.PutUint32(sb[:], seq)
		sum := crc32cRaw(crc32cRaw(b.seed(), sb[:]), data)
		be.PutUint32(blk[off:], uint32(t.fsblk))
		if b.incompat&jbd2IncompatCsumV3 != 0 {
			be.PutUint32(blk[off+4:], flags)
			be.PutUint32(blk[off+8:], uint32(t.fsblk>>32))
			be.PutUint32(blk[off+12:], sum)
		} else {
			be.PutUint16(blk[off+4:], uint16(sum))
			be.PutUint16(blk[off+6:], uint16(flags))
			if b.incompat&jbd2Incompat64Bit != 0 {
				be.PutUint32(blk[off+8:], uint32(t.fsblk>>32))
			}
		}
		off += js.tagBytes()
		if n == 0 {
			copy(blk[off:], b.uuid[:])
			off += 16
		}
	}
	b.tail(blk)
}

func (b *jbuilder) commit(i int, seq uint32) {
	blk := b.header(i, jbd2Commit, seq)
	if b.csum() {
		binary.BigEndian.PutUint32(blk[16:], crc32cRaw(b.seed(), blk))
	}
}

func (b *jbuilder) revoke(i int, seq uint32, fsblks ...uint64) {
	blk := b.header(i, jbd2Revoke, seq)
	be := binary.BigEndian
	off := 16
	for _, f := range fsblks {
		if b.incompat&jbd2Incompat64Bit != 0 {
			be.PutUint64(blk[off:], f)
			off += 8
		} else {
			be.PutUint32(blk[off:], uint32(f))
			off += 4
		}
	}
	be.PutUint32(blk[12:], uint32(off))
	b.tail(blk)
}

func fill(c byte) []byte { return bytes.Repeat([]byte{c}, 1024) }

func TestPlanReplaySynthetic(t *testing.T) {
	escaped := fill('E')
	binary.BigEndian.PutUint32(escaped, jbd2Magic)

	for name, incompat := range map[string]uint32{
		"32bit-nocsum": jbd2IncompatRevoke,
		"64bit-csum3":  jbd2IncompatRevoke | jbd2Incompat64Bit | jbd2IncompatCsumV3,
		"64bit-csum2":  jbd2IncompatRevoke | jbd2Incompat64Bit | jbd2IncompatCsumV2,
	} {
		t.Run(name, func(t *testing.T) {
			b := newJBuilder(incompat)
			b.super(100, 15)
			big := uint64(600)
			if incompat&jbd2Incompat64Bit != 0 {
				big = 1<<33 + 7
			}
			// txn 100: wraps nothing yet
			b.descriptor(15, 100, []jtagSpec{{500, 16, escaped}, {501, 17, fill('a')}})
			b.commit(18, 100)
			// txn 101: data wraps around to the start of the log; revokes 501
			b.descriptor(19, 101, []jtagSpec{{502, 1, fill('b')}, {503, 2, fill('c')}})
			b.revoke(3, 101, 501)
			b.commit(4, 101)
			// txn 102: rewrites 503
			b.descriptor(5, 102, []jtagSpec{{503, 6, fill('d')}, {big, 7, fill('g')}})
			b.commit(8, 102)
			// txn 103: never committed
			b.descriptor(9, 103, []jtagSpec{{504, 10, fill('x')}})

			src := memJournal{data: b.data, bs: b.bs}
			js, err := parseJournalSuper(b.block(0))
			if err != nil {
				t.Fatal(err)
			}
			plan, err := planReplay(js, src)
			if err != nil {
				t.Fatal(err)
			}
			if plan.txns != 3 {
				t.Fatalf("txns = %d, want 3", plan.txns)
			}
			if plan.revoked != 1 {
				t.Errorf("revoked = %d, want 1", plan.revoked)
			}
			want := map[uint64]journalTag{
				500: {fsblk: 500, jblk: 16, escape: true},
				502: {fsblk: 502, jblk: 1},
				503: {fsblk: 503, jblk: 6},
				big: {fsblk: big, jblk: 7},
			}
			if len(plan.blocks) != len(want) {
				t.Errorf("blocks = %+v", plan.blocks)
			}
			for k, v := range want {
				if plan.blocks[k] != v {
					t.Errorf("block %d = %+v, want %+v", k, plan.blocks[k], v)
				}
			}

			if !b.csum() {
				return
			}
			// A bad data checksum in txn 102 ends the log before it.
			b.block(6)[100] ^= 0xFF
			plan, err = planReplay(js, src)
			if err != nil {
				t.Fatal(err)
			}
			if plan.txns != 2 || plan.blocks[503].jblk != 2 {
				t.Errorf("after data corruption: txns=%d 503=%+v", plan.txns, plan.blocks[503])
			}
			// A bad commit checksum ends the log too.
			b.block(6)[100] ^= 0xFF
			b.block(4)[200] ^= 0xFF
			plan, _ = planReplay(js, src)
			if plan.txns != 1 {
				t.Errorf("after commit corruption: txns=%d", plan.txns)
			}
		})
	}
}

func TestPlanReplaySequenceMismatch(t *testing.T) {
	b := newJBuilder(jbd2IncompatRevoke)
	b.super(7, 1)
	b.descriptor(1, 7, []jtagSpec{{10, 2, fill('a')}})
	b.commit(3, 7)
	b.descriptor(4, 9, []jtagSpec{{11, 5, fill('b')}}) // wrong sequence: stale
	b.commit(6, 9)
	js, _ := parseJournalSuper(b.block(0))
	plan, err := planReplay(js, memJournal{data: b.data, bs: b.bs})
	if err != nil {
		t.Fatal(err)
	}
	if plan.txns != 1 || len(plan.blocks) != 1 {
		t.Errorf("txns=%d blocks=%v", plan.txns, plan.blocks)
	}
}
