//go:build linux

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
	"os"
	"path/filepath"
	"testing"
)

// Hardlink dedupe needs a real inode number, which only the Linux build
// reports; the portable fallback deliberately reports none.
func TestWriteLayerDedupesHardlinks(t *testing.T) {
	upper := t.TempDir()
	first := filepath.Join(upper, "opt", "conda", "bin", "python")
	if err := os.MkdirAll(filepath.Dir(first), 0o755); err != nil {
		t.Fatalf("create dir: %v", err)
	}
	if err := os.WriteFile(first, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write first: %v", err)
	}
	second := filepath.Join(upper, "opt", "conda", "bin", "python3")
	if err := os.Link(first, second); err != nil {
		t.Fatalf("create hardlink: %v", err)
	}

	var buf bytes.Buffer
	stats, err := WriteLayer(&buf, Options{UpperDir: upper})
	if err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}

	layer := readLayer(t, buf.Bytes())
	const firstRel = "opt/conda/bin/python"
	const secondRel = "opt/conda/bin/python3"
	if layer[firstRel].header.Typeflag != tar.TypeReg {
		t.Errorf("%s typeflag = %v, want TypeReg", firstRel, layer[firstRel].header.Typeflag)
	}
	link, ok := layer[secondRel]
	if !ok {
		t.Fatalf("%s missing: %v", secondRel, names(layer))
	}
	if link.header.Typeflag != tar.TypeLink {
		t.Errorf("%s typeflag = %v, want TypeLink", secondRel, link.header.Typeflag)
	}
	if link.header.Linkname != firstRel {
		t.Errorf("%s linkname = %q, want %q", secondRel, link.header.Linkname, firstRel)
	}
	if stats.Hardlinks != 1 || stats.Files != 1 {
		t.Errorf("stats = %+v, want one hardlink and one file", stats)
	}
}
