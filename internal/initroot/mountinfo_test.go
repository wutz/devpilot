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

const (
	testDevPts = "/dev/pts"
)

const mountInfoSample = `22 1 0:21 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
23 1 0:22 / /dev rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755
24 23 0:23 / /dev/pts rw,nosuid,noexec,relatime - devpts devpts rw,gid=5,mode=620
25 23 0:24 / /dev/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k
26 1 0:26 / /sys/fs/cgroup ro,nosuid - cgroup2 cgroup2 rw,nsdelegate
27 24 0:27 / /usr/bin/nvidia-smi ro,relatime - ext4 /dev/nvme0n1p1 rw
28 1 0:28 / /mnt/.devpilot-rootdisk rw - ext4 /dev/rbd0 rw
29 1 0:29 / /run/devpilot rw - tmpfs tmpfs rw
30 1 0:30 / /var/run/secrets/kubernetes.io/serviceaccount ro - tmpfs tmpfs rw
31 1 0:31 / /etc/hosts\040with\040space rw - ext4 /dev/vda1 rw
`

func TestParseMountInfo(t *testing.T) {
	entries, err := ParseMountInfo(strings.NewReader(mountInfoSample))
	if err != nil {
		t.Fatalf("ParseMountInfo() error = %v", err)
	}
	if len(entries) != 10 {
		t.Fatalf("ParseMountInfo() returned %d entries, want 10", len(entries))
	}

	pts := entries[2]
	if pts.Target != testDevPts || pts.FSType != "devpts" || pts.ParentID != 23 || pts.MountID != 24 {
		t.Errorf("unexpected /dev/pts entry: %+v", pts)
	}

	space := entries[9]
	if space.Target != "/etc/hosts with space" {
		t.Errorf("octal escape not decoded: %q", space.Target)
	}

	smi := entries[5]
	if smi.Source != "/dev/nvme0n1p1" || smi.SuperOpts != "rw" || smi.Options != "ro,relatime" {
		t.Errorf("unexpected nvidia-smi entry: %+v", smi)
	}
}

func TestParseMountInfoRejectsMalformedLines(t *testing.T) {
	for _, line := range []string{
		"22 1 0:21 / /proc rw,nosuid",
		"22 1 0:21 / /proc",
	} {
		if _, err := ParseMountInfo(strings.NewReader(line)); err == nil {
			t.Errorf("ParseMountInfo(%q) succeeded, want error", line)
		}
	}
}

func TestUnescapeMountPath(t *testing.T) {
	cases := map[string]string{
		"/plain":              "/plain",
		`/with\040space`:      "/with space",
		`/tab\011here`:        "/tab\there",
		`/newline\012`:        "/newline\n",
		`/backslash\134`:      `/backslash\`,
		`/truncated\04`:       `/truncated\04`,
		`/nonoctal\089`:       `/nonoctal\089`,
		`/kubernetes.io/auth`: "/kubernetes.io/auth",
	}
	for in, want := range cases {
		if got := unescapeMountPath(in); got != want {
			t.Errorf("unescapeMountPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSubmountsUnderRootSortsParentsFirst(t *testing.T) {
	entries, err := ParseMountInfo(strings.NewReader(mountInfoSample))
	if err != nil {
		t.Fatalf("ParseMountInfo() error = %v", err)
	}

	got := SubmountsUnderRoot(entries, "/mnt/.devpilot-rootdisk", "/run/devpilot")
	targets := make([]string, 0, len(got))
	for _, e := range got {
		targets = append(targets, e.Target)
	}

	// Depth first, then lexical, so /dev precedes /dev/pts and /dev/shm.
	want := []string{
		"/dev",
		"/proc",
		testDevPts,
		"/dev/shm",
		"/etc/hosts with space",
		"/sys/fs/cgroup",
		"/usr/bin/nvidia-smi",
		"/var/run/secrets/kubernetes.io/serviceaccount",
	}
	if strings.Join(targets, " ") != strings.Join(want, " ") {
		t.Errorf("SubmountsUnderRoot() = %v, want %v", targets, want)
	}
}

func TestSubmountsUnderRootExcludesChildrenOfExcludedPaths(t *testing.T) {
	entries := []MountEntry{
		{Target: "/"},
		{Target: "/mnt/.devpilot-rootdisk/upper"},
		{Target: "/run/devpilot/base-lower"},
		{Target: "/data"},
		{Target: "relative/path"},
	}
	got := SubmountsUnderRoot(entries, "/mnt/.devpilot-rootdisk", "/run/devpilot")
	if len(got) != 1 || got[0].Target != "/data" {
		t.Errorf("SubmountsUnderRoot() = %+v, want only /data", got)
	}
}

func TestPathDepth(t *testing.T) {
	cases := map[string]int{"/": 0, "/dev": 1, "/dev/pts": 2, "/a/b/c": 3}
	for path, want := range cases {
		if got := pathDepth(path); got != want {
			t.Errorf("pathDepth(%q) = %d, want %d", path, got, want)
		}
	}
}
