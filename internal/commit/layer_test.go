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
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"
)

// containerd decompresses the uploaded blob and compares its digest against
// rootfs.diff_ids, so a diffID hashed over the compressed stream is refused at
// pull time with "wrong diff id calculated on extraction". Observed on
// bz1-dev1; nothing here could fail while compression was "none".
func TestStageLayerDiffIDIsTheTarDigest(t *testing.T) {
	upper := writeTree(t, map[string]string{
		testMarker:        testKeepMe,
		"opt/app/main.py": testScript,
	})

	layer, err := stageLayer(Options{UpperDir: upper}, CompressionGzip, t.TempDir())
	if err != nil {
		t.Fatalf("stageLayer: %v", err)
	}
	defer layer.Discard()

	digest, err := layer.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	diffID, err := layer.DiffID()
	if err != nil {
		t.Fatalf("diff id: %v", err)
	}
	if diffID == digest {
		t.Fatalf("diff id %s equals the compressed digest, so it was hashed after compression", diffID)
	}

	blob, err := os.Open(layer.path)
	if err != nil {
		t.Fatalf("open staged blob: %v", err)
	}
	defer func() { _ = blob.Close() }()
	gz, err := gzip.NewReader(blob)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	// This mirrors an image store pulling the layer back.
	raw := sha256.New()
	if _, err := io.Copy(raw, gz); err != nil {
		t.Fatalf("decompress staged blob: %v", err)
	}
	want := hex.EncodeToString(raw.Sum(nil))
	if diffID.Hex != want {
		t.Fatalf("diff id = %s, want the decompressed digest %s", diffID.Hex, want)
	}
}
