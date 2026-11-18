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

package loopcsi

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr/testr"
	"google.golang.org/grpc/codes"

	"github.com/wutz/devpilot/internal/commit"
	"github.com/wutz/devpilot/internal/ext4"
	"github.com/wutz/devpilot/internal/loopdev"
)

const (
	testPoolE2E  = "e2e"
	e2eBasePin   = `{"imageRef":"example.com/base:v1","imageDigest":"sha256:abcd"}`
	e2eHelloBody = "from node-a\n"
)

// TestE2ERealKernel drives the controller and two node servers against real
// loop devices, ext4 and e2fsprogs. It needs root, /dev/loop-control and a
// pool directory on a filesystem with O_DIRECT and hole punching (ext4 or
// XFS; not overlayfs or tmpfs). Run it in a privileged container:
//
//	DEVPILOT_LOOP_E2E_POOL=/pool go test -run E2E ./internal/loopcsi
//
// See hack/loop-csi-e2e.sh.
func TestE2ERealKernel(t *testing.T) {
	poolDir := os.Getenv("DEVPILOT_LOOP_E2E_POOL")
	if poolDir == "" {
		t.Skip("DEVPILOT_LOOP_E2E_POOL is not set")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ctx := context.Background()
	poolDir = filepath.Join(poolDir, strings.ReplaceAll(t.Name(), "/", "_")+time.Now().Format("150405.000"))
	if err := os.MkdirAll(poolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pools, _ := NewPools([]Pool{{Name: testPoolE2E, Path: poolDir}})
	log := testr.New(t)
	ctrl := &ControllerServer{Pools: pools, Log: log}
	newNode := func(name string) *NodeServer {
		n := &NodeServer{NodeID: name, Pools: pools, StateDir: filepath.Join(t.TempDir(), name), Host: LinuxHost{}, LeaseInterval: time.Second, Log: log.WithName(name)}
		if err := n.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return n
	}
	a, b := newNode("node-a"), newNode("node-b")

	vol, img, ta := e2eCreateAndMount(ctx, t, ctrl, a, pools[testPoolE2E])
	big := e2ePopulateVolume(t, ta)

	// Guard 1: the OFD lock held by the loop device.
	tb := filepath.Join(t.TempDir(), "b")
	wantCode(t, publish(b, vol, tb, false), codes.FailedPrecondition, "second writer")
	wantCode(t, publish(b, vol, tb, true), codes.FailedPrecondition, "raw reader while mounted")

	// Guard 3: MMP stops a mount that bypasses the lock and the lease.
	e2eTestMMPGuard(t, img)

	// Online expansion.
	e2eTestExpand(ctx, t, ctrl, a, vol.GetVolumeId(), ta)

	// Online snapshot under a freeze, restore, and read raw via ext4 + commit.WriteLayer.
	restoredID, snapID := e2eTestSnapshotRestoreAndCommit(ctx, t, ctrl, b, pools[testPoolE2E], vol.GetVolumeId(), ta)

	// Trim on unpublish and takeover on node-b.
	e2eTestTrimAndTakeover(t, a, b, vol, img, ta, tb, big)

	for _, id := range []string{vol.GetVolumeId(), restoredID} {
		if _, err := ctrl.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ctrl.DeleteSnapshot(ctx, &csi.DeleteSnapshotRequest{SnapshotId: snapID}); err != nil {
		t.Fatal(err)
	}
}

func e2eCreateAndMount(ctx context.Context, t *testing.T, ctrl *ControllerServer, node *NodeServer, pool Pool) (*csi.Volume, string, string) {
	t.Helper()
	resp, err := ctrl.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name: "pvc-e2e", VolumeCapabilities: []*csi.VolumeCapability{rwCap},
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30}, Parameters: map[string]string{ParamPool: testPoolE2E},
	})
	if err != nil {
		t.Fatal(err)
	}
	vol := resp.GetVolume()
	img := imagePath(pool, "pvc-e2e")
	sb, err := readSuperblock(img)
	if err != nil || !sb.MMP || sb.Size() != 1<<30 {
		t.Fatalf("created image superblock = %+v, %v", sb, err)
	}
	var st syscall.Stat_t
	_ = syscall.Stat(img, &st)
	t.Logf("fresh 1GiB image allocates %d KiB", st.Blocks/2)

	ta := filepath.Join(t.TempDir(), "a")
	if err := publish(node, vol, ta, false); err != nil {
		t.Fatal(err)
	}
	loops, err := loopdev.FindByBackingFile(img)
	if err != nil || len(loops) != 1 {
		t.Fatalf("loops for image = %+v, %v", loops, err)
	}
	if !loops[0].DirectIO {
		t.Fatalf("loop device %s is buffered", loops[0].Path)
	}
	return vol, img, ta
}

func e2ePopulateVolume(t *testing.T, ta string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ta, "hello"), []byte(e2eHelloBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ta, "upper", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ta, "upper", "app", "run.sh"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mknod(filepath.Join(ta, "upper", "app", "wh"), syscall.S_IFCHR, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ta, "upper", "replaced"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setxattr(filepath.Join(ta, "upper", "replaced"), "trusted.overlay.opaque", []byte("y"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ta, ".meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ta, ".meta", "base-image.json"), []byte(e2eBasePin), 0o600); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(ta, "big")
	if err := os.WriteFile(big, make1(64<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	return big
}

func e2eTestMMPGuard(t *testing.T, img string) {
	t.Helper()
	t.Run("MMP", func(t *testing.T) {
		dev, h, err := loopdev.Attach(img, loopdev.Options{DirectIO: loopdev.DirectIOOff})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = h.Close() }()
		dir := t.TempDir()
		start := time.Now()
		// MMP is only checked for read-write mounts.
		err = LinuxHost{}.MountExt4(dev.Path, dir, nil)
		if err == nil {
			_ = syscall.Unmount(dir, 0)
			t.Fatal("a second mount of a live MMP image succeeded")
		}
		t.Logf("second mount refused after %s: %v", time.Since(start).Round(time.Second), err)
	})
}

func e2eTestExpand(ctx context.Context, t *testing.T, ctrl *ControllerServer, node *NodeServer, volID, target string) {
	t.Helper()
	if _, err := ctrl.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
		VolumeId: volID, CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30}}); err != nil {
		t.Fatal(err)
	}
	if _, err := node.NodeExpandVolume(ctx, &csi.NodeExpandVolumeRequest{VolumeId: volID, VolumePath: target}); err != nil {
		t.Fatal(err)
	}
	stats, err := node.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{VolumeId: volID, VolumePath: target})
	if err != nil {
		t.Fatal(err)
	}
	if total := stats.GetUsage()[0].GetTotal(); total < 1900<<20 {
		t.Fatalf("filesystem is %d bytes after expanding to 2GiB", total)
	}
}

func e2eTestSnapshotRestoreAndCommit(ctx context.Context, t *testing.T, ctrl *ControllerServer, node *NodeServer, pool Pool, volID, ta string) (string, string) {
	t.Helper()
	// Online snapshot under a freeze, as the DevPilot controller does it.
	if err := loopdev.Freeze(ta); err != nil {
		t.Fatal(err)
	}
	snap, err := ctrl.CreateSnapshot(ctx, &csi.CreateSnapshotRequest{
		Name: "snapshot-e2e", SourceVolumeId: volID, Parameters: map[string]string{"allowLiveCopy": "true"}})
	if thawErr := loopdev.Thaw(ta); thawErr != nil {
		t.Fatal(thawErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	snapImg := snapshotImagePath(pool, SnapshotID{Pool: testPoolE2E, Kind: SnapshotFile, Name: "snapshot-e2e"})
	if ssb, err := readSuperblock(snapImg); err != nil || ssb.NeedsRecovery {
		t.Fatalf("frozen snapshot superblock = %+v, %v (a frozen ext4 has a clean journal)", ssb, err)
	}

	// Restore and read it raw, the way a commit Job does.
	rresp, err := ctrl.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name: "pvc-restored", VolumeCapabilities: []*csi.VolumeCapability{rwCap}, Parameters: map[string]string{ParamPool: testPoolE2E},
		CapacityRange: &csi.CapacityRange{RequiredBytes: 2 << 30},
		VolumeContentSource: &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
			Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: snap.GetSnapshot().GetSnapshotId()}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	restored := rresp.GetVolume()
	traw := filepath.Join(t.TempDir(), "raw")
	if err := publish(node, restored, traw, true); err != nil {
		t.Fatal(err)
	}
	rawDiskPath := filepath.Join(traw, ImageName)
	out, err := exec.Command("debugfs", "-R", "cat /hello", rawDiskPath).Output()
	if err != nil || string(out) != e2eHelloBody {
		t.Fatalf("debugfs cat /hello from the raw restored image = %q, %v", out, err)
	}
	verifyE2ELayer(t, rawDiskPath)
	if err := os.WriteFile(filepath.Join(traw, "x"), nil, 0o644); err == nil {
		t.Fatal("raw publish is writable")
	}
	if err := unpublish(node, restored, traw); err != nil {
		t.Fatal(err)
	}
	return restored.GetVolumeId(), snap.GetSnapshot().GetSnapshotId()
}

func verifyE2ELayer(t *testing.T, rawDiskPath string) {
	t.Helper()
	// Verify pure-Go ext4 reader + commit.Ext4Source on the raw snapshot clone.
	extImg, err := ext4.OpenFile(rawDiskPath)
	if err != nil {
		t.Fatalf("ext4.OpenFile(%s): %v", rawDiskPath, err)
	}
	defer func() { _ = extImg.Close() }()
	pinIno, err := extImg.Lookup(".meta/base-image.json")
	if err != nil {
		t.Fatalf("ext4 lookup .meta/base-image.json: %v", err)
	}
	pinSR, err := extImg.OpenSection(pinIno)
	if err != nil {
		t.Fatal(err)
	}
	pinData, err := io.ReadAll(pinSR)
	if err != nil || string(pinData) != e2eBasePin {
		t.Fatalf("base-image.json = %q, %v", pinData, err)
	}
	var layerBuf bytes.Buffer
	layerStats, err := commit.WriteLayer(&layerBuf, commit.Options{
		Source: &commit.Ext4Source{Img: extImg, Root: "upper"},
	})
	if err != nil {
		t.Fatalf("commit.WriteLayer over Ext4Source: %v", err)
	}
	gotTar := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(layerBuf.Bytes()))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		gotTar[hdr.Name] = string(body)
	}
	if gotTar["app/run.sh"] != "#!/bin/sh\necho ok\n" {
		t.Fatalf("layer app/run.sh = %q", gotTar["app/run.sh"])
	}
	if _, ok := gotTar["app/.wh.wh"]; !ok {
		t.Fatalf("layer missing app/.wh.wh, got %v", gotTar)
	}
	if _, ok := gotTar["replaced/.wh..wh..opq"]; !ok {
		t.Fatalf("layer missing replaced/.wh..wh..opq, got %v", gotTar)
	}
	if layerStats.Whiteouts != 1 || layerStats.OpaqueDirs != 1 || layerStats.Files != 1 {
		t.Fatalf("unexpected E2E layer stats: %+v", layerStats)
	}
}

func e2eTestTrimAndTakeover(t *testing.T, a, b *NodeServer, vol *csi.Volume, img, ta, tb, big string) {
	t.Helper()
	// Trim on unpublish gives freed blocks back to the pool.
	if err := os.Remove(big); err != nil {
		t.Fatal(err)
	}
	syscall.Sync()
	var st syscall.Stat_t
	_ = syscall.Stat(img, &st)
	before := st.Blocks / 2
	if err := unpublish(a, vol, ta); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Stat(img, &st)
	t.Logf("image allocation %d KiB before trim, %d KiB after", before, st.Blocks/2)
	if st.Blocks/2 > before-32<<10 {
		t.Errorf("trim on unpublish did not release the deleted 64MiB (%d -> %d KiB)", before, st.Blocks/2)
	}
	if loops, _ := loopdev.FindByBackingFile(img); len(loops) != 0 {
		t.Fatalf("loop devices left after unpublish: %+v", loops)
	}
	if held, _, _ := probeLock(img); held {
		t.Fatal("lock left after unpublish")
	}

	// The other node can take over, and sees the data.
	if err := publish(b, vol, tb, false); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(tb, "hello")); err != nil || string(got) != e2eHelloBody {
		t.Fatalf("node-b read %q, %v", got, err)
	}
	if err := unpublish(b, vol, tb); err != nil {
		t.Fatal(err)
	}
}

func make1(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + 1)
	}
	return b
}
