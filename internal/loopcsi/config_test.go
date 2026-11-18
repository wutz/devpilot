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

package loopcsi

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wutz/devpilot/internal/loopdev"
)

func TestParsePool(t *testing.T) {
	p, err := ParsePool("gpfs=/mnt/gpfs/devpilot/")
	if err != nil || p.Name != "gpfs" || p.Path != "/mnt/gpfs/devpilot" {
		t.Fatalf("ParsePool = %+v, %v", p, err)
	}
	for _, bad := range []string{"gpfs", "GPFS=/x", "gpfs=relative", "=/x", "a_b=/x"} {
		if _, err := ParsePool(bad); err == nil {
			t.Errorf("ParsePool(%q) succeeded", bad)
		}
	}
	pools, err := NewPools([]Pool{{Name: "a", Path: "/a"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := pools.Resolve(""); err != nil || got.Name != "a" {
		t.Errorf("Resolve(\"\") with one pool = %+v, %v", got, err)
	}
	if _, err := NewPools([]Pool{{Name: "a"}, {Name: "a"}}); err == nil {
		t.Error("duplicate pool accepted")
	}
	two, _ := NewPools([]Pool{{Name: "a", Path: "/a"}, {Name: "b", Path: "/b"}})
	if _, err := two.Resolve(""); err == nil {
		t.Error("Resolve(\"\") with two pools succeeded")
	}
	if _, err := two.Resolve("c"); err == nil {
		t.Error("unknown pool resolved")
	}
}

func TestIDs(t *testing.T) {
	const testPool = "vast"
	v, err := ParseVolumeID(testPool + "/pvc-0b1c")
	if err != nil || v.Pool != testPool || v.Name != "pvc-0b1c" ||
		v.String() != testPool+"/pvc-0b1c" || v.Key() != testPool+"_pvc-0b1c" {
		t.Fatalf("ParseVolumeID = %+v, %v", v, err)
	}
	for _, bad := range []string{testPool, testPool + "/", testPool + "/.snapshots", testPool + "/a/b", "Vast/x", testPool + "/../x"} {
		if _, err := ParseVolumeID(bad); err == nil {
			t.Errorf("ParseVolumeID(%q) succeeded", bad)
		}
	}
	for _, raw := range []string{"p/snap/snapshot-1", "p/snapdir/pvc-1/snapshot-1"} {
		s, err := ParseSnapshotID(raw)
		if err != nil || s.String() != raw {
			t.Errorf("ParseSnapshotID(%q) = %+v, %v", raw, s, err)
		}
	}
	for _, bad := range []string{"p/snap", "p/snapdir/x", "p/other/x", "p/snap/a/b", "p/snap/.."} {
		if _, err := ParseSnapshotID(bad); err == nil {
			t.Errorf("ParseSnapshotID(%q) succeeded", bad)
		}
	}
}

func TestRoundSize(t *testing.T) {
	cases := []struct {
		req, limit, want int64
		err              bool
	}{
		{0, 0, MinVolumeSize, false},
		{MinVolumeSize + 1, 0, MinVolumeSize + sizeAlign, false},
		{50 << 30, 0, 50 << 30, false},
		{0, 512 << 20, 0, true},
		{MinVolumeSize + 1, MinVolumeSize + 10, 0, true},
		{MinVolumeSize + 1, 2 * MinVolumeSize, MinVolumeSize + sizeAlign, false},
		{-1, 0, 0, true},
	}
	for _, c := range cases {
		got, err := RoundSize(c.req, c.limit)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("RoundSize(%d, %d) = %d, %v; want %d, err=%v", c.req, c.limit, got, err, c.want, c.err)
		}
	}
}

func TestParseParams(t *testing.T) {
	const testPool = "vast"
	p, err := ParseVolumeParams(map[string]string{
		"pool": testPool, "directIO": "preferred", "trimOnUnmount": "false", "trimMinLength": "65536", "mmp": "false",
		"csi.storage.k8s.io/pv/name": "pvc-1",
		// external-provisioner writes this into the PV's volumeAttributes and
		// kubelet passes it to NodePublishVolume in the volume context.
		"storage.kubernetes.io/csiProvisionerIdentity": "1794922185608-3190-loop.devpilot.wutz.dev",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Pool != testPool || p.DirectIO != loopdev.DirectIOPreferred || p.TrimOnUnmount || p.TrimMinLength != 65536 || p.MMP {
		t.Fatalf("ParseVolumeParams = %+v", p)
	}
	back, err := ParseVolumeParams(p.Context())
	if err != nil || back.DirectIO != p.DirectIO || back.TrimMinLength != p.TrimMinLength {
		t.Fatalf("context round trip = %+v, %v", back, err)
	}
	d, _ := ParseVolumeParams(nil)
	if d.DirectIO != loopdev.DirectIORequired || !d.TrimOnUnmount || !d.MMP {
		t.Errorf("defaults = %+v", d)
	}
	for _, bad := range []map[string]string{{"directIO": "yes"}, {"typo": "1"}, {"trimMinLength": "-1"}, {"storage.kubernetes.ioX/csiProvisionerIdentity": "1"}} {
		if _, err := ParseVolumeParams(bad); err == nil {
			t.Errorf("ParseVolumeParams(%v) succeeded", bad)
		}
	}
	const testTrue = "true"
	s, err := ParseSnapshotParams(map[string]string{ParamSnapshotStrategy: "cephfs-snapdir", ParamAllowLiveCopy: testTrue})
	if err != nil || s.Strategy != StrategyCephSnapDir || !s.AllowLiveCopy {
		t.Fatalf("ParseSnapshotParams = %+v, %v", s, err)
	}
	if _, err := ParseSnapshotParams(map[string]string{ParamSnapshotStrategy: "lvm"}); err == nil {
		t.Error("bad strategy accepted")
	}
}

func TestLease(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, LeaseName)
	if l, err := readLease(p); err != nil || l != nil {
		t.Fatalf("missing lease = %v, %v", l, err)
	}
	now := time.Now()
	if err := writeLease(p, &Lease{Node: "a", VolumeID: "p/v", Heartbeat: now}); err != nil {
		t.Fatal(err)
	}
	l, err := readLease(p)
	if err != nil || l.Node != "a" {
		t.Fatalf("readLease = %+v, %v", l, err)
	}
	if !l.ForeignAndFresh("b", now.Add(10*time.Second), 45*time.Second) {
		t.Error("fresh foreign lease not detected")
	}
	if l.ForeignAndFresh("b", now.Add(time.Minute), 45*time.Second) {
		t.Error("stale lease treated as fresh")
	}
	if l.ForeignAndFresh("a", now, 45*time.Second) {
		t.Error("own lease treated as foreign")
	}
	if err := removeLease(p, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Error("lease of another node removed")
	}
	if err := removeLease(p, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("own lease not removed")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

func TestLockConflicts(t *testing.T) {
	p := filepath.Join(t.TempDir(), ImageName)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	open := func() *os.File {
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	if held, _, err := probeLock(p); err != nil || held {
		t.Fatalf("probe of unlocked file = %v, %v", held, err)
	}
	w := open()
	if err := lockFile(w, true); err != nil {
		t.Fatal(err)
	}
	// Two descriptions in one process conflict: this stands in for two nodes.
	r := open()
	if err := lockFile(r, false); err != ErrLocked {
		t.Fatalf("shared lock under exclusive = %v", err)
	}
	if held, excl, _ := probeLock(p); !held || !excl {
		t.Errorf("probe = %v, %v; want held exclusive", held, excl)
	}
	_ = w.Close()
	if err := lockFile(r, false); err != nil {
		t.Fatalf("shared lock after release = %v", err)
	}
	r2 := open()
	defer func() { _ = r2.Close() }()
	if err := lockFile(r2, false); err != nil {
		t.Fatalf("second shared lock = %v", err)
	}
	if held, excl, _ := probeLock(p); !held || excl {
		t.Errorf("probe = %v, %v; want held shared", held, excl)
	}
	w2 := open()
	defer func() { _ = w2.Close() }()
	if err := lockFile(w2, true); err != ErrLocked {
		t.Fatalf("exclusive under shared = %v", err)
	}
	_ = r.Close()
}

// fakeSuperblock writes an ext4 superblock with the given geometry.
func fakeSuperblock(t *testing.T, path string, size int64, mmp, errs bool) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sb := make([]byte, sbSize)
	le := binary.LittleEndian
	blocks := uint64(size / 4096)
	le.PutUint32(sb[offBlocksCountLo:], uint32(blocks))
	le.PutUint32(sb[offBlocksCountHi:], uint32(blocks>>32))
	le.PutUint32(sb[offLogBlockSize:], 2)
	le.PutUint16(sb[offMagic:], sbMagic)
	state := uint16(1)
	if errs {
		state |= sbStateErrors
	}
	le.PutUint16(sb[offState:], state)
	incompat := uint32(incompat64Bit)
	if mmp {
		incompat |= incompatMMP
	}
	le.PutUint32(sb[offIncompat:], incompat)
	if _, err := f.WriteAt(sb, sbOffset); err != nil {
		t.Fatal(err)
	}
}

func TestSuperblock(t *testing.T) {
	p := filepath.Join(t.TempDir(), ImageName)
	if err := os.WriteFile(p, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSuperblock(p); err != errNoExt4 {
		t.Fatalf("zero file = %v", err)
	}
	fakeSuperblock(t, p, 20<<40, true, true)
	sb, err := readSuperblock(p)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Size() != 20<<40 || !sb.MMP || !sb.Errors || sb.BlockSize != 4096 {
		t.Errorf("superblock = %+v", sb)
	}
}

func TestSparseCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	const size = 8 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	marks := map[int64][]byte{0: []byte("head"), 3<<20 + 17: []byte("middle"), size - 4: []byte("tail")}
	for off, b := range marks {
		if _, err := f.WriteAt(b, off); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	dst := filepath.Join(dir, "dst")
	method, err := copyImage(src, dst, StrategyAuto)
	if err != nil {
		t.Fatal(err)
	}
	if method != CopySparse && method != CopyReflink {
		t.Errorf("method = %q", method)
	}
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(dst)
	if !bytes.Equal(a, b) {
		t.Fatal("copy differs from source")
	}
	if _, err := os.Stat(dst + ".copying"); !os.IsNotExist(err) {
		t.Error("temporary copy left behind")
	}
	if _, err := copyImage(src, filepath.Join(dir, "r"), StrategyReflink); err != nil && !strings.Contains(err.Error(), "reflink") {
		t.Errorf("reflink failure not reported as such: %v", err)
	}
}

func TestParseMountInfo(t *testing.T) {
	info := `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
30 22 7:3 / /var/lib/kubelet/plugins/x/mounts/p_v rw,noatime shared:5 - ext4 /dev/loop3 rw,errors=remount-ro
31 22 7:3 / /var/lib/kubelet/pods/u/volumes/kubernetes.io~csi/pvc\040x/mount rw,noatime shared:5 - ext4 /dev/loop3 ro,errors=remount-ro
`
	st, err := parseMountInfo(strings.NewReader(info), "/var/lib/kubelet/plugins/x/mounts/p_v")
	if err != nil || !st.Mounted || st.ReadOnly {
		t.Errorf("private mount = %+v, %v", st, err)
	}
	st, _ = parseMountInfo(strings.NewReader(info), "/var/lib/kubelet/pods/u/volumes/kubernetes.io~csi/pvc x/mount")
	if !st.Mounted || !st.ReadOnly {
		t.Errorf("remounted-ro target = %+v", st)
	}
	st, _ = parseMountInfo(strings.NewReader(info), "/nope")
	if st.Mounted {
		t.Error("unmounted path reported mounted")
	}
}
