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

// Package initroot implements the kernel-level part of devpilot-init: the
// OverlayFS assembly over the root disk PVC, migration of the container's
// initial submounts and the pivot_root handover.
//
// The logic is split so that everything except the syscall surface can be
// exercised on any platform; see setup_linux.go and setup_other.go.
package initroot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Directory layout inside the root disk PVC (see docs/persistence-and-commit.md).
const (
	UpperDir        = "upper"
	WorkDir         = "work"
	MergedDir       = "merged"
	MetaDir         = ".meta"
	BaseImageFile   = "base-image.json"
	GenerationFile  = "generation.json"
	OpaqueIndexFile = "opaque-dirs.json"
)

// BaseImageMeta pins the base image the upper layer was created against.
// While upper/ holds data the pod must start from exactly this digest,
// otherwise hardlinks and dynamic library paths in upper/ become inconsistent.
type BaseImageMeta struct {
	ImageRef    string    `json:"imageRef"`
	ImageDigest string    `json:"imageDigest"`
	CreatedAt   time.Time `json:"createdAt"`
	RebaseCount int       `json:"rebaseCount"`
}

// GenerationMeta records the last mount and whether the previous unmount was
// clean, which the operator surfaces to decide whether a repair is needed.
type GenerationMeta struct {
	MountedAt    time.Time `json:"mountedAt"`
	Generation   int       `json:"generation"`
	CleanUnmount bool      `json:"cleanUnmount"`
}

// ReadBaseImageMeta loads .meta/base-image.json.
func ReadBaseImageMeta(root string) (*BaseImageMeta, error) {
	var meta BaseImageMeta
	if err := readJSON(filepath.Join(root, MetaDir, BaseImageFile), &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// WriteBaseImageMeta atomically writes .meta/base-image.json.
func WriteBaseImageMeta(root string, meta BaseImageMeta) error {
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}
	return writeJSONAtomic(filepath.Join(root, MetaDir, BaseImageFile), meta)
}

// ReadGenerationMeta loads .meta/generation.json.
func ReadGenerationMeta(root string) (*GenerationMeta, error) {
	var meta GenerationMeta
	if err := readJSON(filepath.Join(root, MetaDir, GenerationFile), &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// WriteGenerationMeta atomically writes .meta/generation.json.
func WriteGenerationMeta(root string, meta GenerationMeta) error {
	return writeJSONAtomic(filepath.Join(root, MetaDir, GenerationFile), meta)
}

// UpperIsEmpty reports whether upper/ holds no entries at all. A missing
// upper/ counts as empty: the first boot creates it.
func UpperIsEmpty(root string) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(root, UpperDir))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read upper dir: %w", err)
	}
	return len(entries) == 0, nil
}

// EnsureLayout creates the directory tree the overlay mount expects.
func EnsureLayout(root string) error {
	for _, dir := range []string{UpperDir, WorkDir, MergedDir, MetaDir} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

// ClearUpper removes the writable state of the previous base image so the next
// overlay can be assembled on top of a new digest. upper/ is the user's
// accumulated changes, work/ holds in-flight overlayfs state tied to the old
// lower layer, and merged/ is a mount point whose contents are invalid without
// the lower layer it was assembled from; all three have to go together.
//
// The caller must guarantee that nothing writes to the root disk: the operator
// only asks for it on a stopped DevBox whose commit was taken offline.
func ClearUpper(root string) error {
	// The merged mount point itself is owned by the mount; emptying it is
	// enough and keeps the directory in place for the next Setup.
	for _, dir := range []string{UpperDir, WorkDir, MergedDir} {
		path := filepath.Join(root, dir)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("clear %s: %w", dir, err)
		}
	}
	return nil
}

// RebaseUpper replaces upper/ with an empty one before the digest invariant is
// checked: the point of a rebase is precisely that the old upper layer does not
// belong to the new base image. rebaseCount is incremented so the operator can
// tell how many times the root disk was flattened.
func RebaseUpper(root string, imageRef, imageDigest string) (BaseImageMeta, error) {
	if imageRef == "" || imageDigest == "" {
		return BaseImageMeta{}, errors.New("rebase needs both the image reference and the image digest")
	}
	count := 0
	if existing, err := ReadBaseImageMeta(root); err == nil {
		count = existing.RebaseCount
	} else if !errors.Is(err, os.ErrNotExist) {
		return BaseImageMeta{}, err
	}

	if err := ClearUpper(root); err != nil {
		return BaseImageMeta{}, err
	}
	meta := BaseImageMeta{
		ImageRef:    imageRef,
		ImageDigest: imageDigest,
		CreatedAt:   time.Now().UTC(),
		RebaseCount: count + 1,
	}
	// Setup recreates the tree as well, but the meta write below needs .meta/
	// and a caller may read the result before any mount happens.
	if err := EnsureLayout(root); err != nil {
		return BaseImageMeta{}, err
	}
	return meta, nil
}

// PinBaseImage checks the digest invariant and returns the meta to persist.
//
//   - upper/ empty: whatever image the pod started with becomes the new pin;
//     an existing meta is replaced and rebaseCount is carried over.
//   - upper/ non-empty and meta present: digests must match exactly.
//   - upper/ non-empty and meta missing: refuse, the upper layer could belong
//     to any image and mounting it on a different base silently corrupts it.
func PinBaseImage(root, imageRef, imageDigest string) (BaseImageMeta, error) {
	if imageRef == "" {
		return BaseImageMeta{}, errors.New("base image reference is empty")
	}
	if imageDigest == "" {
		return BaseImageMeta{}, errors.New("base image digest is empty")
	}

	empty, err := UpperIsEmpty(root)
	if err != nil {
		return BaseImageMeta{}, err
	}
	existing, err := ReadBaseImageMeta(root)
	switch {
	case err == nil:
		if empty {
			return BaseImageMeta{
				ImageRef:    imageRef,
				ImageDigest: imageDigest,
				CreatedAt:   time.Now().UTC(),
				RebaseCount: existing.RebaseCount,
			}, nil
		}
		if existing.ImageDigest != imageDigest {
			return BaseImageMeta{}, fmt.Errorf(
				"base image digest mismatch: root disk was created from %s (%s) but pod started from %s",
				existing.ImageRef, existing.ImageDigest, imageRef)
		}
		return *existing, nil
	case errors.Is(err, os.ErrNotExist):
		if empty {
			return BaseImageMeta{
				ImageRef:    imageRef,
				ImageDigest: imageDigest,
				CreatedAt:   time.Now().UTC(),
			}, nil
		}
		return BaseImageMeta{}, fmt.Errorf(
			"root disk has data in %s/ but no %s; refusing to mount %s on top of it",
			UpperDir, filepath.Join(MetaDir, BaseImageFile), imageRef)
	default:
		return BaseImageMeta{}, err
	}
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func writeJSONAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}
