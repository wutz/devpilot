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

package commit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wutz/devpilot/internal/ext4"
)

func openExt4Fixture(t *testing.T, name string) *ext4.Image {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "ext4", "testdata", name+".img.gz"))
	if err != nil {
		t.Fatalf("open fixture %s: %v", name, err)
	}
	defer func() { _ = f.Close() }()

	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip %s: %v", name, err)
	}
	defer func() { _ = zr.Close() }()

	rawPath := filepath.Join(t.TempDir(), name+".img")
	out, err := os.Create(rawPath)
	if err != nil {
		t.Fatalf("create %s: %v", rawPath, err)
	}
	if _, err := io.Copy(out, zr); err != nil {
		_ = out.Close()
		t.Fatalf("decompress %s: %v", name, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close %s: %v", rawPath, err)
	}

	img, err := ext4.OpenFile(rawPath)
	if err != nil {
		t.Fatalf("ext4.OpenFile(%s): %v", name, err)
	}
	t.Cleanup(func() { _ = img.Close() })
	return img
}

func TestExt4SourceCleanImage(t *testing.T) {
	img := openExt4Fixture(t, "clean")

	const testLostFound = "lost+found"

	var buf bytes.Buffer
	stats, err := WriteLayer(&buf, Options{
		Source:       &Ext4Source{Img: img},
		ExcludePaths: []string{testLostFound, "htree", "big.bin", "frag.bin", "sparse.bin", "keepsize.bin"},
	})
	if err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}

	layer := readLayer(t, buf.Bytes())

	// Whiteouts become .wh.<name> empty files, while non-zero devices/FIFOs/sockets are omitted.
	if got, ok := layer["devs/.wh.whiteout"]; !ok || got.header.Typeflag != tar.TypeReg || got.header.Size != 0 {
		t.Errorf("devs/.wh.whiteout = %+v, ok=%v", got, ok)
	}
	for _, omitted := range []string{"devs/null", "devs/sda1", "devs/bigdev", "devs/fifo", "devs/sock"} {
		if _, ok := layer[omitted]; ok {
			t.Errorf("unexpected special file %s in OCI layer", omitted)
		}
	}

	// Both trusted.overlay.opaque and user.overlay.opaque produce opaque markers without an external index.
	for _, opq := range []string{"opaque/.wh..wh..opq", "uopaque/.wh..wh..opq"} {
		if got, ok := layer[opq]; !ok || got.header.Typeflag != tar.TypeReg {
			t.Errorf("%s missing or wrong type: %+v, ok=%v", opq, got, ok)
		}
	}
	if got, ok := layer["opaque/.wh.gone"]; !ok || got.header.Typeflag != tar.TypeReg {
		t.Errorf("opaque/.wh.gone = %+v, ok=%v", got, ok)
	}
	if got := layer["opaque/inner.txt"].content; got != "inner\n" {
		t.Errorf("opaque/inner.txt = %q", got)
	}

	// Hardlinks are detected by inode number in lexical order (dir1/ precedes linked.txt).
	if got := layer["dir1/linked-hard.txt"]; got.header.Typeflag != tar.TypeReg || got.content != "linked content\n" {
		t.Errorf("dir1/linked-hard.txt = %+v", got)
	}
	if got := layer["linked.txt"]; got.header.Typeflag != tar.TypeLink || got.header.Linkname != "dir1/linked-hard.txt" {
		t.Errorf("linked.txt = %+v, want hardlink to dir1/linked-hard.txt", got)
	}

	// Fast and slow symlinks.
	if got := layer["sym-short"]; got.header.Typeflag != tar.TypeSymlink || got.header.Linkname != "hello.txt" {
		t.Errorf("sym-short = %+v", got)
	}
	wantLong := "dir1/dir2/dir3/" + strings.Repeat("x", 80)
	if got := layer["sym-long"]; got.header.Typeflag != tar.TypeSymlink || got.header.Linkname != wantLong {
		t.Errorf("sym-long = %+v", got)
	}

	if got := layer["hello.txt"]; got.content != "hello world\n" || got.header.Mode != 0o640 {
		t.Errorf("hello.txt content=%q mode=%#o", got.content, got.header.Mode)
	}
	if stats.Whiteouts != 2 || stats.OpaqueDirs != 2 || stats.Hardlinks != 1 {
		t.Errorf("unexpected stats: %+v", stats)
	}
}

func TestExt4SourceDirtyJournalReplay(t *testing.T) {
	img := openExt4Fixture(t, "dirty")
	if !img.NeedsRecovery() || img.Replayed() == 0 {
		t.Fatalf("expected dirty image with replayed transactions, got needsRecovery=%v replayed=%d",
			img.NeedsRecovery(), img.Replayed())
	}

	const testLostFound = "lost+found"
	var buf bytes.Buffer
	stats, err := WriteLayer(&buf, Options{
		Source:       &Ext4Source{Img: img},
		ExcludePaths: []string{testLostFound, "base-big.bin", "after-revoke.bin", "new/sub/data.bin"},
	})
	if err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}
	layer := readLayer(t, buf.Bytes())

	if got := layer["a/base1.txt"].content; got != "base1\nappended after mount\n" {
		t.Errorf("a/base1.txt = %q", got)
	}
	if got := layer["renamed3.txt"].content; got != "base3 - will be renamed\n" {
		t.Errorf("renamed3.txt = %q", got)
	}
	if _, ok := layer["base2.txt"]; ok {
		t.Error("deleted base2.txt should not be present after journal replay")
	}
	if _, ok := layer["doomed/"]; ok {
		t.Error("deleted doomed/ should not be present after journal replay")
	}
	if _, ok := layer["new/.wh..wh..opq"]; !ok {
		t.Error("new/.wh..wh..opq missing")
	}
	if _, ok := layer["new/.wh.wh"]; !ok {
		t.Error("new/.wh.wh missing")
	}
	if got := layer["new/link"]; got.header.Typeflag != tar.TypeSymlink || got.header.Linkname != "../a/base1.txt" {
		t.Errorf("new/link = %+v", got)
	}
	if stats.OpaqueDirs != 1 || stats.Whiteouts != 1 {
		t.Errorf("unexpected stats: %+v", stats)
	}
}

func TestExt4SourceInlineAndSubdirRoot(t *testing.T) {
	img := openExt4Fixture(t, "features")

	// Root="." walks the full features image (inline files/dirs + opaque/whiteout).
	const testLostFound = "lost+found"
	var fullBuf bytes.Buffer
	if _, err := WriteLayer(&fullBuf, Options{
		Source:       &Ext4Source{Img: img},
		ExcludePaths: []string{testLostFound},
	}); err != nil {
		t.Fatalf("WriteLayer root: %v", err)
	}
	fullLayer := readLayer(t, fullBuf.Bytes())
	if got := fullLayer["tiny.txt"].content; got != "tiny file\n" {
		t.Errorf("tiny.txt = %q", got)
	}
	if got := fullLayer["smalldir/bb"].content; got != "bb" {
		t.Errorf("smalldir/bb = %q", got)
	}
	if _, ok := fullLayer[".wh.whiteout"]; !ok {
		t.Error(".wh.whiteout missing")
	}
	if _, ok := fullLayer["opaque/.wh..wh..opq"]; !ok {
		t.Error("opaque/.wh..wh..opq missing")
	}

	// Root="smalldir" walks only that subtree, matching Root=upper on disk.img.
	var subBuf bytes.Buffer
	if _, err := WriteLayer(&subBuf, Options{
		Source: &Ext4Source{Img: img, Root: "smalldir"},
	}); err != nil {
		t.Fatalf("WriteLayer smalldir: %v", err)
	}
	subLayer := readLayer(t, subBuf.Bytes())
	if len(subLayer) != 3 || subLayer["a"].content != "a" || subLayer["bb"].content != "bb" || subLayer["ccc"].content != "ccc" {
		t.Errorf("unexpected smalldir layer: %v", names(subLayer))
	}

	// Missing root reports an error.
	var errBuf bytes.Buffer
	if _, err := WriteLayer(&errBuf, Options{
		Source: &Ext4Source{Img: img, Root: "no-such-upper"},
	}); err == nil {
		t.Error("expected error when Root directory is missing")
	}
}
