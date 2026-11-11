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

package controller

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// DevBoxPlatform is the platform the operator resolves image configs for. The
// cluster nodes are amd64, and a config resolved for another architecture
// would hand the wrong ENTRYPOINT to devpilot-init.
var DevBoxPlatform = v1.Platform{OS: "linux", Architecture: "amd64"}

// ImageInfo is everything the pod template needs from the base image.
type ImageInfo struct {
	// ImageRef is the reference the user asked for, recorded in the root disk
	// metadata so a later rebase can tell what upper/ was built from.
	ImageRef string
	// Digest is the platform specific manifest digest.
	Digest string
	// Entrypoint and Cmd reproduce the image's own start command.
	Entrypoint []string
	Cmd        []string
}

// ImageResolver resolves an image reference to its digest and config.
type ImageResolver interface {
	Resolve(ctx context.Context, image string) (ImageInfo, error)
}

// RegistryResolver resolves images by asking the registry directly. It only
// fetches the manifest and the config blob, never the layers.
type RegistryResolver struct{}

// Resolve implements ImageResolver.
func (RegistryResolver) Resolve(ctx context.Context, image string) (ImageInfo, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return ImageInfo{}, fmt.Errorf("parse image reference %q: %w", image, err)
	}
	img, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(DevBoxPlatform),
	)
	if err != nil {
		return ImageInfo{}, fmt.Errorf("fetch image %q: %w", image, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return ImageInfo{}, fmt.Errorf("digest of %q: %w", image, err)
	}
	config, err := img.ConfigFile()
	if err != nil {
		return ImageInfo{}, fmt.Errorf("config of %q: %w", image, err)
	}
	return ImageInfo{
		ImageRef:   image,
		Digest:     digest.String(),
		Entrypoint: append([]string{}, config.Config.Entrypoint...),
		Cmd:        append([]string{}, config.Config.Cmd...),
	}, nil
}
