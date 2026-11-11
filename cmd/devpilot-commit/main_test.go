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
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wutz/devpilot/internal/initroot"
)

const (
	testShell   = "/bin/bash"
	testBaseRef = "registry.example.com/base/pytorch:2"
)

func TestSplitCommand(t *testing.T) {
	cases := map[string][]string{
		testShell:                            {testShell},
		"/entry.sh --no-browser":             {"/entry.sh --no-browser"},
		`["/bin/bash", "-lc", "echo hi"]`:    {testShell, "-lc", "echo hi"},
		`sh -c "python train.py --epochs 3"`: {"sh", "-c", "python train.py --epochs 3"},
		`bash -c 'echo "quoted"'`:            {"bash", "-c", `echo "quoted"`},
	}
	for line, want := range cases {
		t.Run(line, func(t *testing.T) {
			got, err := splitCommand(line)
			if err != nil {
				t.Fatalf("splitCommand(%q): %v", line, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("splitCommand(%q) = %#v, want %#v", line, got, want)
			}
		})
	}
}

func TestSplitCommandRejectsMalformedInput(t *testing.T) {
	for _, line := range []string{`["/bin/bash"`, `sh -c "unterminated`, `[testShell, ]`} {
		t.Run(line, func(t *testing.T) {
			if _, err := splitCommand(line); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestBuildChanges(t *testing.T) {
	changes, err := buildChanges("alice", "install einops", "A=1, B=two,EMPTY=", "/opt/app", `sh -c "echo hi"`)
	if err != nil {
		t.Fatalf("buildChanges: %v", err)
	}
	if changes.Author != "alice" || changes.Comment != "install einops" || changes.WorkingDir != "/opt/app" {
		t.Errorf("changes = %+v", changes)
	}
	wantEnv := []struct{ Name, Value string }{{"A", "1"}, {"B", "two"}, {"EMPTY", ""}}
	if len(changes.Env) != len(wantEnv) {
		t.Fatalf("env = %+v, want %d entries", changes.Env, len(wantEnv))
	}
	for i, want := range wantEnv {
		if changes.Env[i].Name != want.Name || changes.Env[i].Value != want.Value {
			t.Errorf("env[%d] = %+v, want %+v", i, changes.Env[i], want)
		}
	}
	if !reflect.DeepEqual(changes.Cmd, []string{"sh", "-c", "echo hi"}) {
		t.Errorf("cmd = %#v", changes.Cmd)
	}
}

func TestBuildChangesRejectsBadEnv(t *testing.T) {
	if _, err := buildChanges("", "", "NOEQUALS", "", ""); err == nil {
		t.Fatal("expected an error for an env entry without a value")
	}
}

// A missing env value is legal in a Dockerfile, so an explicit trailing "="
// must not be rejected.
func TestBuildChangesAllowsEmptyValue(t *testing.T) {
	changes, err := buildChanges("", "", "DEBIAN_FRONTEND=", "", "")
	if err != nil {
		t.Fatalf("buildChanges: %v", err)
	}
	if len(changes.Env) != 1 || changes.Env[0].Value != "" {
		t.Errorf("env = %+v", changes.Env)
	}
}

func TestResolveBasePrefersTheRecordedPin(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, initroot.MetaDir), 0o755); err != nil {
		t.Fatalf("create meta dir: %v", err)
	}
	pin := initroot.BaseImageMeta{
		ImageRef:    testBaseRef,
		ImageDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	data, err := json.Marshal(pin)
	if err != nil {
		t.Fatalf("encode pin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, initroot.MetaDir, initroot.BaseImageFile), data, 0o644); err != nil {
		t.Fatalf("write pin: %v", err)
	}

	image, digest := "flag/ignored:1", "sha256:2222"
	if err := resolveBase(root, &image, &digest); err != nil {
		t.Fatalf("resolveBase: %v", err)
	}
	if image != pin.ImageRef || digest != pin.ImageDigest {
		t.Errorf("base = %s@%s, want the recorded pin", image, digest)
	}
}

// Without a pin the explicit flags are the only source, and a missing base is a
// startup error rather than a mysterious failure during the push.
func TestResolveBaseFallsBackToFlags(t *testing.T) {
	root := t.TempDir()
	image, digest := testBaseRef, "sha256:3333"
	if err := resolveBase(root, &image, &digest); err != nil {
		t.Fatalf("resolveBase: %v", err)
	}
	if image != testBaseRef || digest != "sha256:3333" {
		t.Errorf("base = %s@%s, want the flags unchanged", image, digest)
	}

	empty := ""
	if err := resolveBase(root, &empty, &digest); err == nil {
		t.Fatal("expected an error when no base image is known")
	}
}

func TestReadPassword(t *testing.T) {
	file := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(file, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatalf("write password: %v", err)
	}
	got, err := readPassword("inline", file)
	if err != nil {
		t.Fatalf("readPassword: %v", err)
	}
	if got != "s3cret" {
		t.Errorf("password = %q, want the file contents with the newline stripped", got)
	}

	if got, err := readPassword("inline", ""); err != nil || got != "inline" {
		t.Errorf("readPassword without a file = %q, %v", got, err)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" v1 , ,v2,, v3 ")
	want := []string{"v1", "v2", "v3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitList = %v, want %v", got, want)
	}
	if splitList("  ") != nil {
		t.Error("a blank value should produce no entries")
	}
}
