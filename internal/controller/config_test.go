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
)

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

	if err := (Config{InitImage: "ghcr.io/wutz/devpilot-init:v0.1.0"}).Validate(); err != nil {
		t.Fatalf("Validate() with init image returned unexpected error: %v", err)
	}
	if err := (Config{InitImage: "  "}).Validate(); err == nil {
		t.Fatal("Validate() without init image returned nil error, want error")
	}
}
