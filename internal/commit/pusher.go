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
	"fmt"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Platform is the base image platform to resolve when the reference is a
// multi-platform index. DevBox root disks are built on linux/amd64 nodes in
// bz1-dev1.
var Platform = v1.Platform{OS: "linux", Architecture: "amd64"}

// EnvVar is one environment entry in Changes.
type EnvVar struct {
	Name  string
	Value string
}

// Changes overlays image config metadata, mirroring DevBoxCommit.spec.changes.
type Changes struct {
	Author     string
	Comment    string
	Env        []EnvVar
	WorkingDir string
	Cmd        []string
}

// PushRequest is one commit.
type PushRequest struct {
	// BaseReference is the repository and tag the root disk was built from.
	BaseReference string
	// BaseDigest pins the exact base image the upper layer belongs to. It
	// normally comes from .meta/base-image.json, whose digest the devbox booted
	// from; anything else risks a layer that no longer matches its parent.
	BaseDigest string
	// Repository is registry/project/repo to push to, without a tag.
	Repository string
	// Tags are pushed for the resulting manifest.
	Tags []string
	// UpperDir is the scanned overlay upper/ directory.
	UpperDir string
	// OpaqueDirs is the snapshot from .meta/opaque-dirs.json.
	OpaqueDirs []string
	// ExcludePaths are extra globs.
	ExcludePaths []string
	// CleanPackageCache drops package manager caches.
	CleanPackageCache bool
	// Compression is CompressionZstd or CompressionGzip.
	Compression string
	// Changes are config overlays.
	Changes Changes
	// ScratchDir holds the staged layer; defaults to the OS temp dir.
	ScratchDir string
	// Created stamps the config and the new history entry. A zero value means
	// now. Tests and reproducible rebuilds set it explicitly.
	Created time.Time
	// Options are passed to every registry request.
	Options []remote.Option
	// NameOptions configure reference parsing, for example name.Insecure for a
	// plain HTTP registry.
	NameOptions []name.Option
	// Uploader transfers the increment layer before the manifest is written.
	// Nil sends it through remote.Write, which is correct but puts the whole
	// layer in one request; see BlobUploader.
	Uploader BlobUploader
}

// PushResult reports what was published.
type PushResult struct {
	BaseDigest     string
	BaseMediaType  types.MediaType
	ConfigDigest   v1.Hash
	ManifestDigest v1.Hash
	DiffID         v1.Hash
	LayerSize      int64
	// Images are the pushed references in repo:tag@digest form.
	Images []string
	Stats  Stats
}

// Push builds the increment layer and appends it to the base image manifest.
//
// Nothing from the base image is downloaded: the manifest and config are a few
// kilobytes over HTTPS, and the existing layer blobs are referenced by digest.
// The registry mounts them into the destination repository itself when it can,
// which is what keeps a commit of a 30 GB PyTorch image down to the size of the
// user's own changes.
func Push(ctx context.Context, req PushRequest) (*PushResult, error) {
	if req.BaseReference == "" {
		return nil, fmt.Errorf("base image reference is required")
	}
	if req.Repository == "" {
		return nil, fmt.Errorf("destination repository is required")
	}
	if len(req.Tags) == 0 {
		return nil, fmt.Errorf("at least one destination tag is required")
	}
	if req.Compression == "" {
		req.Compression = CompressionZstd
	}

	opts := append([]remote.Option{remote.WithContext(ctx), remote.WithPlatform(Platform)}, req.Options...)
	base, err := readBase(req, opts)
	if err != nil {
		return nil, err
	}
	docker := base.media == types.DockerManifestSchema2

	layer, err := stageLayer(Options{
		UpperDir:          req.UpperDir,
		OpaqueDirs:        req.OpaqueDirs,
		ExcludePaths:      req.ExcludePaths,
		CleanPackageCache: req.CleanPackageCache,
	}, req.Compression, req.ScratchDir)
	if err != nil {
		return nil, err
	}
	defer layer.Discard()

	media, err := layerMediaType(req.Compression, docker)
	if err != nil {
		return nil, err
	}
	if err := checkLayerMedia(docker, media, base.ref, req.Compression); err != nil {
		return nil, err
	}
	layer.mediaType = media

	diffID, err := layer.DiffID()
	if err != nil {
		return nil, fmt.Errorf("read layer diff id: %w", err)
	}
	baseConfig, err := base.image.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("read base config: %w", err)
	}

	created := req.Created
	if created.IsZero() {
		created = time.Now()
	}
	created = created.Truncate(time.Second)
	config := applyChanges(baseConfig, req.Changes, created)

	image, err := mutate.ConfigFile(base.image, config)
	if err != nil {
		return nil, fmt.Errorf("apply config: %w", err)
	}
	image, err = mutate.Append(image, mutate.Addendum{
		Layer: layer,
		History: v1.History{
			Author:    req.Changes.Author,
			Comment:   req.Changes.Comment,
			Created:   v1.Time{Time: created},
			CreatedBy: "devpilot-commit",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("append layer: %w", err)
	}
	// The manifest format follows the base image, otherwise a docker base would
	// silently turn into an OCI image and lose consumer compatibility.
	image = mutate.MediaType(image, base.media)
	if base.media == types.DockerManifestSchema2 {
		image = mutate.ConfigMediaType(image, types.DockerConfigJSON)
	}

	configDigest, err := image.ConfigName()
	if err != nil {
		return nil, fmt.Errorf("compute config digest: %w", err)
	}
	manifestDigest, err := image.Digest()
	if err != nil {
		return nil, fmt.Errorf("compute manifest digest: %w", err)
	}
	size, err := layer.Size()
	if err != nil {
		return nil, fmt.Errorf("read layer size: %w", err)
	}

	result := &PushResult{
		BaseDigest:     base.digest.String(),
		BaseMediaType:  base.media,
		ConfigDigest:   configDigest,
		ManifestDigest: manifestDigest,
		DiffID:         diffID,
		LayerSize:      size,
		Stats:          layer.Stats(),
	}

	repo, err := name.NewRepository(req.Repository, req.NameOptions...)
	if err != nil {
		return nil, fmt.Errorf("parse destination repository %q: %w", req.Repository, err)
	}
	if req.Uploader != nil {
		// remote.Write skips a blob it finds by digest, so uploading it here
		// first is what keeps the manifest write to a few kilobytes.
		if err := req.Uploader.Upload(ctx, req.Repository, layer); err != nil {
			return nil, fmt.Errorf("upload the increment layer to %s: %w", req.Repository, err)
		}
	}
	for _, tag := range req.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		ref := repo.Tag(tag)
		if err := remote.Write(ref, image, opts...); err != nil {
			return nil, fmt.Errorf("push %s: %w", ref, err)
		}
		// repo:tag@digest is the form that survives a tag being moved later.
		result.Images = append(result.Images, ref.Name()+"@"+manifestDigest.String())
	}
	if len(result.Images) == 0 {
		return nil, fmt.Errorf("no destination tag was usable")
	}
	return result, nil
}

// baseImage is the image being extended, with its descriptor already read.
//
// No layer blob is fetched: the digest, media type and config all live in the
// manifest, which is a few kilobytes.
type baseImage struct {
	ref    name.Reference
	image  v1.Image
	digest v1.Hash
	media  types.MediaType
}

// readBase resolves the base reference and reads its descriptor.
func readBase(req PushRequest, opts []remote.Option) (*baseImage, error) {
	ref, err := name.ParseReference(req.BaseReference, req.NameOptions...)
	if err != nil {
		return nil, fmt.Errorf("parse base reference %q: %w", req.BaseReference, err)
	}
	// Pinning the digest keeps the pushed manifest built on exactly the image
	// the caller based its filesystem on, even if the tag moves meanwhile.
	if req.BaseDigest != "" {
		digest, err := v1.NewHash(req.BaseDigest)
		if err != nil {
			return nil, fmt.Errorf("parse base digest %q: %w", req.BaseDigest, err)
		}
		ref = ref.Context().Digest(digest.String())
	}
	image, err := remote.Image(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("read base image %s: %w", ref, err)
	}
	digest, err := image.Digest()
	if err != nil {
		return nil, fmt.Errorf("read base digest: %w", err)
	}
	media, err := image.MediaType()
	if err != nil {
		return nil, fmt.Errorf("read base media type: %w", err)
	}
	return &baseImage{ref: ref, image: image, digest: digest, media: media}, nil
}

// checkLayerMedia rejects a layer a docker manifest cannot describe.
func checkLayerMedia(docker bool, media types.MediaType, ref name.Reference, compression string) error {
	if !docker {
		return nil
	}
	if media == types.DockerLayer || media == types.DockerUncompressedLayer {
		return nil
	}
	return fmt.Errorf("base image %s uses a docker manifest, which cannot carry a %s layer; commit with gzip compression",
		ref, compression)
}

// applyChanges clones the base config and overlays the requested metadata.
//
// rootfs.diff_ids and history are deliberately left alone: mutate.Append writes
// both from the layer it is given, so doing it here as well would duplicate the
// increment's diffID and put history out of step with the layer count.
func applyChanges(base *v1.ConfigFile, changes Changes, created time.Time) *v1.ConfigFile {
	config := base.DeepCopy()
	if config.RootFS.Type == "" {
		config.RootFS.Type = "layers"
	}
	config.Created = v1.Time{Time: created}
	if changes.Author != "" {
		config.Author = changes.Author
	}
	if changes.WorkingDir != "" {
		config.Config.WorkingDir = changes.WorkingDir
	}
	if len(changes.Cmd) > 0 {
		config.Config.Cmd = append([]string{}, changes.Cmd...)
	}
	for _, env := range changes.Env {
		config.Config.Env = setEnv(config.Config.Env, env)
	}
	return config
}

// setEnv replaces an existing entry in place, matching how ENV behaves in a
// Dockerfile: a later assignment wins but does not move the variable.
func setEnv(env []string, entry EnvVar) []string {
	prefix := entry.Name + "="
	for i, current := range env {
		if strings.HasPrefix(current, prefix) {
			updated := make([]string, len(env))
			copy(updated, env)
			updated[i] = prefix + entry.Value
			return updated
		}
	}
	return append(append([]string{}, env...), prefix+entry.Value)
}
