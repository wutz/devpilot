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
	"strings"
	"testing"
)

func TestOverlayOptionsDisablesMetadataOnlyFeatures(t *testing.T) {
	opts := OverlayOptions("/run/devpilot/base-lower", "/mnt/root/upper", "/mnt/root/work")

	for _, want := range []string{
		"lowerdir=/run/devpilot/base-lower",
		"upperdir=/mnt/root/upper",
		"workdir=/mnt/root/work",
		"metacopy=off",
		"index=off",
	} {
		if !strings.Contains(opts, want) {
			t.Errorf("options %q missing %q", opts, want)
		}
	}
	// metacopy=on would archive zero-byte stubs in the commit builder.
	if strings.Contains(opts, "metacopy=on") || strings.Contains(opts, "index=on") {
		t.Errorf("options %q enable a mode the commit builder cannot handle", opts)
	}
}

func TestPathsLayout(t *testing.T) {
	p := Paths{Root: "/mnt/.devpilot-rootdisk/", Lower: BaseLower}
	cases := map[string]string{
		p.Upper():  "/mnt/.devpilot-rootdisk/upper",
		p.Work():   "/mnt/.devpilot-rootdisk/work",
		p.Merged(): "/mnt/.devpilot-rootdisk/merged",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	}
	if !strings.Contains(p.Options(), "upperdir="+p.Upper()) {
		t.Errorf("Options() %q does not use the layout paths", p.Options())
	}
}

func TestCheckOverlaySupport(t *testing.T) {
	cases := []struct {
		name string
		p    Paths
		ok   bool
	}{
		{"valid", Paths{Root: "/mnt/root", Lower: BaseLower}, true},
		{"relative root", Paths{Root: "root", Lower: BaseLower}, false},
		{"empty lower", Paths{Root: "/mnt/root"}, false},
		{"comma in root", Paths{Root: "/mnt/ro,ot", Lower: BaseLower}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckOverlaySupport(c.p)
			if c.ok && err != nil {
				t.Errorf("CheckOverlaySupport() error = %v", err)
			}
			if !c.ok && err == nil {
				t.Error("CheckOverlaySupport() succeeded, want error")
			}
		})
	}
}
