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
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"syscall"
	"testing"
	"time"
)

const (
	testMarker    = "root/persistence-marker"
	testKeepMe    = "keep me\n"
	testProfileSh = "etc/profile.d/x.sh"
)

// layerEntry is one tar member with its body decoded as text.
type layerEntry struct {
	header  *tar.Header
	content string
}

// readLayer parses a finished layer. Duplicate names are impossible because a
// tar with repeated members would already be a bug worth failing on.
func readLayer(t *testing.T, data []byte) map[string]layerEntry {
	t.Helper()
	out := make(map[string]layerEntry)
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read layer: %v", err)
		}
		if _, dup := out[hdr.Name]; dup {
			t.Fatalf("layer contains %s twice", hdr.Name)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read body of %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = layerEntry{header: hdr, content: string(body)}
	}
	return out
}

// names returns the layer members in lexical order.
func names(layer map[string]layerEntry) []string {
	out := make([]string, 0, len(layer))
	for name := range layer {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// writeTree materializes slash separated paths below a fresh directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("create parent of %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

func TestWriteLayerPackagesFilesAndDirectories(t *testing.T) {
	upper := writeTree(t, map[string]string{
		testMarker:         testKeepMe,
		"opt/app/train.py": "print('hi')\n",
		testProfileSh:      "export A=1\n",
	})

	var buf bytes.Buffer
	stats, err := WriteLayer(&buf, Options{UpperDir: upper})
	if err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}

	layer := readLayer(t, buf.Bytes())
	want := []string{
		"etc/", "etc/profile.d/", testProfileSh,
		"opt/", "opt/app/", "opt/app/train.py",
		"root/", testMarker,
	}
	if got := names(layer); !reflect.DeepEqual(got, want) {
		t.Fatalf("layer members = %v, want %v", got, want)
	}

	for _, dir := range []string{"etc/", "opt/", "root/"} {
		if layer[dir].header.Typeflag != tar.TypeDir {
			t.Errorf("%s typeflag = %v, want TypeDir", dir, layer[dir].header.Typeflag)
		}
	}
	if got := layer["opt/app/train.py"].content; got != "print('hi')\n" {
		t.Errorf("train.py content = %q", got)
	}
	if got := layer["opt/app/train.py"].header.Typeflag; got != tar.TypeReg {
		t.Errorf("train.py typeflag = %v, want TypeReg", got)
	}
	if stats.Files != 3 || stats.Whiteouts != 0 || stats.OpaqueDirs != 0 || stats.Hardlinks != 0 {
		t.Errorf("stats = %+v, want 3 files and nothing else", stats)
	}
}

func TestWriteLayerRecordsSymlinks(t *testing.T) {
	upper := t.TempDir()
	if err := os.Symlink("/usr/local/cuda", filepath.Join(upper, "cuda-link")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	var buf bytes.Buffer
	if _, err := WriteLayer(&buf, Options{UpperDir: upper}); err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}

	entry, ok := readLayer(t, buf.Bytes())["cuda-link"]
	if !ok {
		t.Fatalf("symlink missing from layer: %v", names(readLayer(t, buf.Bytes())))
	}
	if entry.header.Typeflag != tar.TypeSymlink {
		t.Errorf("typeflag = %v, want TypeSymlink", entry.header.Typeflag)
	}
	if entry.header.Linkname != "/usr/local/cuda" {
		t.Errorf("linkname = %q, want /usr/local/cuda", entry.header.Linkname)
	}
}

func TestWriteLayerEmitsOpaqueMarker(t *testing.T) {
	upper := writeTree(t, map[string]string{"workspace/notes.txt": "new\n"})

	var buf bytes.Buffer
	stats, err := WriteLayer(&buf, Options{
		UpperDir:   upper,
		OpaqueDirs: []string{"./workspace"},
	})
	if err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}

	layer := readLayer(t, buf.Bytes())
	marker, ok := layer["workspace/"+OpaqueMarker]
	if !ok {
		t.Fatalf("opaque marker missing: %v", names(layer))
	}
	if marker.header.Typeflag != tar.TypeReg || marker.header.Size != 0 {
		t.Errorf("marker header = %+v, want an empty regular file", marker.header)
	}
	if marker.header.Mode != int64(whiteoutMode) {
		t.Errorf("marker mode = %o, want %o", marker.header.Mode, whiteoutMode)
	}
	if stats.OpaqueDirs != 1 {
		t.Errorf("OpaqueDirs = %d, want 1", stats.OpaqueDirs)
	}
}

func TestWriteLayerAppliesDefaultExcludes(t *testing.T) {
	upper := writeTree(t, map[string]string{
		"tmp/scratch.log": "noise\n",
		"etc/hosts":       "10.0.0.1\n",
		testProfileSh:     "export A=1\n",
		testMarker:        testKeepMe,
	})

	var buf bytes.Buffer
	stats, err := WriteLayer(&buf, Options{UpperDir: upper})
	if err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}

	layer := readLayer(t, buf.Bytes())
	for _, gone := range []string{"tmp/", "tmp/scratch.log", "etc/hosts"} {
		if _, ok := layer[gone]; ok {
			t.Errorf("%s should have been excluded", gone)
		}
	}
	for _, kept := range []string{testMarker, testProfileSh} {
		if _, ok := layer[kept]; !ok {
			t.Errorf("%s should have been kept", kept)
		}
	}
	if stats.Skipped == 0 {
		t.Error("Skipped = 0, want the excluded paths counted")
	}
}

func TestWriteLayerCleanPackageCache(t *testing.T) {
	upper := writeTree(t, map[string]string{
		"root/.cache/pip/http-v2/abc": "wheel\n",
		testMarker:                    testKeepMe,
	})

	var kept bytes.Buffer
	if _, err := WriteLayer(&kept, Options{UpperDir: upper}); err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}
	if _, ok := readLayer(t, kept.Bytes())["root/.cache/pip/http-v2/abc"]; !ok {
		t.Error("pip cache should be kept unless CleanPackageCache is set")
	}

	var cleaned bytes.Buffer
	if _, err := WriteLayer(&cleaned, Options{UpperDir: upper, CleanPackageCache: true}); err != nil {
		t.Fatalf("WriteLayer: %v", err)
	}
	layer := readLayer(t, cleaned.Bytes())
	if _, ok := layer["root/.cache/pip/http-v2/abc"]; ok {
		t.Error("pip cache should be dropped by CleanPackageCache")
	}
	if _, ok := layer[testMarker]; !ok {
		t.Error("cleanPackageCache must not touch unrelated files")
	}
}

func TestWriteLayerIsDeterministic(t *testing.T) {
	upper := writeTree(t, map[string]string{
		"a/one.txt": "1\n",
		"b/two.txt": "2\n",
		testMarker:  "3\n",
	})

	var first, second bytes.Buffer
	for _, buf := range []*bytes.Buffer{&first, &second} {
		if _, err := WriteLayer(buf, Options{UpperDir: upper}); err != nil {
			t.Fatalf("WriteLayer: %v", err)
		}
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Error("two scans of an unchanged disk produced different layers")
	}
}

func TestWriteLayerRejectsBadInput(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cases := map[string]Options{
		"missing upper dir":   {UpperDir: filepath.Join(t.TempDir(), "absent")},
		"upper dir is a file": {UpperDir: file},
		"empty upper dir":     {},
		"bad exclude":         {UpperDir: t.TempDir(), ExcludePaths: []string{"[bad"}},
		"escaping opaque dir": {UpperDir: t.TempDir(), OpaqueDirs: []string{"../etc"}},
		"absolute opaque dir": {UpperDir: t.TempDir(), OpaqueDirs: []string{"/etc"}},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := WriteLayer(io.Discard, opts); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// fakeInfo is a FileInfo with a synthetic stat, so the whiteout branch can be
// exercised without creating a device node.
type fakeInfo struct {
	mode fs.FileMode
	sys  any
}

func (f fakeInfo) Name() string       { return "fake" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return f.sys }

func TestWriteEntryConvertsOverlayDeletionToWhiteout(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	var stats Stats
	// abs does not exist: the conversion must not open the "file".
	err := writeEntry(tw, entry{
		rel:  "opt/nvidia/entrypoint.d/12-banner.sh",
		abs:  filepath.Join(t.TempDir(), "absent"),
		info: fakeInfo{mode: fs.ModeCharDevice | 0o600, sys: &syscall.Stat_t{}},
	}, nil, map[uint64]string{}, &stats)
	if err != nil {
		t.Fatalf("writeEntry: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	layer := readLayer(t, buf.Bytes())
	want := "opt/nvidia/entrypoint.d/" + WhiteoutPrefix + "12-banner.sh"
	whiteout, ok := layer[want]
	if !ok {
		t.Fatalf("whiteout %s missing: %v", want, names(layer))
	}
	if whiteout.header.Typeflag != tar.TypeReg || whiteout.header.Size != 0 {
		t.Errorf("whiteout header = %+v, want an empty regular file", whiteout.header)
	}
	if stats.Whiteouts != 1 || stats.Files != 0 {
		t.Errorf("stats = %+v, want one whiteout and no files", stats)
	}
}

func TestWriteEntryLeavesRealDevicesAndPipesOut(t *testing.T) {
	upper := t.TempDir()
	cases := map[string]fs.FileMode{
		"real device": fs.ModeCharDevice | 0o600,
		"fifo":        fs.ModeNamedPipe | 0o600,
		"socket":      fs.ModeSocket | 0o600,
	}
	for name, mode := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			var stats Stats
			// A non-zero rdev means a real device node, not a deletion.
			err := writeEntry(tw, entry{
				rel:  name,
				abs:  filepath.Join(upper, name),
				info: fakeInfo{mode: mode, sys: &syscall.Stat_t{Rdev: 1}},
			}, nil, map[uint64]string{}, &stats)
			if err != nil {
				t.Fatalf("writeEntry: %v", err)
			}
			if err := tw.Close(); err != nil {
				t.Fatalf("close tar: %v", err)
			}
			if got := names(readLayer(t, buf.Bytes())); len(got) != 0 {
				t.Errorf("layer members = %v, want none", got)
			}
			if stats != (Stats{}) {
				t.Errorf("stats = %+v, want zero", stats)
			}
		})
	}
}

func TestNormalizeOpaqueDirs(t *testing.T) {
	got, err := normalizeOpaqueDirs([]string{"./var/lib/app", "opt", ".", "", "var/lib/app/"})
	if err != nil {
		t.Fatalf("normalizeOpaqueDirs: %v", err)
	}
	want := map[string]struct{}{"var/lib/app": {}, "opt": {}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("normalizeOpaqueDirs = %v, want %v", got, want)
	}
}

func TestCompileExcludesMatchesSubtreePatterns(t *testing.T) {
	rules, err := compileExcludes([]string{"workspace/scratch/*", "data"}, false)
	if err != nil {
		t.Fatalf("compileExcludes: %v", err)
	}

	excluded := []string{"tmp", "tmp/a/b", "workspace/scratch", "workspace/scratch/x.log", "data", "etc/ssh/ssh_host_ed25519_key"}
	kept := []string{"tmpfile", "workspace/scratchpad/x", testProfileSh, "data2"}
	for _, p := range excluded {
		if !excludesMatched(rules, p) {
			t.Errorf("%s should match an exclude rule", p)
		}
	}
	for _, p := range kept {
		if excludesMatched(rules, p) {
			t.Errorf("%s should not match any exclude rule", p)
		}
	}
}
