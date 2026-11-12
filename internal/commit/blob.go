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
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// BlobUploader puts a staged blob in the registry before the manifest is
// written.
//
// go-containerregistry v0.20 sends the whole layer in one PATCH. On a real
// increment that is a single request carrying tens of gigabytes and staying
// open for around an hour, which registries behind a reverse proxy cut off
// (a 400 from nginx after ~11 GB in the M4 verification). A caller that knows
// how the registry is reached can hand in an uploader that splits the transfer
// into bounded requests and resumes from whatever the registry acknowledged.
type BlobUploader interface {
	// Upload makes the blob available in repository. It must be idempotent: a
	// blob already present is success, not an error.
	Upload(ctx context.Context, repository string, blob v1.Layer) error
}

// ChunkSize is the amount of compressed data one PATCH carries in the default
// uploader.
//
// 64 MiB keeps a request at a few seconds on a slow link, so a proxy timeout
// or a broken connection costs one chunk rather than the whole layer, and it
// stays far below the smallest body limit seen in practice.
const ChunkSize int64 = 64 << 20

// RegistryUploader uploads blobs over the OCI distribution protocol in chunks.
type RegistryUploader struct {
	// ChunkSize is the body size of one PATCH; zero means ChunkSize.
	ChunkSize int64
	// Auth is the registry credential. Nil falls back to the keychain.
	Auth authn.Authenticator
	// Transport reaches the registry; nil means a plain HTTP transport, so
	// name.Insecure registries work the same way they do for remote.Push.
	Transport http.RoundTripper
	// NameOptions configure reference parsing of repository.
	NameOptions []name.Option
	// Logf receives one line per chunk and per resume. Nil is silent.
	Logf func(msg string, kv ...any)
}

// Upload transfers blob to repository in bounded PATCH requests.
func (u *RegistryUploader) Upload(ctx context.Context, repository string, blob v1.Layer) error {
	repo, err := name.NewRepository(repository, u.NameOptions...)
	if err != nil {
		return fmt.Errorf("parse destination repository %q: %w", repository, err)
	}
	digest, err := blob.Digest()
	if err != nil {
		return fmt.Errorf("read layer digest: %w", err)
	}
	auth := u.Auth
	if auth == nil {
		auth = authn.Anonymous
	}
	// transport.NewWithContext wraps whatever it is given; a nil inner
	// transport panics on the first request instead of defaulting.
	base := u.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	tr, err := transport.NewWithContext(ctx, repo.Registry, auth, base,
		[]string{repo.Scope(transport.PushScope)})
	if err != nil {
		return fmt.Errorf("authenticate to %s: %w", repo.RegistryStr(), err)
	}
	client := &http.Client{Transport: tr}

	exists, err := u.blobExists(ctx, client, repo, digest)
	if err != nil {
		return err
	}
	if exists {
		u.logf("Layer blob already in the repository", "repository", repo.RepositoryStr(), "digest", digest.String())
		return nil
	}

	file, err := blob.Compressed()
	if err != nil {
		return fmt.Errorf("read compressed layer: %w", err)
	}
	defer func() { _ = file.Close() }()
	size, err := blob.Size()
	if err != nil {
		return fmt.Errorf("read layer size: %w", err)
	}
	// Layers in this project come from a staged tar stream, whose reader is a
	// NopCloser over a plain reader, so seeking is optional: the normal path
	// only ever reads forward and keeps the chunk it just sent in memory.
	seeker, _ := file.(io.ReadSeeker)

	location, err := u.startUpload(ctx, client, repo)
	if err != nil {
		return err
	}
	chunkSize := u.ChunkSize
	if chunkSize <= 0 {
		chunkSize = ChunkSize
	}

	// buf holds the chunk most recently sent so that a resume can resend its
	// tail without asking the layer to seek.
	buf := make([]byte, chunkSize)
	var sent int64
	for sent < size {
		end := min(sent+chunkSize, size)
		n, readErr := io.ReadFull(file, buf[:end-sent])
		if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
			return fmt.Errorf("read the staged layer at %d: %w", sent, readErr)
		}
		if n == 0 {
			break
		}
		start := sent
		end = start + int64(n)
		sent = end

		// next is only adopted on success: a failed PATCH returns no location,
		// and the session from before it is what a resume has to talk to.
		next, err := u.patchChunk(ctx, client, repo, location, buf[:n], start, end)
		if err == nil {
			location = next
			u.logf("Uploaded layer chunk", "offset", sent, "total", size)
			continue
		}
		// A cut connection is the failure this design exists for: ask the
		// registry how much it kept and continue from there instead of
		// throwing away everything already sent.
		resumed, resumeErr := u.resumeAt(ctx, client, location)
		if resumeErr != nil {
			return fmt.Errorf("%w (and the upload offset could not be recovered: %v)", err, resumeErr)
		}
		u.logf("Resuming the layer upload", "offset", resumed, "total", size)
		switch {
		case resumed == sent:
			// It stored the chunk; only the answer was lost.
		case resumed > sent:
			return fmt.Errorf("the registry reports %d bytes of a %d-byte layer, past the %d sent",
				resumed, size, sent)
		case resumed >= start:
			// It kept part of this chunk. The rest is still in buf, so resend
			// from there rather than re-reading the layer.
			resend, err := u.patchChunk(ctx, client, repo, location, buf[resumed-start:], resumed, sent)
			if err != nil {
				return err
			}
			location = resend
		default:
			// It kept less than this chunk started with, so those bytes have
			// already been read past and only a seekable layer can supply them.
			if seeker == nil {
				return fmt.Errorf("%w (and the upload can only be resumed at byte %d, behind a layer that cannot seek)",
					err, resumed)
			}
			if _, err := seeker.Seek(resumed, io.SeekStart); err != nil {
				return fmt.Errorf("seek the staged layer to %d: %w", resumed, err)
			}
			sent = resumed
		}
	}

	if err := u.commitUpload(ctx, client, location, digest); err != nil {
		return err
	}
	// Reporting success without the blob present would let the manifest push
	// fail afterwards with a far less useful message.
	exists, err = u.blobExists(ctx, client, repo, digest)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the registry accepted the upload but does not report blob %s", digest.String())
	}
	return nil
}

// blobExists asks the registry whether the digest is already stored.
func (u *RegistryUploader) blobExists(ctx context.Context, client *http.Client, repo name.Repository,
	digest v1.Hash) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		u.url(repo, fmt.Sprintf("/v2/%s/blobs/%s", repo.RepositoryStr(), digest.String())), nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("check blob %s: %w", digest.String(), err)
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, transport.CheckError(resp, http.StatusOK, http.StatusNotFound)
	}
}

// startUpload opens an upload session and returns its location.
func (u *RegistryUploader) startUpload(ctx context.Context, client *http.Client,
	repo name.Repository) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		u.url(repo, fmt.Sprintf("/v2/%s/blobs/uploads/", repo.RepositoryStr())), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("start the layer upload: %w", err)
	}
	defer drain(resp)
	if err := transport.CheckError(resp, http.StatusAccepted); err != nil {
		return "", fmt.Errorf("start the layer upload: %w", err)
	}
	location, err := nextLocation(repo, resp)
	if err != nil {
		return "", fmt.Errorf("start the layer upload: %w", err)
	}
	return location, nil
}

// patchChunk sends chunk as bytes [start,end) and returns the location for the
// next one.
func (u *RegistryUploader) patchChunk(ctx context.Context, client *http.Client, repo name.Repository,
	location string, chunk []byte, start, end int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, location, bytes.NewReader(chunk))
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(len(chunk))
	// Content-Range is what lets the registry place the bytes when several
	// chunks arrive, and what makes a resumed upload continue rather than
	// restart.
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Range", fmt.Sprintf("%d-%d", start, end-1))

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload bytes %d-%d: %w", start, end-1, err)
	}
	defer drain(resp)
	if err := transport.CheckError(resp, http.StatusAccepted, http.StatusNoContent); err != nil {
		return "", fmt.Errorf("upload bytes %d-%d: %w", start, end-1, err)
	}
	return locationOf(repo, resp, location)
}

// resumeAt asks the registry how far the upload got. Distribution answers GET
// on the session with a Range header of the bytes it holds.
func (u *RegistryUploader) resumeAt(ctx context.Context, client *http.Client, location string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("the registry answered %d when asked for the upload offset", resp.StatusCode)
	}
	// "0-<last byte>" or "0-0" for an empty session; ignore a unit prefix.
	value := strings.TrimSpace(resp.Header.Get("Range"))
	if _, rest, found := strings.Cut(value, "="); found {
		value = rest
	}
	start, end, found := strings.Cut(value, "-")
	if !found {
		return 0, fmt.Errorf("the registry reported no upload range (%q)", value)
	}
	first, err := strconv.ParseInt(start, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse the upload range start %q: %w", start, err)
	}
	last, err := strconv.ParseInt(end, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse the upload range end %q: %w", end, err)
	}
	if last < first {
		return 0, nil
	}
	return last + 1, nil
}

// commitUpload closes the session with the layer digest.
func (u *RegistryUploader) commitUpload(ctx context.Context, client *http.Client, location string,
	digest v1.Hash) error {
	parsed, err := url.Parse(location)
	if err != nil {
		return fmt.Errorf("parse the upload location %q: %w", location, err)
	}
	query := parsed.Query()
	query.Set("digest", digest.String())
	parsed.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, parsed.String(), nil)
	if err != nil {
		return err
	}
	req.ContentLength = 0
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("finish the layer upload: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("finish the layer upload: %w", transport.CheckError(resp, http.StatusCreated))
	}
	return nil
}

// url joins the registry base, the location path and the repository.
func (u *RegistryUploader) url(repo name.Repository, path string) string {
	return fmt.Sprintf("%s://%s%s", repo.Scheme(), repo.RegistryStr(), path)
}

// logf writes one progress line when a logger was supplied.
func (u *RegistryUploader) logf(msg string, kv ...any) {
	if u.Logf != nil {
		u.Logf(msg, kv...)
	}
}

// locationOf resolves the Location header of a chunk, keeping fallback when the
// header is absent.
//
// Registries are allowed to answer with a path relative to the registry root,
// which Harbor does on every PATCH. Keeping that verbatim would leave the next
// request with no scheme at all, so it is resolved here rather than assumed
// absolute.
func locationOf(repo name.Repository, resp *http.Response, fallback string) (string, error) {
	location := resp.Header.Get("Location")
	if location == "" {
		return fallback, nil
	}
	resolved, err := absolute(repo, location)
	if err != nil {
		return "", fmt.Errorf("parse the upload location %q: %w", location, err)
	}
	return resolved, nil
}

// nextLocation resolves the Location header of a session start.
func nextLocation(repo name.Repository, resp *http.Response) (string, error) {
	location := resp.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("the registry returned no upload location")
	}
	return absolute(repo, location)
}

// absolute resolves a location against the registry base.
func absolute(repo name.Repository, location string) (string, error) {
	parsed, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	if parsed.IsAbs() {
		return location, nil
	}
	base, err := url.Parse(fmt.Sprintf("%s://%s", repo.Scheme(), repo.RegistryStr()))
	if err != nil {
		return "", err
	}
	return base.ResolveReference(parsed).String(), nil
}

// drain closes the body after reading nothing of interest.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}
