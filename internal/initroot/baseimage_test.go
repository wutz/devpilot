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

package initroot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testImageRefA    = "pytorch/pytorch:2.4.0-cuda12.4"
	testImageDigestA = "sha256:aaaa"
	testImageRefB    = "pytorch/pytorch:2.5.0-cuda12.4"
	testImageDigestB = "sha256:bbbb"
)

func newRootDisk(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := EnsureLayout(root); err != nil {
		t.Fatalf("EnsureLayout() error = %v", err)
	}
	return root
}

func TestEnsureLayoutCreatesEveryLayer(t *testing.T) {
	root := newRootDisk(t)
	for _, dir := range []string{UpperDir, WorkDir, MergedDir, MetaDir} {
		info, err := os.Stat(filepath.Join(root, dir))
		if err != nil || !info.IsDir() {
			t.Errorf("layout directory %q missing: %v", dir, err)
		}
	}
}

func TestPinBaseImageFirstBoot(t *testing.T) {
	root := t.TempDir()
	meta, err := PinBaseImage(root, testImageRefA, testImageDigestA)
	if err != nil {
		t.Fatalf("PinBaseImage() error = %v", err)
	}
	if meta.ImageDigest != testImageDigestA || meta.ImageRef != testImageRefA {
		t.Errorf("PinBaseImage() = %+v, want the started image", meta)
	}
	if meta.RebaseCount != 0 {
		t.Errorf("RebaseCount = %d, want 0 on first boot", meta.RebaseCount)
	}
	if meta.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

func TestPinBaseImageReusesPinWhenDigestMatches(t *testing.T) {
	root := newRootDisk(t)
	if err := os.WriteFile(filepath.Join(root, UpperDir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	original := BaseImageMeta{
		ImageRef:    testImageRefA,
		ImageDigest: testImageDigestA,
		CreatedAt:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		RebaseCount: 3,
	}
	if err := WriteBaseImageMeta(root, original); err != nil {
		t.Fatalf("WriteBaseImageMeta() error = %v", err)
	}

	meta, err := PinBaseImage(root, "mirror.local/"+testImageRefA, testImageDigestA)
	if err != nil {
		t.Fatalf("PinBaseImage() error = %v", err)
	}
	if !meta.CreatedAt.Equal(original.CreatedAt) || meta.RebaseCount != 3 {
		t.Errorf("PinBaseImage() = %+v, want the persisted pin preserved", meta)
	}
}

func TestPinBaseImageRejectsDigestMismatch(t *testing.T) {
	root := newRootDisk(t)
	if err := os.WriteFile(filepath.Join(root, UpperDir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteBaseImageMeta(root, BaseImageMeta{ImageRef: testImageRefA, ImageDigest: testImageDigestA}); err != nil {
		t.Fatal(err)
	}

	_, err := PinBaseImage(root, testImageRefB, testImageDigestB)
	if err == nil {
		t.Fatal("PinBaseImage() succeeded on a digest mismatch, want error")
	}
	if !strings.Contains(err.Error(), testImageDigestA) || !strings.Contains(err.Error(), testImageRefB) {
		t.Errorf("error does not mention both images: %v", err)
	}
}

func TestPinBaseImageRefusesDataWithoutMeta(t *testing.T) {
	root := newRootDisk(t)
	if err := os.WriteFile(filepath.Join(root, UpperDir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PinBaseImage(root, testImageRefA, testImageDigestA); err == nil {
		t.Fatal("PinBaseImage() mounted data with no recorded base image, want error")
	}
}

func TestPinBaseImageRepinsEmptyUpper(t *testing.T) {
	root := newRootDisk(t)
	if err := WriteBaseImageMeta(root, BaseImageMeta{
		ImageRef:    testImageRefA,
		ImageDigest: testImageDigestA,
		RebaseCount: 2,
	}); err != nil {
		t.Fatal(err)
	}

	meta, err := PinBaseImage(root, testImageRefB, testImageDigestB)
	if err != nil {
		t.Fatalf("PinBaseImage() error = %v", err)
	}
	if meta.ImageDigest != testImageDigestB || meta.RebaseCount != 2 {
		t.Errorf("PinBaseImage() = %+v, want the new digest and carried RebaseCount", meta)
	}
}

func TestClearUpperEmptiesWritableLayers(t *testing.T) {
	root := newRootDisk(t)
	for _, dir := range []string{UpperDir, WorkDir, MergedDir} {
		if err := os.WriteFile(filepath.Join(root, dir, "leftover"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteBaseImageMeta(root, BaseImageMeta{ImageRef: testImageRefA, ImageDigest: testImageDigestA}); err != nil {
		t.Fatal(err)
	}

	if err := ClearUpper(root); err != nil {
		t.Fatalf("ClearUpper() error = %v", err)
	}

	empty, err := UpperIsEmpty(root)
	if err != nil || !empty {
		t.Errorf("UpperIsEmpty() = (%v, %v), want (true, nil)", empty, err)
	}
	// work/ and merged/ go with their contents: the next Setup recreates them.
	for _, dir := range []string{WorkDir, MergedDir} {
		if _, err := os.Stat(filepath.Join(root, dir)); !os.IsNotExist(err) {
			t.Errorf("%s/ still exists (err %v), want it removed with its contents", dir, err)
		}
	}

	// The pin itself stays put: only a boot decides which image the disk joins.
	meta, err := ReadBaseImageMeta(root)
	if err != nil {
		t.Fatalf("ReadBaseImageMeta() error = %v", err)
	}
	if meta.ImageDigest != testImageDigestA {
		t.Errorf("pin = %q, want it untouched by ClearUpper", meta.ImageDigest)
	}
}

// RebaseUpper is the exception to the digest invariant: the whole point of a
// rebase is that the old upper layer does not belong to the new base image.
func TestRebaseUpperReplacesALayerOnAnotherImage(t *testing.T) {
	root := newRootDisk(t)
	if err := os.WriteFile(filepath.Join(root, UpperDir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteBaseImageMeta(root, BaseImageMeta{
		ImageRef:    testImageRefA,
		ImageDigest: testImageDigestA,
		RebaseCount: 1,
	}); err != nil {
		t.Fatal(err)
	}

	meta, err := RebaseUpper(root, testImageRefB, testImageDigestB)
	if err != nil {
		t.Fatalf("RebaseUpper() error = %v", err)
	}
	if meta.ImageDigest != testImageDigestB || meta.ImageRef != testImageRefB {
		t.Errorf("RebaseUpper() = %+v, want the new image", meta)
	}
	if meta.RebaseCount != 2 {
		t.Errorf("RebaseCount = %d, want the previous count incremented", meta.RebaseCount)
	}

	empty, err := UpperIsEmpty(root)
	if err != nil || !empty {
		t.Errorf("UpperIsEmpty() = (%v, %v), want (true, nil)", empty, err)
	}

	// The disk must pass the regular digest check afterwards, or every later
	// start of the same image would be refused.
	if _, err := PinBaseImage(root, testImageRefB, testImageDigestB); err != nil {
		t.Errorf("PinBaseImage() after a rebase error = %v", err)
	}
}

func TestRebaseUpperStartsWithNoMeta(t *testing.T) {
	root := newRootDisk(t)
	meta, err := RebaseUpper(root, testImageRefA, testImageDigestA)
	if err != nil {
		t.Fatalf("RebaseUpper() error = %v", err)
	}
	if meta.RebaseCount != 1 {
		t.Errorf("RebaseCount = %d, want 1 when no pin existed", meta.RebaseCount)
	}
}

func TestRebaseUpperRequiresIdentity(t *testing.T) {
	root := newRootDisk(t)
	if _, err := RebaseUpper(root, "", testImageDigestA); err == nil {
		t.Error("RebaseUpper() accepted an empty image reference")
	}
	if _, err := RebaseUpper(root, testImageRefA, ""); err == nil {
		t.Error("RebaseUpper() accepted an empty digest")
	}
}

func TestPinBaseImageRequiresIdentity(t *testing.T) {
	root := t.TempDir()
	if _, err := PinBaseImage(root, "", testImageDigestA); err == nil {
		t.Error("PinBaseImage() accepted an empty image reference")
	}
	if _, err := PinBaseImage(root, testImageRefA, ""); err == nil {
		t.Error("PinBaseImage() accepted an empty digest")
	}
}

func TestGenerationMetaRoundTrip(t *testing.T) {
	root := newRootDisk(t)
	want := GenerationMeta{MountedAt: time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC), Generation: 7, CleanUnmount: true}
	if err := WriteGenerationMeta(root, want); err != nil {
		t.Fatalf("WriteGenerationMeta() error = %v", err)
	}
	got, err := ReadGenerationMeta(root)
	if err != nil {
		t.Fatalf("ReadGenerationMeta() error = %v", err)
	}
	if got.Generation != want.Generation || got.CleanUnmount != want.CleanUnmount || !got.MountedAt.Equal(want.MountedAt) {
		t.Errorf("ReadGenerationMeta() = %+v, want %+v", got, want)
	}
}

func TestReadBaseImageMetaMissing(t *testing.T) {
	if _, err := ReadBaseImageMeta(t.TempDir()); !os.IsNotExist(err) {
		t.Errorf("ReadBaseImageMeta() error = %v, want fs.ErrNotExist", err)
	}
}
