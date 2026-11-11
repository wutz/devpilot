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
	"context"
	"io"
	"log"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

const (
	testCommitTime = "2026-11-12T08:00:00Z"
	testBaseRef    = "example.com/base:1"
	testRepository = "example.com/app"
	testUpperDir   = "/workspace"
	testModelPath  = "opt/app/model.py"
	testAuthor     = "alice"
	testEntrypoint = "/entry"
	testModelBody  = "print(1)\n"
)

// testRegistry serves the in-memory registry over plain HTTP, which
// go-containerregistry addresses as http:// for a loopback host.
func testRegistry(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

// seedBase pushes a one-layer base image that the push tests append to.
func seedBase(t *testing.T, host string) string {
	t.Helper()
	layer, err := random.Layer(1024, types.DockerLayer)
	if err != nil {
		t.Fatalf("create base layer: %v", err)
	}
	image, err := mutate.Append(empty.Image, mutate.Addendum{
		Layer:   layer,
		History: v1.History{CreatedBy: "seed"},
	})
	if err != nil {
		t.Fatalf("append base layer: %v", err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatalf("read base config: %v", err)
	}
	config.Architecture = Platform.Architecture
	config.OS = Platform.OS
	config.Created = v1.Time{Time: time.Unix(0, 0)}
	config.Config.Env = []string{"PATH=/usr/local/bin:/usr/bin", "BASE=1"}
	config.Config.Cmd = []string{"/bin/bash"}
	config.Config.WorkingDir = testUpperDir
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatalf("write base config: %v", err)
	}
	image = mutate.MediaType(image, types.OCIManifestSchema1)

	ref := host + "/base/devbox:1"
	tagged, err := name.ParseReference(ref)
	if err != nil {
		t.Fatalf("parse seed reference: %v", err)
	}
	if err := remote.Write(tagged, image); err != nil {
		t.Fatalf("push base image: %v", err)
	}
	return ref
}

// pushIncrement runs the full commit against a seeded registry and returns the
// result together with the image as the registry stored it.
func pushIncrement(t *testing.T) (*PushResult, v1.Image, time.Time) {
	t.Helper()
	host := testRegistry(t)
	base := seedBase(t, host)

	created, err := time.Parse(time.RFC3339, testCommitTime)
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	result, err := Push(context.Background(), PushRequest{
		BaseReference: base,
		Repository:    host + "/library/devbox",
		Tags:          []string{"v1", "v2"},
		UpperDir: writeTree(t, map[string]string{
			testModelPath:       testModelBody,
			"root/.cache/pip/x": "cache\n",
		}),
		ExcludePaths:      []string{"root/.cache/**"},
		CleanPackageCache: true,
		Compression:       CompressionZstd,
		Created:           created,
		Changes: Changes{
			Author:     testAuthor,
			Comment:    "install einops",
			Env:        []EnvVar{{Name: "BASE", Value: "2"}, {Name: "NEW", Value: "x"}},
			WorkingDir: "/opt/app",
			Cmd:        []string{testEntrypoint, "--flag"},
		},
	})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	ref, err := name.ParseReference(host + "/library/devbox:v1")
	if err != nil {
		t.Fatalf("parse result reference: %v", err)
	}
	pushed, err := remote.Image(ref)
	if err != nil {
		t.Fatalf("read pushed image: %v", err)
	}
	return result, pushed, created
}

func TestPushAppendsAnIncrementLayer(t *testing.T) {
	result, pushed, created := pushIncrement(t)

	assertPushShape(t, result, pushed)
	assertPushConfig(t, pushed.ConfigFile, result, created)

	members := layerMembers(t, pushed)
	if _, ok := members[testModelPath]; !ok {
		t.Errorf("increment layer = %v, want model.py", members)
	}
	for member := range members {
		if strings.HasPrefix(member, "root/.cache/") {
			t.Errorf("%s should have been excluded", member)
		}
	}
}

func assertPushShape(t *testing.T, result *PushResult, pushed v1.Image) {
	t.Helper()
	if len(result.Images) != 2 {
		t.Fatalf("pushed images = %v, want two", result.Images)
	}
	for _, image := range result.Images {
		if !strings.Contains(image, "@sha256:") || !strings.Contains(image, ":v") {
			t.Errorf("image reference %q should carry a tag and a digest", image)
		}
	}
	if result.Stats.Files != 1 {
		t.Errorf("stats = %+v, want only model.py packaged", result.Stats)
	}
	if result.LayerSize == 0 {
		t.Error("staged layer size is zero")
	}
	if media, err := pushed.MediaType(); err != nil || media != types.OCIManifestSchema1 {
		t.Errorf("manifest media type = %v (%v), want the OCI type of the base", media, err)
	}

	manifest, err := pushed.Manifest()
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if len(manifest.Layers) != 2 {
		t.Fatalf("layer count = %d, want base plus increment", len(manifest.Layers))
	}
	if got := manifest.Layers[1].MediaType; got != types.OCILayerZStd {
		t.Errorf("increment media type = %v, want OCILayerZStd", got)
	}
}

func assertPushConfig(t *testing.T, read func() (*v1.ConfigFile, error), result *PushResult, created time.Time) {
	t.Helper()
	config, err := read()
	if err != nil {
		t.Fatalf("read pushed config: %v", err)
	}
	if len(config.RootFS.DiffIDs) != 2 {
		t.Fatalf("diff ids = %v, want base plus increment", config.RootFS.DiffIDs)
	}
	if config.RootFS.DiffIDs[1] != result.DiffID {
		t.Errorf("diff id %s not recorded as the final layer %s", result.DiffID, config.RootFS.DiffIDs[1])
	}
	if got := config.Config.Env; !contains(got, "BASE=2") || !contains(got, "NEW=x") || !contains(got, "PATH=/usr/local/bin:/usr/bin") {
		t.Errorf("env = %v, want the override, the addition and the inherited entry", got)
	}
	if countPrefix(config.Config.Env, "BASE=") != 1 {
		t.Errorf("env = %v, want BASE to be replaced rather than duplicated", config.Config.Env)
	}
	if config.Config.WorkingDir != "/opt/app" {
		t.Errorf("working dir = %q, want /opt/app", config.Config.WorkingDir)
	}
	if len(config.Config.Cmd) != 2 || config.Config.Cmd[0] != testEntrypoint {
		t.Errorf("cmd = %v, want [/entry --flag]", config.Config.Cmd)
	}
	if config.Author != testAuthor {
		t.Errorf("author = %q, want alice", config.Author)
	}
	if !config.Created.Equal(created) {
		t.Errorf("created = %v, want %v so the manifest is reproducible", config.Created.Time, created)
	}

	history := config.History[len(config.History)-1]
	if history.CreatedBy != "devpilot-commit" || history.Comment != "install einops" || history.Author != testAuthor {
		t.Errorf("last history = %+v", history)
	}
	if len(config.History) != len(config.RootFS.DiffIDs) {
		t.Errorf("history has %d entries but %d diff ids", len(config.History), len(config.RootFS.DiffIDs))
	}
}

func TestPushRejectsZstdForADockerBase(t *testing.T) {
	host := testRegistry(t)
	base := seedBase(t, host)

	baseRef, err := name.ParseReference(base)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	image, err := remote.Image(baseRef)
	if err != nil {
		t.Fatalf("read base: %v", err)
	}
	docker := mutate.MediaType(image, types.DockerManifestSchema2)
	if err := remote.Write(baseRef, docker); err != nil {
		t.Fatalf("rewrite base as docker: %v", err)
	}

	_, err = Push(context.Background(), PushRequest{
		BaseReference: base,
		Repository:    host + "/library/devbox",
		Tags:          []string{"v1"},
		UpperDir:      writeTree(t, map[string]string{testModelPath: testModelBody}),
		Compression:   CompressionZstd,
	})
	if err == nil || !strings.Contains(err.Error(), "gzip") {
		t.Fatalf("err = %v, want a docker manifest refusing a zstd layer", err)
	}
}

func TestPushUsesTheDockerLayerTypeForADockerBase(t *testing.T) {
	host := testRegistry(t)
	base := seedBase(t, host)

	baseRef, err := name.ParseReference(base)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	image, err := remote.Image(baseRef)
	if err != nil {
		t.Fatalf("read base: %v", err)
	}
	if err := remote.Write(baseRef, mutate.MediaType(image, types.DockerManifestSchema2)); err != nil {
		t.Fatalf("rewrite base as docker: %v", err)
	}

	if _, err := Push(context.Background(), PushRequest{
		BaseReference: base,
		Repository:    host + "/library/devbox",
		Tags:          []string{"v1"},
		UpperDir:      writeTree(t, map[string]string{testModelPath: testModelBody}),
		Compression:   CompressionGzip,
	}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	ref, err := name.ParseReference(host + "/library/devbox:v1")
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	pushed, err := remote.Image(ref)
	if err != nil {
		t.Fatalf("read pushed image: %v", err)
	}
	manifest, err := pushed.Manifest()
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if got := manifest.Layers[len(manifest.Layers)-1].MediaType; got != types.DockerLayer {
		t.Errorf("increment media type = %v, want DockerLayer", got)
	}
	if media, err := pushed.MediaType(); err != nil || media != types.DockerManifestSchema2 {
		t.Errorf("manifest media type = %v (%v), want the docker type of the base", media, err)
	}
}

func TestPushRejectsInvalidRequests(t *testing.T) {
	cases := map[string]PushRequest{
		"no base":       {Repository: testRepository, Tags: []string{"v1"}, UpperDir: t.TempDir()},
		"no repository": {BaseReference: testBaseRef, Tags: []string{"v1"}, UpperDir: t.TempDir()},
		"no tags":       {BaseReference: testBaseRef, Repository: testRepository, UpperDir: t.TempDir()},
		"missing upper": {BaseReference: testBaseRef, Repository: testRepository, Tags: []string{"v1"}},
	}
	for label, req := range cases {
		t.Run(label, func(t *testing.T) {
			if _, err := Push(context.Background(), req); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestApplyChangesKeepsUnsetFields(t *testing.T) {
	base := &v1.ConfigFile{
		Config: v1.Config{
			Env:        []string{"A=1", "B=2"},
			WorkingDir: testUpperDir,
			Cmd:        []string{"/bin/bash"},
			Entrypoint: []string{testEntrypoint},
		},
		RootFS: v1.RootFS{Type: "layers", DiffIDs: []v1.Hash{{Algorithm: "sha256", Hex: "00"}}},
	}

	config := applyChanges(base, Changes{Env: []EnvVar{{Name: "A", Value: "9"}}}, time.Unix(0, 0))
	if config.Config.WorkingDir != testUpperDir || len(config.Config.Cmd) != 1 || len(config.Config.Entrypoint) != 1 {
		t.Errorf("unset changes overwrote config: %+v", config.Config)
	}
	if got := config.Config.Env; !contains(got, "A=9") || !contains(got, "B=2") {
		t.Errorf("env = %v", got)
	}
	// mutate.Append records the increment's diffID, so applyChanges must not.
	if len(config.RootFS.DiffIDs) != 1 {
		t.Errorf("diff ids = %v, want the base entry untouched", config.RootFS.DiffIDs)
	}
	if len(base.RootFS.DiffIDs) != 1 {
		t.Error("applyChanges mutated the base config instead of a copy")
	}
}

// layerMembers decodes the final layer of an image into its entry names.
func layerMembers(t *testing.T, image v1.Image) map[string]struct{} {
	t.Helper()
	layers, err := image.Layers()
	if err != nil {
		t.Fatalf("read layers: %v", err)
	}
	reader, err := layers[len(layers)-1].Uncompressed()
	if err != nil {
		t.Fatalf("open final layer: %v", err)
	}
	defer func() { _ = reader.Close() }()

	out := make(map[string]struct{})
	tr := tar.NewReader(reader)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("read layer: %v", err)
		}
		out[hdr.Name] = struct{}{}
	}
}

func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}

func countPrefix(values []string, prefix string) int {
	n := 0
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			n++
		}
	}
	return n
}
