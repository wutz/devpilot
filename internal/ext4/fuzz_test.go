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
	"io"
	"os"
	"testing"
	"time"
)

// exercise opens data as an image and touches everything reachable. It must
// never panic or hang, whatever the input.
func exercise(data []byte) {
	img, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return
	}
	budget := 2000 // inodes
	_ = img.Walk("", func(rel string, ino *Inode) error {
		budget--
		if budget < 0 {
			return io.EOF
		}
		_, _ = img.Xattrs(ino)
		_ = ino.Mode()
		switch {
		case ino.IsSymlink():
			_, _ = img.Readlink(ino)
		case ino.IsRegular():
			if r, err := img.Open(ino); err == nil {
				buf := make([]byte, 64<<10)
				for off := int64(0); off < ino.Size && off < 1<<20; off += int64(len(buf)) {
					if _, err := r.ReadAt(buf, off); err != nil && err != io.EOF {
						break
					}
				}
			}
		}
		return nil
	})
	_, _ = img.Lookup("d/e/f.txt")
	_, _ = img.Lookup("a/../b/./c")
	for n := uint32(1); n <= 16; n++ {
		if ino, err := img.Inode(n); err == nil && ino.IsDir() {
			_, _ = img.ReadDir(ino)
		}
	}
}

func FuzzOpen(f *testing.F) {
	tiny := fixturePath(f, "tiny")
	b, err := os.ReadFile(tiny)
	if err != nil {
		f.Fatal(err)
	}
	// Go's fuzzer minimizes every new interesting input and stalls for
	// minutes on inputs of hundreds of KiB, so keep the seeds small: the
	// tiny image's metadata, directories and most file data live in its
	// first 45 blocks (only one xattr block lies beyond).
	f.Add(b[:45<<10])
	// headers of the 4K-block images: superblock + group descriptors
	for _, name := range []string{"clean", "features", "legacy"} {
		hb, err := os.ReadFile(fixturePath(f, name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(hb[:12<<10])
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			exercise(data)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("input (%d bytes) took longer than 10s", len(data))
		}
	})
}

type memJournal struct {
	data []byte
	bs   int
}

func (m memJournal) read(i uint64) ([]byte, error) {
	off := i * uint64(m.bs)
	if off >= uint64(len(m.data)) || uint64(len(m.data))-off < uint64(m.bs) {
		return nil, io.ErrUnexpectedEOF
	}
	return m.data[off : off+uint64(m.bs)], nil
}

// FuzzJournal feeds arbitrary bytes to the jbd2 scanner as journal contents.
func FuzzJournal(f *testing.F) {
	img := openFixture(f, "dirty")
	jino, err := img.Inode(img.sb.journalInum)
	if err != nil {
		f.Fatal(err)
	}
	r, err := img.Open(jino)
	if err != nil {
		f.Fatal(err)
	}
	seed := make([]byte, 64*img.blockSize)
	if _, err := r.ReadAt(seed, 0); err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 1024 {
			return
		}
		js, err := parseJournalSuper(data)
		if err != nil {
			return
		}
		if js.blockSize < 1024 || js.blockSize > 65536 || js.blockSize&(js.blockSize-1) != 0 {
			return
		}
		if uint64(js.maxLen) > uint64(len(data))/uint64(js.blockSize) {
			return
		}
		_, _ = planReplay(js, memJournal{data: data, bs: int(js.blockSize)})
	})
}

func TestJournalSeedPlan(t *testing.T) {
	img := openFixture(t, "dirty")
	jino, err := img.Inode(img.sb.journalInum)
	if err != nil {
		t.Fatal(err)
	}
	r, err := img.Open(jino)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, jino.Size)
	if _, err := r.ReadAt(data, 0); err != nil {
		t.Fatal(err)
	}
	js, err := parseJournalSuper(data)
	if err != nil {
		t.Fatal(err)
	}
	if !js.csum() || js.tagBytes() != 16 {
		t.Errorf("expected csum v3 journal, incompat %#x", js.incompat)
	}
	plan, err := planReplay(js, memJournal{data: data, bs: int(js.blockSize)})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dirty journal: %d txns, %d blocks, %d revoked tags", plan.txns, len(plan.blocks), plan.revoked)
	if plan.txns != img.Replayed() || len(plan.blocks) != len(img.overlay) {
		t.Errorf("plan %d txns/%d blocks, image %d/%d", plan.txns, len(plan.blocks), img.Replayed(), len(img.overlay))
	}
	// s_start == 0 means the journal is clean: nothing to replay.
	js.start = 0
	if p, _ := planReplay(js, memJournal{data: data, bs: int(js.blockSize)}); p.txns != 0 {
		t.Errorf("clean journal replayed %d txns", p.txns)
	}
}
