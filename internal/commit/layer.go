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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"

	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/estargz/zstdchunked"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/compress/zstd"
)

// stagedLayer is a finished layer blob on local scratch disk.
//
// The compressed bytes are staged rather than streamed straight to the
// registry because a v1.Layer must answer Digest, DiffID and Size before the
// upload starts, and Size has to be exact for the manifest. Staging keeps
// memory flat at the cost of one scratch file the size of the compressed layer,
// which on a build node with a local NVMe disk is far cheaper than buffering a
// multi-gigabyte layer in RAM or scanning upper/ three times.
type stagedLayer struct {
	path        string
	compression string
	mediaType   types.MediaType
	digest      v1.Hash
	diffID      v1.Hash
	size        int64
	annotations map[string]string
	stats       Stats
}

var _ v1.Layer = (*stagedLayer)(nil)

// stageLayer scans upper/ once and writes the compressed result to scratchDir.
// The caller owns the file and must call Discard when finished.
func stageLayer(opts Options, compression string, scratchDir string) (*stagedLayer, error) {
	media, err := layerMediaType(compression, false)
	if err != nil {
		return nil, err
	}
	if scratchDir == "" {
		scratchDir = os.TempDir()
	}
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		return nil, fmt.Errorf("create scratch dir: %w", err)
	}
	if compression == CompressionZstdChunked {
		return stageZstdChunkedLayer(opts, media, scratchDir)
	}

	file, err := os.CreateTemp(scratchDir, "devpilot-layer-*.tar")
	if err != nil {
		return nil, fmt.Errorf("create scratch file: %w", err)
	}
	path := file.Name()
	staged := &stagedLayer{path: path, compression: compression, mediaType: media}

	uncompressedHash := sha256.New()
	compressedHash := sha256.New()
	counter := &countWriter{}
	// The diffID is the digest of the tar, so the hash must sit before the
	// compressor: bytes flow through WriteLayer into the uncompressed digest,
	// then the compressor, then the compressed digest, then the file.
	sink := io.MultiWriter(newDigestCounter(compressedHash, counter), file)
	compressor, err := newCompressor(sink, compression)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}

	stats, err := WriteLayer(io.MultiWriter(uncompressedHash, compressor), opts)
	if err != nil {
		_ = compressor.Close()
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := compressor.Close(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("flush compressed layer: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("close scratch file: %w", err)
	}

	staged.stats = stats
	staged.digest = hashOf(compressedHash)
	staged.diffID = hashOf(uncompressedHash)
	staged.size = counter.n
	return staged, nil
}

// stageZstdChunkedLayer writes the raw tar stream to scratch, then builds a
// seekable zstd:chunked OCI blob with an embedded TOC and landmark priorities.
func stageZstdChunkedLayer(opts Options, media types.MediaType, scratchDir string) (*stagedLayer, error) {
	rawFile, err := os.CreateTemp(scratchDir, "devpilot-raw-*.tar")
	if err != nil {
		return nil, fmt.Errorf("create raw tar scratch file: %w", err)
	}
	rawPath := rawFile.Name()
	defer func() {
		_ = rawFile.Close()
		_ = os.Remove(rawPath)
	}()

	stats, keptFiles, err := writeLayer(rawFile, opts)
	if err != nil {
		return nil, err
	}
	rawInfo, err := rawFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat raw tar scratch file: %w", err)
	}

	metadata := make(map[string]string)
	zc := struct {
		*zstdchunked.Compressor
		*zstdchunked.Decompressor
	}{
		Compressor: &zstdchunked.Compressor{
			CompressionLevel: zstd.SpeedDefault,
			Metadata:         metadata,
		},
		Decompressor: &zstdchunked.Decompressor{},
	}
	prioritized := prioritizedLandmarkFiles(keptFiles)
	var missed []string
	buildOpts := []estargz.Option{
		estargz.WithCompression(zc),
		estargz.WithAllowPrioritizeNotFound(&missed),
	}
	if len(prioritized) > 0 {
		buildOpts = append(buildOpts, estargz.WithPrioritizedFiles(prioritized))
	}
	blob, err := estargz.Build(io.NewSectionReader(rawFile, 0, rawInfo.Size()), buildOpts...)
	if err != nil {
		return nil, fmt.Errorf("build zstd:chunked layer: %w", err)
	}
	defer func() { _ = blob.Close() }()

	file, err := os.CreateTemp(scratchDir, "devpilot-layer-*.tar")
	if err != nil {
		return nil, fmt.Errorf("create scratch file: %w", err)
	}
	path := file.Name()

	compressedHash := sha256.New()
	counter := &countWriter{}
	if _, err := io.Copy(io.MultiWriter(newDigestCounter(compressedHash, counter), file), blob); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("write zstd:chunked layer: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("close scratch file: %w", err)
	}

	diffDigest := blob.DiffID()
	diffID, err := v1.NewHash(diffDigest.String())
	if err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("parse zstd:chunked diff id %q: %w", diffDigest.String(), err)
	}

	annotations := map[string]string{
		estargz.TOCJSONDigestAnnotation:         blob.TOCDigest().String(),
		estargz.StoreUncompressedSizeAnnotation: strconv.FormatInt(rawInfo.Size(), 10),
	}
	maps.Copy(annotations, metadata)

	return &stagedLayer{
		path:        path,
		compression: CompressionZstdChunked,
		mediaType:   media,
		digest:      hashOf(compressedHash),
		diffID:      diffID,
		size:        counter.n,
		annotations: annotations,
		stats:       stats,
	}, nil
}

// Compressed returns the staged blob for upload.
func (l *stagedLayer) Compressed() (io.ReadCloser, error) {
	return os.Open(l.path)
}

// Uncompressed decodes the staged blob. The registry client uses it to verify
// the diffID after an upload, so it must work repeatedly.
func (l *stagedLayer) Uncompressed() (io.ReadCloser, error) {
	f, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	dec, err := newDecompressor(f, l.compression)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return readCloser{Reader: dec, closers: []io.Closer{dec, f}}, nil
}

func (l *stagedLayer) Digest() (v1.Hash, error) { return l.digest, nil }

func (l *stagedLayer) DiffID() (v1.Hash, error) { return l.diffID, nil }

func (l *stagedLayer) Size() (int64, error) { return l.size, nil }

func (l *stagedLayer) MediaType() (types.MediaType, error) { return l.mediaType, nil }

// Annotations returns OCI descriptor annotations (populated for zstd:chunked).
func (l *stagedLayer) Annotations() map[string]string { return l.annotations }

// Discard removes the scratch blob. It is safe to call more than once.
func (l *stagedLayer) Discard() { _ = os.Remove(l.path) }

// Stats returns the scan summary for status reporting.
func (l *stagedLayer) Stats() Stats { return l.stats }

// String is used by the CLI to report what was staged.
func (l *stagedLayer) String() string {
	return fmt.Sprintf("%s (%s, %d bytes)", filepath.Base(l.path), l.compression, l.size)
}

// readCloser closes every wrapped reader, so a decompressor's own resources are
// released along with the file it reads.
type readCloser struct {
	io.Reader
	closers []io.Closer
}

func (r readCloser) Close() error {
	var first error
	for _, c := range r.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// countWriter records how many bytes passed through it.
type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// digestCounter feeds every write into h as well as into the next writer in the
// chain, so one pass both hashes and stores the compressed bytes.
func newDigestCounter(h hash.Hash, next io.Writer) io.Writer {
	return io.MultiWriter(h, next)
}

// hashOf renders a finished sha256 as a registry hash.
func hashOf(h hash.Hash) v1.Hash {
	return v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(h.Sum(nil))}
}
