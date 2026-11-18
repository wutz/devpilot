//go:build linux

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

package loopdev

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func sectors(t *testing.T, d *Device) int64 {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/sys/block/loop%d/size", d.Number))
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestLinuxAttach needs root and a directory on a filesystem with O_DIRECT
// (see hack/loop-csi-e2e.sh).
func TestLinuxAttach(t *testing.T) {
	dir := os.Getenv("DEVPILOT_LOOP_E2E_POOL")
	if dir == "" || os.Geteuid() != 0 {
		t.Skip("needs root and DEVPILOT_LOOP_E2E_POOL")
	}
	img := filepath.Join(dir, fmt.Sprintf("loopdev-%d.img", os.Getpid()))
	if err := os.WriteFile(img, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(img) }()
	if err := os.Truncate(img, 64<<20); err != nil {
		t.Fatal(err)
	}

	d, h, err := Attach(img, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !d.DirectIO {
		t.Error("direct I/O not enabled on a 4096-byte block device over ext4")
	}
	if got := sectors(t, d); got != 64<<20/512 {
		t.Errorf("device has %d sectors", got)
	}
	found, err := FindByBackingFile(img)
	if err != nil || len(found) != 1 || found[0].Path != d.Path {
		t.Fatalf("FindByBackingFile = %+v, %v", found, err)
	}
	if st, err := Status(d.Path); err != nil || !st.DirectIO {
		t.Errorf("Status = %+v, %v", st, err)
	}

	if err := os.Truncate(img, 128<<20); err != nil {
		t.Fatal(err)
	}
	if err := SetCapacity(d.Path); err != nil {
		t.Fatal(err)
	}
	if got := sectors(t, d); got != 128<<20/512 {
		t.Errorf("after SetCapacity the device has %d sectors", got)
	}

	// Autoclear: closing the last opener detaches the device.
	_ = h.Close()
	if found, _ := FindByBackingFile(img); len(found) != 0 {
		// Some kernels clear asynchronously; an explicit detach must work.
		for _, f := range found {
			if err := Detach(f.Path); err != nil {
				t.Fatal(err)
			}
		}
	}
	if found, _ := FindByBackingFile(img); len(found) != 0 {
		t.Fatalf("device survived close and detach: %+v", found)
	}

	ro, h2, err := Attach(img, Options{ReadOnly: true, DirectIO: DirectIOPreferred})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h2.Close() }()
	flag, err := os.ReadFile(fmt.Sprintf("/sys/block/loop%d/ro", ro.Number))
	if err != nil || strings.TrimSpace(string(flag)) != "1" {
		t.Errorf("read-only attach: /sys/block/loop%d/ro = %q, %v", ro.Number, flag, err)
	}
}
