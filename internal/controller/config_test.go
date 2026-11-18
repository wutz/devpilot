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
	"reflect"
	"testing"
	"time"
)

const testInitImage = "ghcr.io/wutz/devpilot-init:v0.1.0"

func TestParseSnapshotClassMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "empty input yields empty map",
			raw:  "",
			want: map[string]string{},
		},
		{
			name: "blank input yields empty map",
			raw:  "   ",
			want: map[string]string{},
		},
		{
			name: "single pair",
			raw:  "block-nvme=csi-rbdplugin-snapclass",
			want: map[string]string{testStorageClassNVMe: testSnapshotClassRBD},
		},
		{
			name: "multiple pairs with whitespace and trailing comma",
			raw:  " block-nvme = csi-rbdplugin-snapclass , block-vast-cs=vast-snapshot-cs , ",
			want: map[string]string{
				testStorageClassNVMe: testSnapshotClassRBD,
				testStorageClassVast: testSnapshotClassVast,
			},
		},
		{
			name: "duplicate identical pair is accepted",
			raw:  "block-nvme=csi-rbdplugin-snapclass,block-nvme=csi-rbdplugin-snapclass",
			want: map[string]string{testStorageClassNVMe: testSnapshotClassRBD},
		},
		{
			name:    "missing separator is rejected",
			raw:     testStorageClassNVMe,
			wantErr: true,
		},
		{
			name:    "empty storage class is rejected",
			raw:     "=csi-rbdplugin-snapclass",
			wantErr: true,
		},
		{
			name:    "empty snapshot class is rejected",
			raw:     "block-nvme=",
			wantErr: true,
		},
		{
			name:    "conflicting mappings for one storage class are rejected",
			raw:     "block-nvme=a,block-nvme=b",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseSnapshotClassMapping(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseSnapshotClassMapping(%q) = %v, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSnapshotClassMapping(%q) returned unexpected error: %v", tt.raw, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseSnapshotClassMapping(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestConfigSnapshotClass(t *testing.T) {
	t.Parallel()

	cfg := Config{DefaultSnapshotClasses: map[string]string{testStorageClassNVMe: testSnapshotClassRBD}}

	got, err := cfg.SnapshotClass(testStorageClassNVMe)
	if err != nil {
		t.Fatalf("SnapshotClass("+testStorageClassNVMe+") returned unexpected error: %v", err)
	}
	if got != testSnapshotClassRBD {
		t.Fatalf("SnapshotClass("+testStorageClassNVMe+") = %q, want %q", got, testSnapshotClassRBD)
	}

	if _, err := cfg.SnapshotClass(testStorageClassVast); err == nil {
		t.Fatal("SnapshotClass(block-vast-cs) = nil error, want error for unconfigured storage class")
	}
	if _, err := cfg.SnapshotClass(""); err == nil {
		t.Fatal("SnapshotClass(\"\") = nil error, want error for empty storage class")
	}
}

func TestConfigSnapshotClassMappingIsStable(t *testing.T) {
	t.Parallel()

	cfg := Config{DefaultSnapshotClasses: map[string]string{
		testStorageClassVast: testSnapshotClassVast,
		testStorageClassNVMe: testSnapshotClassRBD,
	}}
	want := "block-nvme=csi-rbdplugin-snapclass,block-vast-cs=vast-snapshot-cs"
	for range 20 {
		if got := cfg.SnapshotClassMapping(); got != want {
			t.Fatalf("SnapshotClassMapping() = %q, want %q", got, want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	if err := (Config{InitImage: testInitImage}).Validate(); err != nil {
		t.Fatalf("Validate() with init image returned unexpected error: %v", err)
	}
	if err := (Config{InitImage: "  "}).Validate(); err == nil {
		t.Fatal("Validate() without init image returned nil error, want error")
	}
}

func TestConfigBuildNamespaceSuffix(t *testing.T) {
	t.Parallel()

	if got := (Config{}).BuildNamespaceSuffixMode(); got != "" {
		t.Fatalf("BuildNamespaceSuffixMode() = %q, want empty when isolation is off", got)
	}
	if got := (Config{BuildNamespaceSuffix: "  "}).BuildNamespaceSuffixMode(); got != "" {
		t.Fatalf("BuildNamespaceSuffixMode() = %q, want empty for blank input", got)
	}
	if got := (Config{BuildNamespaceSuffix: BuildNamespaceSuffixDefault}).BuildNamespaceSuffixMode(); got != BuildNamespaceSuffixDefault {
		t.Fatalf("BuildNamespaceSuffixMode() = %q, want %q", got, BuildNamespaceSuffixDefault)
	}

	base := Config{InitImage: testInitImage}
	for _, suffix := range []string{BuildNamespaceSuffixDefault, "-ci", "build"} {
		withSuffix := base
		withSuffix.BuildNamespaceSuffix = suffix
		if err := withSuffix.Validate(); err != nil {
			t.Fatalf("Validate() with suffix %q returned unexpected error: %v", suffix, err)
		}
	}
	for _, suffix := range []string{"-build/extra", " build", "foo:bar"} {
		withSuffix := base
		withSuffix.BuildNamespaceSuffix = suffix
		if err := withSuffix.Validate(); err == nil {
			t.Fatalf("Validate() with suffix %q returned nil error, want error", suffix)
		}
	}
}

func TestConfigPrewarmNamespaceOrDefault(t *testing.T) {
	t.Setenv("DEVPILOT_NAMESPACE", "")
	if got := (Config{}).PrewarmNamespaceOrDefault(); got != DefaultPrewarmNamespace {
		t.Fatalf("PrewarmNamespaceOrDefault() = %q, want %q", got, DefaultPrewarmNamespace)
	}

	t.Setenv("DEVPILOT_NAMESPACE", "operator-ns")
	if got := (Config{}).PrewarmNamespaceOrDefault(); got != "operator-ns" {
		t.Fatalf("PrewarmNamespaceOrDefault() = %q, want operator-ns", got)
	}

	if got := (Config{PrewarmNamespace: "custom-prewarm"}).PrewarmNamespaceOrDefault(); got != "custom-prewarm" {
		t.Fatalf("PrewarmNamespaceOrDefault() = %q, want custom-prewarm", got)
	}
}

func TestParsePrepullThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw     string
		want    int64
		wantErr bool
	}{
		{raw: "", want: 0},
		{raw: "0", want: 0},
		{raw: "20Gi", want: 20 << 30},
		{raw: "512Mi", want: 512 << 20},
		{raw: "-1Gi", wantErr: true},
		{raw: "not-a-size", wantErr: true},
	}
	for _, tc := range tests {
		got, err := ParsePrepullThreshold(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParsePrepullThreshold(%q) = %d, want error", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParsePrepullThreshold(%q) error = %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("ParsePrepullThreshold(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}

	cfg := Config{InitImage: testInitImage, PrepullThresholdBytes: -1}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted negative PrepullThresholdBytes")
	}
}

func TestConfigCommitFreezeWindow(t *testing.T) {
	if got := (Config{}).CommitFreezeWindow(); got != DefaultCommitFreezeTimeout {
		t.Errorf("CommitFreezeWindow() = %s, want the %s default", got, DefaultCommitFreezeTimeout)
	}
	if got := (Config{CommitFreezeTimeout: 5 * time.Second}).CommitFreezeWindow(); got != 5*time.Second {
		t.Errorf("CommitFreezeWindow() = %s, want 5s", got)
	}
	if err := (Config{InitImage: "init", CommitFreezeTimeout: -time.Second}).Validate(); err == nil {
		t.Error("Validate() accepted a negative freeze timeout")
	}
}
