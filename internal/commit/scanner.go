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

// Package commit turns the OverlayFS upper/ directory of a DevBox root disk
// into an OCI image layer.
//
// upper/ is already a file system diff against the base image, but OverlayFS
// records deletions and directory replacement in ways the OCI layer format does
// not understand: a deleted file is a 0:0 character device and a replaced
// directory carries the overlay.opaque xattr. The scanner rewrites both into
// whiteout entries before anything reaches the tar stream.
package commit

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// WhiteoutPrefix marks a deleted entry in an OCI layer, as in .wh.<name>.
const WhiteoutPrefix = ".wh."

// OpaqueMarker replaces every lower entry of its directory, as in
// .wh..wh..opq. The name is what containerd and docker unpack; the kernel
// itself uses the overlay.opaque xattr.
const OpaqueMarker = ".wh..wh..opq"

// whiteoutMode is what the runtime expects of a whiteout entry.
const whiteoutMode fs.FileMode = 0o600

// Options configures one scan.
type Options struct {
	// UpperDir is the directory to package, normally <root>/upper. It is
	// ignored when Source is set.
	UpperDir string
	// Source overrides UpperDir, e.g. with an ext4 image source.
	Source Source
	// OpaqueDirs lists directories whose lower content must be hidden. It is
	// the snapshot devpilot-init wrote while it still held CAP_SYS_ADMIN; an
	// unprivileged process cannot read trusted.overlay.opaque itself.
	OpaqueDirs []string
	// ExcludePaths are extra globs to skip, relative to the layer root.
	ExcludePaths []string
	// CleanPackageCache drops downloaded package archives and pip caches.
	CleanPackageCache bool
}

// Stats summarizes what the scan wrote.
type Stats struct {
	// Files is the number of regular entries and symlinks written.
	Files int `json:"files"`
	// Whiteouts is the number of deleted entries represented.
	Whiteouts int `json:"whiteouts"`
	// OpaqueDirs is the number of directory replacements represented.
	OpaqueDirs int `json:"opaqueDirs"`
	// Hardlinks is the number of entries written as links to an earlier path.
	Hardlinks int `json:"hardlinks"`
	// Skipped is the number of paths dropped by an exclude rule.
	Skipped int `json:"skipped"`
	// UncompressedBytes is the sum of regular file bytes packaged into the layer.
	UncompressedBytes int64 `json:"uncompressedBytes,omitempty"`
	// ModelCacheBytes is the sum of regular file bytes detected under common
	// ML model cache directories (HuggingFace, ModelScope, Torch Hub).
	ModelCacheBytes int64 `json:"modelCacheBytes,omitempty"`
}

// WriteLayer streams upper/ as an uncompressed OCI layer into w.
//
// Entries are sorted by path so repeated commits of an unchanged disk produce
// the same tar, and every entry is normalized to root ownership like image
// layers conventionally are.
func WriteLayer(w io.Writer, opts Options) (Stats, error) {
	stats, _, err := writeLayer(w, opts)
	return stats, err
}

func writeLayer(w io.Writer, opts Options) (Stats, []string, error) {
	var stats Stats
	src := opts.Source
	if src == nil {
		if opts.UpperDir == "" {
			return stats, nil, errors.New("upper dir is required")
		}
		dir := DirSource{Root: opts.UpperDir}
		if err := dir.Check(); err != nil {
			return stats, nil, err
		}
		src = dir
	}

	excludes, err := compileExcludes(opts.ExcludePaths, opts.CleanPackageCache)
	if err != nil {
		return stats, nil, err
	}
	opaque, err := normalizeOpaqueDirs(opts.OpaqueDirs)
	if err != nil {
		return stats, nil, err
	}

	entries, err := collectEntries(src, excludes)
	if err != nil {
		return stats, nil, err
	}
	stats.Skipped = len(entries.skipped)

	tw := tar.NewWriter(w)
	// Close runs twice by design: the explicit call below reports a write error
	// the caller can act on, while this one still terminates the archive when
	// a writeEntry error returns early. tar.Writer treats the second Close as a
	// no-op, so its error carries no information.
	defer func() { _ = tw.Close() }()

	hardlinks := make(map[uint64]string)
	var keptFiles []string
	for _, e := range entries.kept {
		if err := writeEntry(tw, src, e, opaque, hardlinks, &stats); err != nil {
			return stats, nil, err
		}
		if e.info.Mode().IsRegular() {
			keptFiles = append(keptFiles, e.rel)
		}
	}
	if err := tw.Close(); err != nil {
		return stats, nil, fmt.Errorf("close tar stream: %w", err)
	}
	return stats, keptFiles, nil
}

// entry is one path discovered in upper/, in walk (lexical) order.
type entry struct {
	// rel is the slash separated path below upper/.
	rel  string
	info fs.FileInfo
}

type collected struct {
	kept    []entry
	skipped []string
}

// collectEntries walks upper/ once, keeping only what the layer should carry.
// It never follows symlinks: a symlink out of upper/ is recorded as a link, and
// its target is irrelevant to the layer.
func collectEntries(src Source, excludes []excludeRule) (collected, error) {
	var out collected
	err := src.Walk(func(rel string, info fs.FileInfo) error {
		if excludesMatched(excludes, rel) {
			out.skipped = append(out.skipped, rel)
			if info.IsDir() {
				// Skipping a directory means skipping everything below it.
				return fs.SkipDir
			}
			return nil
		}
		out.kept = append(out.kept, entry{rel: rel, info: info})
		return nil
	})
	return out, err
}

// writeEntry renders one discovered path: a whiteout, an opaque marker, a
// hardlink, or the file itself.
func writeEntry(tw *tar.Writer, src Source, e entry, opaque map[string]struct{}, hardlinks map[uint64]string, stats *Stats) error {
	mode := e.info.Mode()

	// A 0:0 character device is OverlayFS's way of recording a deletion.
	// Writing it through would make the layer carry a bogus device node, and
	// unpackers without CAP_MKNOD would fail.
	if src.IsWhiteout(e.info) {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     path.Join(path.Dir(e.rel), WhiteoutPrefix+path.Base(e.rel)),
			Mode:     int64(whiteoutMode),
			Size:     0,
			ModTime:  e.info.ModTime(),
			Format:   tar.FormatPAX,
		}); err != nil {
			return fmt.Errorf("write whiteout %s: %w", e.rel, err)
		}
		stats.Whiteouts++
		return nil
	}

	if mode.IsDir() {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     e.rel + "/",
			Mode:     int64(mode.Perm()),
			ModTime:  e.info.ModTime(),
			Format:   tar.FormatPAX,
		}); err != nil {
			return fmt.Errorf("write directory %s: %w", e.rel, err)
		}
		if _, ok := opaque[e.rel]; ok || src.Opaque(e.rel, e.info) {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg,
				Name:     path.Join(e.rel, OpaqueMarker),
				Mode:     int64(whiteoutMode),
				Size:     0,
				ModTime:  e.info.ModTime(),
				Format:   tar.FormatPAX,
			}); err != nil {
				return fmt.Errorf("write opaque marker %s: %w", e.rel, err)
			}
			stats.OpaqueDirs++
		}
		return nil
	}

	switch {
	case mode&fs.ModeSymlink != 0:
		target, err := src.Readlink(e.rel, e.info)
		if err != nil {
			return fmt.Errorf("read symlink %s: %w", e.rel, err)
		}
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeSymlink,
			Name:     e.rel,
			Linkname: target,
			Mode:     int64(mode.Perm()),
			ModTime:  e.info.ModTime(),
			Format:   tar.FormatPAX,
		}); err != nil {
			return fmt.Errorf("write symlink %s: %w", e.rel, err)
		}
		stats.Files++
		return nil
	case mode.IsRegular():
		return writeRegular(tw, src, e, hardlinks, stats)
	default:
		// FIFOs and sockets have no meaning in an image layer and cannot be
		// created by an unpacker without privileges.
		return nil
	}
}

// writeRegular writes a file, or a link when an earlier path already carried
// the same inode. Conda and npm install trees are full of hardlinks.
func writeRegular(tw *tar.Writer, src Source, e entry, hardlinks map[uint64]string, stats *Stats) error {
	if ino, ok := src.Inode(e.info); ok {
		if first, seen := hardlinks[ino]; seen {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeLink,
				Name:     e.rel,
				Linkname: first,
				Mode:     int64(e.info.Mode().Perm()),
				ModTime:  e.info.ModTime(),
				Format:   tar.FormatPAX,
			}); err != nil {
				return fmt.Errorf("write hardlink %s: %w", e.rel, err)
			}
			stats.Hardlinks++
			return nil
		}
		hardlinks[ino] = e.rel
	}

	f, err := src.Open(e.rel, e.info)
	if err != nil {
		return fmt.Errorf("open %s: %w", e.rel, err)
	}
	// A read-only close has nothing to flush, and returning it would mask the
	// copy error that already describes the failure.
	defer func() { _ = f.Close() }()

	if err := tw.WriteHeader(&tar.Header{
		Typeflag:   tar.TypeReg,
		Name:       e.rel,
		Mode:       int64(e.info.Mode().Perm()),
		Size:       e.info.Size(),
		ModTime:    e.info.ModTime(),
		AccessTime: e.info.ModTime(),
		ChangeTime: e.info.ModTime(),
		Format:     tar.FormatPAX,
	}); err != nil {
		return fmt.Errorf("write header %s: %w", e.rel, err)
	}
	if _, err := io.CopyN(tw, f, e.info.Size()); err != nil {
		return fmt.Errorf("write content of %s: %w", e.rel, err)
	}
	stats.Files++
	stats.UncompressedBytes += e.info.Size()
	if isModelCachePath(e.rel) {
		stats.ModelCacheBytes += e.info.Size()
	}
	return nil
}

// isModelCachePath reports whether rel sits inside a common ML model weight
// cache directory that is normally better kept on a shared PVC or filesystem.
func isModelCachePath(rel string) bool {
	if cacheRel, ok := strings.CutPrefix(rel, "root/.cache/"); ok {
		return isModelCacheSuffix(cacheRel)
	}
	if rest, ok := strings.CutPrefix(rel, "home/"); ok {
		if _, afterUser, found := strings.Cut(rest, "/"); found {
			if cacheRel, ok := strings.CutPrefix(afterUser, ".cache/"); ok {
				return isModelCacheSuffix(cacheRel)
			}
		}
	}
	return false
}

func isModelCacheSuffix(cacheRel string) bool {
	return strings.HasPrefix(cacheRel, "huggingface/") ||
		strings.HasPrefix(cacheRel, "modelscope/") ||
		strings.HasPrefix(cacheRel, "torch/hub/")
}

// prioritizedLandmarkFiles picks regular files in the layer that are read
// during container startup (sshd, python/conda binaries, torch imports, CUDA
// shared libraries) so zstd:chunked places them before the prefetch landmark.
func prioritizedLandmarkFiles(paths []string) []string {
	var prioritized []string
	for _, p := range paths {
		if isLandmarkFile(p) {
			prioritized = append(prioritized, p)
		}
	}
	return prioritized
}

func isLandmarkFile(rel string) bool {
	switch {
	case strings.HasPrefix(rel, "etc/"),
		strings.HasPrefix(rel, "usr/bin/"),
		rel == "usr/sbin/sshd",
		strings.HasPrefix(rel, "opt/conda/bin/"):
		return true
	case (strings.HasPrefix(rel, "opt/conda/lib/python") ||
		strings.HasPrefix(rel, "usr/local/lib/python") ||
		strings.HasPrefix(rel, "usr/lib/python")) &&
		(strings.Contains(rel, "/site-packages/torch/") ||
			strings.Contains(rel, "/dist-packages/torch/")):
		return true
	case strings.Contains(rel, "libcudart") ||
		strings.Contains(rel, "libcublas") ||
		strings.Contains(rel, "libcudnn") ||
		strings.Contains(rel, "libnccl"):
		return true
	default:
		return false
	}
}

// isZeroDevice reports whether a character device is OverlayFS's deletion
// marker: major and minor are both zero.
//
// The raw device number is compared rather than its decoded halves, because a
// value of 0 decodes to 0:0 under Linux's and Darwin's differing encodings
// alike, and that keeps the check portable.
func isZeroDevice(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	// Rdev is uint64 on Linux and int32 on Darwin; comparing to the untyped
	// constant compiles for both.
	return stat.Rdev == 0
}

// normalizeOpaqueDirs validates the snapshot and returns it as a set of clean
// slash separated paths. A path that escapes the layer is rejected rather than
// silently ignored.
func normalizeOpaqueDirs(dirs []string) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		clean := path.Clean(strings.TrimPrefix(filepath.ToSlash(dir), "./"))
		if clean == "." || clean == "" {
			continue
		}
		if clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
			return nil, fmt.Errorf("opaque dir %q escapes the layer", dir)
		}
		out[clean] = struct{}{}
	}
	return out, nil
}
