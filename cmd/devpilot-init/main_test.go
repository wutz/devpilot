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

package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// captureStderr runs run() with stderr redirected to a pipe and returns its
// exit code and everything written to it.
func captureStderr(t *testing.T, args []string) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	code := run(args, w)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return code, string(out)
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	code, out := captureStderr(t, []string{"--not-a-flag"})
	if code != 1 {
		t.Errorf("run() = %d, want 1", code)
	}
	if !strings.Contains(out, "not-a-flag") {
		t.Errorf("stderr = %q, want the flag error", out)
	}
}

func TestRunRequiresBaseImageIdentity(t *testing.T) {
	t.Setenv("DEVPILOT_BASE_IMAGE", "")
	t.Setenv("DEVPILOT_BASE_IMAGE_DIGEST", "")

	code, out := captureStderr(t, []string{"--root", t.TempDir()})
	if code != 1 {
		t.Errorf("run() = %d, want 1 without a base image", code)
	}
	if !strings.Contains(out, "base image") {
		t.Errorf("stderr = %q, want a base image error", out)
	}
}

func TestRunRefusesRelativeRootDisk(t *testing.T) {
	code, out := captureStderr(t, []string{
		"--root", "relative/path",
		"--image-ref", "pytorch/pytorch:2.4.0",
		"--image-digest", "sha256:aaaa",
	})
	if code != 1 {
		t.Errorf("run() = %d, want 1 for a relative root disk", code)
	}
	if !strings.Contains(out, "must be absolute") {
		t.Errorf("stderr = %q, want the absolute-path error", out)
	}
}
