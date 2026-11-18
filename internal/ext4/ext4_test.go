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
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// The fixtures in testdata are produced by hack/ext4-fixtures.sh with the
// real mkfs.ext4 and Linux kernel; the manifests record the kernel's view of
// the mounted filesystems.

type manifestEntry struct {
	Path      string            `json:"path"`
	Ino       uint32            `json:"ino"`
	Mode      uint32            `json:"mode"`
	UID       uint32            `json:"uid"`
	GID       uint32            `json:"gid"`
	Size      int64             `json:"size"`
	Nlink     uint32            `json:"nlink"`
	MtimeNs   int64             `json:"mtime_ns"`
	SHA256    string            `json:"sha256"`
	Link      *string           `json:"link"`
	RdevMajor *uint32           `json:"rdev_major"`
	RdevMinor *uint32           `json:"rdev_minor"`
	Xattrs    map[string]string `json:"xattrs"`
}

func fixturePath(t testing.TB, name string) string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name+".img.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), name+".img")
	o, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(o, zr); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

func openFixture(t testing.TB, name string) *Image {
	t.Helper()
	img, err := OpenFile(fixturePath(t, name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { _ = img.Close() })
	return img
}

func loadManifest(t testing.TB, name string) []manifestEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Entries []manifestEntry `json:"entries"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m.Entries
}

func fileSHA(t testing.TB, img *Image, ino *Inode) string {
	t.Helper()
	r, err := img.OpenSection(ino)
	if err != nil {
		t.Fatalf("open inode %d: %v", ino.Number, err)
	}
	h := sha256.New()
	if _, err := io.CopyBuffer(h, r, make([]byte, 1<<20)); err != nil {
		t.Fatalf("read inode %d: %v", ino.Number, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func readAll(t testing.TB, img *Image, path string) []byte {
	t.Helper()
	ino, err := img.Lookup(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := img.OpenSection(ino)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func verifyManifestEntry(t *testing.T, img *Image, e manifestEntry) {
	t.Helper()
	ino, err := img.Lookup(e.Path)
	if err != nil {
		t.Errorf("%s: lookup: %v", e.Path, err)
		return
	}
	errorf := func(format string, args ...any) {
		t.Helper()
		t.Errorf("%s: %s", e.Path, fmt.Sprintf(format, args...))
	}
	if ino.Number != e.Ino {
		errorf("ino %d, want %d", ino.Number, e.Ino)
	}
	if uint32(ino.RawMode) != e.Mode {
		errorf("mode %#o, want %#o", ino.RawMode, e.Mode)
	}
	if ino.UID != e.UID || ino.GID != e.GID {
		errorf("owner %d:%d, want %d:%d", ino.UID, ino.GID, e.UID, e.GID)
	}
	if ino.Size != e.Size {
		errorf("size %d, want %d", ino.Size, e.Size)
	}
	if ino.Links != e.Nlink {
		errorf("nlink %d, want %d", ino.Links, e.Nlink)
	}
	if got := ino.Mtime.UnixNano(); got != e.MtimeNs {
		errorf("mtime %d (%v), want %d", got, ino.Mtime, e.MtimeNs)
	}
	if e.SHA256 != "" {
		if got := fileSHA(t, img, ino); got != e.SHA256 {
			errorf("sha256 %s, want %s", got, e.SHA256)
		}
	}
	if e.Link != nil {
		got, err := img.Readlink(ino)
		if err != nil || got != *e.Link {
			errorf("readlink %q (%v), want %q", got, err, *e.Link)
		}
	}
	if e.RdevMajor != nil {
		if ino.DevMajor != *e.RdevMajor || ino.DevMinor != *e.RdevMinor {
			errorf("rdev %d:%d, want %d:%d", ino.DevMajor, ino.DevMinor, *e.RdevMajor, *e.RdevMinor)
		}
	}
	verifyManifestXattrs(t, img, ino, e)
}

func verifyManifestXattrs(t *testing.T, img *Image, ino *Inode, e manifestEntry) {
	t.Helper()
	xs, err := img.Xattrs(ino)
	if err != nil {
		t.Errorf("%s: xattrs: %v", e.Path, err)
		return
	}
	delete(xs, "system.data") // inline data, hidden by the kernel
	if len(xs) != len(e.Xattrs) {
		t.Errorf("%s: xattrs %v, want %v", e.Path, keys(xs), keys(e.Xattrs))
	}
	for k, v := range e.Xattrs {
		wv, _ := base64.StdEncoding.DecodeString(v)
		if gv, ok := xs[k]; !ok || !bytes.Equal(gv, wv) {
			t.Errorf("%s: xattr %s = %q, want %q", e.Path, k, gv, wv)
		}
	}
}

// verifyManifest checks every manifest entry against the reader and that
// Walk visits exactly the manifest paths.
func verifyManifest(t *testing.T, img *Image, name string) {
	entries := loadManifest(t, name)
	if len(entries) == 0 {
		t.Fatal("empty manifest")
	}
	want := make(map[string]bool, len(entries))
	for _, e := range entries {
		want[e.Path] = true
		verifyManifestEntry(t, img, e)
	}
	seen := map[string]bool{}
	var order []string
	err := img.Walk("/", func(rel string, ino *Inode) error {
		seen[rel] = true
		order = append(order, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for p := range want {
		if !seen[p] {
			t.Errorf("walk did not visit %s", p)
		}
	}
	for p := range seen {
		if !want[p] {
			t.Errorf("walk visited unexpected %s", p)
		}
	}
	// lexical order, parents first
	for i := 1; i < len(order); i++ {
		if order[i-1] != "." && !(pathLess(order[i-1], order[i])) {
			t.Errorf("walk order: %q before %q", order[i-1], order[i])
			break
		}
	}
}

// pathLess orders paths component-wise (the order of a sorted DFS).
func pathLess(a, b string) bool {
	ac, bc := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(ac) && i < len(bc); i++ {
		if ac[i] != bc[i] {
			return ac[i] < bc[i]
		}
	}
	return len(ac) < len(bc)
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestCleanManifest(t *testing.T) {
	img := openFixture(t, "clean")
	if img.Replayed() != 0 || img.NeedsRecovery() {
		t.Errorf("Replayed() = %d, NeedsRecovery() = %v for clean image", img.Replayed(), img.NeedsRecovery())
	}
	if w := img.Warnings(); len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
	verifyManifest(t, img, "clean")
}

func TestLegacyManifest(t *testing.T) {
	img := openFixture(t, "legacy")
	verifyManifest(t, img, "legacy")
	ino, err := img.Lookup("big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if ino.Flags&flagExtents != 0 {
		t.Fatal("legacy big.bin unexpectedly uses extents")
	}
	// the triple indirect block of the huge sparse file
	b := readAll(t, img, "frag.bin")
	for i := range 300 {
		blk := b[2*i*4096 : 2*i*4096+4096]
		if !bytes.HasPrefix(blk, fmt.Appendf(nil, "blk%06d|", i)) {
			t.Fatalf("frag.bin block %d: %q", i, blk[:16])
		}
	}
	r, _ := img.Lookup("sparse-huge.bin")
	f, _ := img.Open(r)
	got := make([]byte, 4096)
	if _, err := f.ReadAt(got, (4<<30)+(16<<20)); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("D"), 4096)) {
		t.Fatalf("triple indirect block: %q", got[:16])
	}
}

func TestFeaturesManifest(t *testing.T) {
	img := openFixture(t, "features")
	verifyManifest(t, img, "features")
	ino, err := img.Lookup("tiny.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !ino.inline() {
		t.Error("tiny.txt is not stored inline")
	}
	dir, err := img.Lookup("smalldir")
	if err != nil {
		t.Fatal(err)
	}
	if !dir.inline() {
		t.Error("smalldir is not stored inline")
	}
	ents, err := img.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 3 || ents[0].Name != "a" || ents[2].Name != "ccc" {
		t.Errorf("smalldir entries %+v", ents)
	}
	ea, err := img.Lookup("ea.txt")
	if err != nil {
		t.Fatal(err)
	}
	xs, err := img.Xattrs(ea)
	if err != nil {
		t.Fatal(err)
	}
	if len(xs["user.huge"]) != 10000 {
		t.Errorf("user.huge len %d", len(xs["user.huge"]))
	}
}

func TestTinyManifest(t *testing.T) {
	img := openFixture(t, "tiny")
	if img.BlockSize() != 1024 {
		t.Errorf("block size %d", img.BlockSize())
	}
	verifyManifest(t, img, "tiny")
}

func testDirty(t *testing.T, name string) {
	path := fixturePath(t, name)
	img, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = img.Close() }()
	if img.Replayed() < 1 || !img.NeedsRecovery() {
		t.Fatalf("Replayed() = %d, NeedsRecovery() = %v", img.Replayed(), img.NeedsRecovery())
	}
	t.Logf("%s: replayed %d transactions, %d blocks", name, img.Replayed(), len(img.overlay))
	verifyManifest(t, img, name)

	for _, p := range []string{"new", "new/sub/data.bin", "new/file-49", "renamed3.txt", "after-revoke.bin"} {
		if _, err := img.Lookup(p); err != nil {
			t.Errorf("lookup %s after replay: %v", p, err)
		}
	}
	for _, p := range []string{"base2.txt", "base3.txt", "doomed"} {
		if _, err := img.Lookup(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("lookup %s after replay: %v, want ErrNotExist", p, err)
		}
	}
	if got := string(readAll(t, img, "a/base1.txt")); got != "base1\nappended after mount\n" {
		t.Errorf("a/base1.txt = %q", got)
	}
	nw, _ := img.Lookup("new")
	xs, _ := img.Xattrs(nw)
	if string(xs["trusted.overlay.opaque"]) != "y" {
		t.Errorf("new: trusted.overlay.opaque = %q", xs["trusted.overlay.opaque"])
	}

	// Without replay the new tree only exists in the journal.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	st, _ := f.Stat()
	raw, err := OpenWithOptions(f, st.Size(), Options{SkipJournalReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	if raw.Replayed() != 0 {
		t.Errorf("Replayed() = %d with SkipJournalReplay", raw.Replayed())
	}
	if _, err := raw.Lookup("new"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("without replay, lookup new: %v, want ErrNotExist", err)
	}
}

func TestDirtyReplay(t *testing.T)       { testDirty(t, "dirty") }
func TestDirtyLegacyReplay(t *testing.T) { testDirty(t, "dirty-legacy") }

func TestCleanDetails(t *testing.T) {
	img := openFixture(t, "clean")
	t.Run("htree", func(t *testing.T) { testCleanHtree(t, img) })
	t.Run("symlinks", func(t *testing.T) { testCleanSymlinks(t, img) })
	t.Run("devices", func(t *testing.T) { testCleanDevices(t, img) })
	t.Run("xattrs", func(t *testing.T) { testCleanXattrs(t, img) })
	t.Run("sparse", func(t *testing.T) { testCleanSparse(t, img) })
	t.Run("frag-depth2", func(t *testing.T) { testCleanFragDepth2(t, img) })
	t.Run("hardlinks", func(t *testing.T) { testCleanHardlinks(t, img) })
	t.Run("modes", func(t *testing.T) { testCleanModes(t, img) })
	t.Run("lookup", func(t *testing.T) { testCleanLookup(t, img) })
	t.Run("walk-skipdir", func(t *testing.T) { testCleanWalkSkipDir(t, img) })
}

func testCleanHtree(t *testing.T, img *Image) {
	dir, err := img.Lookup("htree")
	if err != nil {
		t.Fatal(err)
	}
	if dir.Flags&flagIndex == 0 {
		t.Error("htree directory is not indexed")
	}
	ents, err := img.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 3000 {
		t.Fatalf("htree has %d entries, want 3000", len(ents))
	}
	for i, e := range ents {
		if want := fmt.Sprintf("entry-with-a-longer-name-%05d", i); e.Name != want || e.Type != FileTypeRegular {
			t.Fatalf("entry %d = %+v, want %s", i, e, want)
		}
	}
	if _, err := img.Lookup("htree/entry-with-a-longer-name-02999"); err != nil {
		t.Error(err)
	}
}

func testCleanSymlinks(t *testing.T, img *Image) {
	for p, want := range map[string]string{
		"sym-short": "hello.txt",
		"sym-long":  "dir1/dir2/dir3/" + strings.Repeat("x", 80),
	} {
		ino, err := img.Lookup(p)
		if err != nil {
			t.Fatal(err)
		}
		if ino.Mode()&fs.ModeSymlink == 0 {
			t.Errorf("%s mode %v", p, ino.Mode())
		}
		got, err := img.Readlink(ino)
		if err != nil || got != want {
			t.Errorf("%s -> %q (%v), want %q", p, got, err, want)
		}
	}
	if _, err := img.Lookup("sym-short/x"); err == nil {
		t.Error("lookup through symlink succeeded")
	}
}

func testCleanDevices(t *testing.T, img *Image) {
	for p, want := range map[string][3]uint32{
		"devs/whiteout": {0, 0, 1},
		"devs/null":     {1, 3, 1},
		"devs/sda1":     {8, 1, 0},
		"devs/bigdev":   {300, 70000, 1},
	} {
		ino, err := img.Lookup(p)
		if err != nil {
			t.Fatal(err)
		}
		if ino.DevMajor != want[0] || ino.DevMinor != want[1] {
			t.Errorf("%s: %d:%d, want %d:%d", p, ino.DevMajor, ino.DevMinor, want[0], want[1])
		}
		m := ino.Mode()
		if m&fs.ModeDevice == 0 || (m&fs.ModeCharDevice != 0) != (want[2] == 1) {
			t.Errorf("%s: mode %v", p, m)
		}
	}
	fifo, _ := img.Lookup("devs/fifo")
	if fifo.Mode()&fs.ModeNamedPipe == 0 {
		t.Errorf("fifo mode %v", fifo.Mode())
	}
	sock, _ := img.Lookup("devs/sock")
	if sock.Mode()&fs.ModeSocket == 0 {
		t.Errorf("sock mode %v", sock.Mode())
	}
	if _, err := img.Open(fifo); err == nil {
		t.Error("Open(fifo) succeeded")
	}
}

func testCleanXattrs(t *testing.T, img *Image) {
	ino, _ := img.Lookup("opaque")
	xs, err := img.Xattrs(ino)
	if err != nil {
		t.Fatal(err)
	}
	if string(xs["trusted.overlay.opaque"]) != "y" {
		t.Errorf("opaque xattrs %v", xs)
	}
	ino, _ = img.Lookup("uopaque")
	xs, _ = img.Xattrs(ino)
	if string(xs["user.overlay.opaque"]) != "y" {
		t.Errorf("uopaque xattrs %v", xs)
	}
	ino, _ = img.Lookup("xattrs.txt")
	if ino.fileACL == 0 {
		t.Error("xattrs.txt has no external xattr block")
	}
	xs, _ = img.Xattrs(ino)
	if len(xs["user.big"]) != 3000 || string(xs["user.small"]) != "v" || string(xs["trusted.t"]) != "trusted-value" {
		t.Errorf("xattrs.txt: %v", keys(xs))
	}
	ino, _ = img.Lookup("acl.txt")
	xs, _ = img.Xattrs(ino)
	if _, ok := xs["system.posix_acl_access"]; !ok {
		t.Errorf("acl.txt: %v", keys(xs))
	}
}

func testCleanSparse(t *testing.T, img *Image) {
	b := readAll(t, img, "sparse.bin")
	if len(b) != 8<<20 {
		t.Fatalf("size %d", len(b))
	}
	check := func(from, to int, c byte) {
		t.Helper()
		for i := from; i < to; i++ {
			if b[i] != c {
				t.Fatalf("sparse.bin[%d] = %q, want %q", i, b[i], c)
			}
		}
	}
	check(0, 4096, 'A')
	check(4096, (1<<20)+(512<<10), 0) // hole + unwritten
	check((1<<20)+(512<<10), (1<<20)+(512<<10)+4096, 'B')
	check((1<<20)+(512<<10)+4096, 3<<20, 0) // unwritten + hole
	check(3<<20, (3<<20)+65536, 'C')
	check((3<<20)+65536, (8<<20)-4096, 0)
	check((8<<20)-4096, 8<<20, 'D')

	ks := readAll(t, img, "keepsize.bin")
	if !bytes.Equal(ks, bytes.Repeat([]byte("K"), 5000)) {
		t.Errorf("keepsize.bin: len %d", len(ks))
	}
}

func testCleanFragDepth2(t *testing.T, img *Image) {
	ino, _ := img.Lookup("frag.bin")
	if d := ino.iblock[6]; d < 2 {
		t.Errorf("frag.bin extent depth %d, want >= 2", d)
	}
	b := readAll(t, img, "frag.bin")
	for i := range 2000 {
		blk := b[2*i*4096 : 2*i*4096+4096]
		if !bytes.HasPrefix(blk, fmt.Appendf(nil, "blk%06d|", i)) {
			t.Fatalf("frag.bin block %d: %q", i, blk[:16])
		}
		if i < 1999 && !bytes.Equal(b[(2*i+1)*4096:(2*i+2)*4096], make([]byte, 4096)) {
			t.Fatalf("frag.bin hole %d not zero", i)
		}
	}
}

func testCleanHardlinks(t *testing.T, img *Image) {
	a, _ := img.Lookup("linked.txt")
	b, _ := img.Lookup("dir1/linked-hard.txt")
	if a.Number != b.Number || a.Links != 2 {
		t.Errorf("hardlinks: %d/%d links %d", a.Number, b.Number, a.Links)
	}
}

func testCleanModes(t *testing.T, img *Image) {
	for p, want := range map[string]fs.FileMode{
		"suid":       fs.ModeSetuid | 0o755,
		"sgid-dir":   fs.ModeDir | fs.ModeSetgid | 0o775,
		"sticky-dir": fs.ModeDir | fs.ModeSticky | 0o777,
		"hello.txt":  0o640,
	} {
		ino, _ := img.Lookup(p)
		if ino.Mode() != want {
			t.Errorf("%s: mode %v, want %v", p, ino.Mode(), want)
		}
		fi := ino.FileInfo(filepath.Base(p))
		if fi.Name() != filepath.Base(p) || fi.Mode() != want || fi.Sys().(*Inode) != ino || fi.IsDir() != ino.IsDir() {
			t.Errorf("%s: bad FileInfo", p)
		}
	}
	h, _ := img.Lookup("hello.txt")
	if h.Mtime.Nanosecond() != 123456789 || h.Atime.Nanosecond() != 987654321 {
		t.Errorf("hello.txt times %v %v", h.Mtime, h.Atime)
	}
}

func testCleanLookup(t *testing.T, img *Image) {
	for _, p := range []string{"", "/", ".", "//dir1//", "dir1/dir2/../dir2/dir3/deep.txt"} {
		if _, err := img.Lookup(p); err != nil {
			t.Errorf("lookup %q: %v", p, err)
		}
	}
	if _, err := img.Lookup("nope/x"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	if _, err := img.Lookup("hello.txt/x"); !errors.Is(err, ErrNotDir) {
		t.Errorf("through file: %v", err)
	}
	root, _ := img.Lookup("")
	if root.Number != rootIno || !root.IsDir() {
		t.Errorf("root %+v", root)
	}
}

func testCleanWalkSkipDir(t *testing.T, img *Image) {
	var got []string
	err := img.Walk("dir1", func(rel string, ino *Inode) error {
		got = append(got, rel)
		if rel == "dir2/dir3" {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".", "dir2", "dir2/dir3", "linked-hard.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("walk = %v, want %v", got, want)
	}
	boom := errors.New("boom")
	if err := img.Walk("", func(string, *Inode) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("walk error = %v", err)
	}
}

type countingReaderAt struct {
	r     io.ReaderAt
	calls atomic.Int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.calls.Add(1)
	return c.r.ReadAt(p, off)
}

func TestLargeReadsAreCoalesced(t *testing.T) {
	path := fixturePath(t, "clean")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	st, _ := f.Stat()
	cr := &countingReaderAt{r: f}
	img, err := Open(cr, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	ino, err := img.Lookup("big.bin")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := img.Open(ino)
	buf := make([]byte, ino.Size)
	before := cr.calls.Load()
	if n, err := r.ReadAt(buf, 0); n != len(buf) || (err != nil && err != io.EOF) {
		t.Fatal(n, err)
	}
	calls := cr.calls.Load() - before
	t.Logf("5 MiB read took %d ReadAt calls", calls)
	if calls > 8 {
		t.Errorf("5 MiB read took %d ReadAt calls", calls)
	}
	for i := range buf {
		if buf[i] != byte(i%251) {
			t.Fatalf("big.bin[%d] = %d", i, buf[i])
		}
	}
	// partial, unaligned reads
	p := make([]byte, 10000)
	n, err := r.ReadAt(p, 12345)
	if n != len(p) || err != nil {
		t.Fatal(n, err)
	}
	for i := range p {
		if p[i] != byte((12345+i)%251) {
			t.Fatalf("unaligned read mismatch at %d", i)
		}
	}
	n, err = r.ReadAt(p, ino.Size-100)
	if n != 100 || err != io.EOF {
		t.Fatalf("tail read n=%d err=%v", n, err)
	}
}

func TestRejectsGarbage(t *testing.T) {
	if _, err := Open(bytes.NewReader(make([]byte, 4096)), 4096); err == nil {
		t.Error("opened zeros")
	}
	if _, err := Open(bytes.NewReader(nil), 0); err == nil {
		t.Error("opened empty")
	}
	// unsupported incompat feature
	path := fixturePath(t, "tiny")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[1024+0x60] |= incompatCompression
	if _, err := Open(bytes.NewReader(b), int64(len(b))); !errors.Is(err, ErrUnsupported) {
		t.Errorf("compression feature: %v", err)
	}
}

func TestConcurrentReads(t *testing.T) {
	img := openFixture(t, "clean")
	var paths []string
	want := map[string]string{}
	for _, e := range loadManifest(t, "clean") {
		if e.SHA256 != "" && e.Size > 0 {
			paths = append(paths, e.Path)
			want[e.Path] = e.SHA256
		}
	}
	errs := make(chan error, 8)
	for w := range 8 {
		go func(w int) {
			for i := w; i < len(paths); i += 8 {
				ino, err := img.Lookup(paths[i])
				if err != nil {
					errs <- err
					return
				}
				r, err := img.OpenSection(ino)
				if err != nil {
					errs <- err
					return
				}
				h := sha256.New()
				if _, err := io.Copy(h, r); err != nil {
					errs <- err
					return
				}
				if hex.EncodeToString(h.Sum(nil)) != want[paths[i]] {
					errs <- fmt.Errorf("%s: checksum mismatch", paths[i])
					return
				}
			}
			errs <- nil
		}(w)
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}
