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
	"compress/gzip"
	"fmt"
	"io"

	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/compress/zstd"
)

// Compression names the layer encodings devpilot-commit can produce.
const (
	// CompressionNone writes the tar through unchanged. It exists for tests;
	// registries reject an uncompressed layer whose media type claims gzip.
	CompressionNone = "none"
	// CompressionGzip is the universally accepted encoding.
	CompressionGzip = "gzip"
	// CompressionZstd is smaller and about an order of magnitude faster to
	// compress, and is what containerd and Harbor both accept.
	CompressionZstd = "zstd"
)

// layerMediaType reports the OCI media type for a compressed layer. The layer
// format follows the manifest format of the image it is appended to, because a
// Docker schema 2 manifest may only carry docker layer types.
func layerMediaType(compression string, docker bool) (types.MediaType, error) {
	switch compression {
	case CompressionZstd:
		// There is no docker zstd layer type; an OCI layer type inside a docker
		// manifest is rejected, which the caller reports as a base image that
		// cannot take a zstd layer.
		return types.OCILayerZStd, nil
	case CompressionGzip:
		if docker {
			return types.DockerLayer, nil
		}
		return types.OCILayer, nil
	case CompressionNone:
		if docker {
			return types.DockerUncompressedLayer, nil
		}
		return types.OCIUncompressedLayer, nil
	default:
		return "", fmt.Errorf("unknown compression %q", compression)
	}
}

// newCompressor wraps w and returns a closer that flushes the stream. The
// returned writer must be closed before the underlying digest is read.
func newCompressor(w io.Writer, compression string) (io.WriteCloser, error) {
	switch compression {
	case CompressionZstd:
		enc, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			return nil, fmt.Errorf("create zstd encoder: %w", err)
		}
		return enc, nil
	case CompressionGzip:
		return gzip.NewWriter(w), nil
	case CompressionNone:
		return nopWriteCloser{w}, nil
	default:
		return nil, fmt.Errorf("unknown compression %q", compression)
	}
}

// newDecompressor mirrors newCompressor so a staged layer can be re-read, as
// the v1.Layer contract requires.
func newDecompressor(r io.Reader, compression string) (io.ReadCloser, error) {
	switch compression {
	case CompressionZstd:
		dec, err := zstd.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("create zstd decoder: %w", err)
		}
		return dec.IOReadCloser(), nil
	case CompressionGzip:
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("create gzip reader: %w", err)
		}
		return zr, nil
	case CompressionNone:
		return io.NopCloser(r), nil
	default:
		return nil, fmt.Errorf("unknown compression %q", compression)
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
