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
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// captureStderr runs run() with stderr redirected to a pipe and returns its
// exit code and everything written to it.
func captureStderr(t *testing.T, args []string) (int, string) {
	t.Helper()
	return captureStderrCtx(t, context.Background(), args)
}

func captureStderrCtx(t *testing.T, ctx context.Context, args []string) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	code := runWithContext(ctx, args, w)
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

func TestRunNoopExitsZero(t *testing.T) {
	code, out := captureStderr(t, []string{"--noop"})
	if code != 0 {
		t.Errorf("run(--noop) = %d, want 0 (out=%q)", code, out)
	}
	if !strings.Contains(out, "Noop complete") {
		t.Errorf("stderr = %q, want Noop complete", out)
	}
}

func TestRunHoldWaitsUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	code, out := captureStderrCtx(t, ctx, []string{"--hold"})
	if code != 0 {
		t.Errorf("run(--hold) = %d, want 0 (out=%q)", code, out)
	}
	if !strings.Contains(out, "Released sentinel hold") {
		t.Errorf("stderr = %q, want Released sentinel hold", out)
	}
}

func TestRunRejectsFreezeAndThawTogether(t *testing.T) {
	code, out := captureStderr(t, []string{"--freeze", "--thaw"})
	if code != 1 {
		t.Errorf("run() = %d, want 1", code)
	}
	if !strings.Contains(out, "mutually exclusive") {
		t.Errorf("stderr = %q, want the conflict error", out)
	}
}

// Outside a DevBox there is no devpilot-init PID 1 holding the root disk, so
// the helper must refuse without ever announcing a freeze.
func TestRunFreezeWithoutRootDiskNeverFreezes(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout strings.Builder
	code := runIO([]string{"--freeze", "--freeze-timeout=1s"}, strings.NewReader(""), &stdout, w)
	_ = w.Close()
	stderr, _ := io.ReadAll(r)
	_ = r.Close()

	if code != 2 {
		t.Errorf("run(--freeze) = %d, want 2; stderr: %s", code, stderr)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing without a freeze", stdout.String())
	}
	if !strings.Contains(string(stderr), "root disk") {
		t.Errorf("stderr = %q, want the lookup error", stderr)
	}
}

func TestRunThawWithoutRootDiskFails(t *testing.T) {
	code, out := captureStderr(t, []string{"--thaw"})
	if code != 1 {
		t.Errorf("run(--thaw) = %d, want 1", code)
	}
	if !strings.Contains(out, "Could not thaw") {
		t.Errorf("stderr = %q, want the thaw error", out)
	}
}
