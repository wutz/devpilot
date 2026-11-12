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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// fakeRegistry speaks just enough of the OCI distribution upload protocol to
// exercise the chunked uploader: sessions, ranged PATCHes, GET for the offset
// and the digest-checked PUT.
type fakeRegistry struct {
	mu sync.Mutex
	// repositories to uploaded blobs, keyed by digest.
	blobs map[string][]byte
	// sessions to the bytes received so far.
	sessions map[string][]byte
	nextID   int
	// patches counts PATCH requests, including the ones failed on purpose.
	patches int
	// failPATCH makes this PATCH (1-based) answer 500 after taking half its
	// body, which is what a cut connection looks like to the client.
	failPATCH int
	// ranges records the Content-Range header of every PATCH.
	ranges []string
	// maxChunk records the largest body any single PATCH carried.
	maxChunk int64
	// noRangeHeader hides the offset from GET, as a registry that cannot
	// resume would.
	noRangeHeader bool
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{blobs: map[string][]byte{}, sessions: map[string][]byte{}}
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && (path == "/v2/" || path == "/v2"):
		// The transport pings this before its first upload.
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodHead && strings.HasPrefix(path, "/v2/") && strings.Contains(path, "/blobs/"):
		digest := path[strings.LastIndex(path, "/")+1:]
		if _, ok := f.blobs[digest]; ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/blobs/uploads/"):
		f.nextID++
		id := strconv.Itoa(f.nextID)
		f.sessions[id] = nil
		w.Header().Set("Location", "/v2/repo/blobs/uploads/"+id)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPatch && strings.Contains(path, "/blobs/uploads/"):
		f.patches++
		id := path[strings.LastIndex(path, "/")+1:]
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.ranges = append(f.ranges, r.Header.Get("Content-Range"))
		if int64(len(body)) > f.maxChunk {
			f.maxChunk = int64(len(body))
		}
		if f.failPATCH == f.patches {
			// Keep half of what arrived, then drop the connection's answer.
			f.sessions[id] = append(f.sessions[id], body[:len(body)/2]...)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.sessions[id] = append(f.sessions[id], body...)
		w.Header().Set("Location", "/v2/repo/blobs/uploads/"+id)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodGet && strings.Contains(path, "/blobs/uploads/"):
		id := path[strings.LastIndex(path, "/")+1:]
		held := f.sessions[id]
		if !f.noRangeHeader && len(held) > 0 {
			w.Header().Set("Range", fmt.Sprintf("0-%d", len(held)-1))
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut && strings.Contains(path, "/blobs/uploads/"):
		id := path[strings.LastIndex(path, "/")+1:]
		want := r.URL.Query().Get("digest")
		got := sha256.Sum256(f.sessions[id])
		if want != "sha256:"+hex.EncodeToString(got[:]) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.blobs[want] = f.sessions[id]
		delete(f.sessions, id)
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// layerOf serves arbitrary bytes as a compressed layer.
func layerOf(t *testing.T, data []byte) v1.Layer {
	t.Helper()
	return static.NewLayer(data, types.OCILayer)
}

// uploaderFor builds a chunked uploader with the given chunk size.
func uploaderFor(chunk int64) *RegistryUploader {
	return &RegistryUploader{
		ChunkSize:   chunk,
		NameOptions: []name.Option{name.Insecure},
	}
}

func TestUploadSplitsTheLayerIntoBoundedRequests(t *testing.T) {
	fake := newFakeRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()

	data := []byte(strings.Repeat("devpilot", 1000))
	layer := layerOf(t, data)
	uploader := uploaderFor(1024)

	if err := uploader.Upload(context.Background(), server.Listener.Addr().String()+"/repo", layer); err != nil {
		t.Fatalf("upload: %v", err)
	}
	digest, _ := layer.Digest()
	stored, ok := fake.blobs[digest.String()]
	if !ok {
		t.Fatalf("blob %s not stored; stored: %v", digest, len(fake.blobs))
	}
	if string(stored) != string(data) {
		t.Fatalf("stored %d bytes, want %d", len(stored), len(data))
	}
	if fake.maxChunk > 1024 {
		t.Fatalf("a PATCH carried %d bytes, want at most 1024", fake.maxChunk)
	}
	if want := (len(data) + 1023) / 1024; fake.patches != want {
		t.Fatalf("patches = %d, want %d", fake.patches, want)
	}
	if fake.ranges[0] != "0-1023" {
		t.Fatalf("first range = %q", fake.ranges[0])
	}
	if last := fake.ranges[len(fake.ranges)-1]; !strings.HasSuffix(last, fmt.Sprintf("-%d", len(data)-1)) {
		t.Fatalf("last range = %q, want it to end at %d", last, len(data)-1)
	}
}

func TestUploadResumesFromTheCheckpointAfterAFailedChunk(t *testing.T) {
	fake := newFakeRegistry()
	fake.failPATCH = 2
	server := httptest.NewServer(fake)
	defer server.Close()

	data := []byte(strings.Repeat("x", 4096))
	layer := layerOf(t, data)
	uploader := uploaderFor(1024)

	if err := uploader.Upload(context.Background(), server.Listener.Addr().String()+"/repo", layer); err != nil {
		t.Fatalf("upload: %v", err)
	}
	digest, _ := layer.Digest()
	stored := fake.blobs[digest.String()]
	if string(stored) != string(data) {
		t.Fatalf("resumed upload stored %d bytes, want %d", len(stored), len(data))
	}
	// The second chunk (1024-2047) kept its first 512 bytes, so the retry must
	// resume at 1536 rather than resending from the start of the layer.
	if got, want := fake.ranges[2], "1536-2047"; got != want {
		t.Fatalf("retry range = %q, want %q", got, want)
	}
}

func TestUploadSkipsABlobTheRegistryAlreadyHas(t *testing.T) {
	fake := newFakeRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()

	data := []byte("already there")
	layer := layerOf(t, data)
	digest, _ := layer.Digest()
	fake.blobs[digest.String()] = data

	uploader := uploaderFor(1024)
	if err := uploader.Upload(context.Background(), server.Listener.Addr().String()+"/repo", layer); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if fake.patches != 0 {
		t.Fatalf("patches = %d, want 0 for an existing blob", fake.patches)
	}
}

func TestUploadFailsWhenTheRegistryCannotReportTheOffset(t *testing.T) {
	fake := newFakeRegistry()
	fake.failPATCH = 2
	fake.noRangeHeader = true
	server := httptest.NewServer(fake)
	defer server.Close()

	layer := layerOf(t, []byte(strings.Repeat("y", 4096)))
	uploader := uploaderFor(1024)
	err := uploader.Upload(context.Background(), server.Listener.Addr().String()+"/repo", layer)
	if err == nil {
		t.Fatal("upload succeeded even though the offset was unknown")
	}
	if !strings.Contains(err.Error(), "could not be recovered") {
		t.Fatalf("error = %v", err)
	}
}

func TestUploadZeroChunkSendsOneRequest(t *testing.T) {
	fake := newFakeRegistry()
	server := httptest.NewServer(fake)
	defer server.Close()

	data := []byte(strings.Repeat("z", 5000))
	layer := layerOf(t, data)
	uploader := uploaderFor(0)
	if err := uploader.Upload(context.Background(), server.Listener.Addr().String()+"/repo", layer); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if fake.patches != 1 || fake.maxChunk != int64(len(data)) {
		t.Fatalf("patches = %d, max chunk = %d; want 1 and %d", fake.patches, fake.maxChunk, len(data))
	}
}
