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
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wutz/devpilot/internal/loopdev"
)

// fakeHost stands in for the kernel. A "loop device" is a dup of the backing
// descriptor, so it keeps the OFD/flock lock exactly like the real device's
// file reference does, and it is closed when its mount goes away
// (autoclear).
type fakeHost struct {
	mu         sync.Mutex
	next       int
	loops      map[string]*fakeLoop // device path -> loop
	mounts     map[string]string    // target -> source (device or dir)
	readOnly   map[string]bool
	calls      []string
	noDIO      bool
	gpfsUnsafe bool
}

type fakeLoop struct {
	image string
	file  *os.File
	held  int // open handles + mounts
}

func newFakeHost() *fakeHost {
	return &fakeHost{loops: map[string]*fakeLoop{}, mounts: map[string]string{}, readOnly: map[string]bool{}}
}

func (h *fakeHost) record(s string, args ...any) {
	h.calls = append(h.calls, fmt.Sprintf(s, args...))
}

func (h *fakeHost) OpenBacking(path string, readOnly, dio bool) (*os.File, error) {
	h.record("open dio=%v", dio)
	if h.gpfsUnsafe && dio {
		return nil, fmt.Errorf("%s: %w", path, loopdev.ErrGPFSDirectIOUnsafe)
	}
	return loopdev.OpenBacking(path, readOnly, false)
}

type loopHandle struct {
	h   *fakeHost
	dev string
}

func (l loopHandle) Close() error {
	l.h.mu.Lock()
	defer l.h.mu.Unlock()
	l.h.release(l.dev)
	return nil
}

func (h *fakeHost) release(dev string) {
	lp := h.loops[dev]
	if lp == nil {
		return
	}
	lp.held--
	if lp.held <= 0 {
		_ = lp.file.Close()
		delete(h.loops, dev)
	}
}

func (h *fakeHost) Attach(f *os.File, opts loopdev.Options) (*loopdev.Device, io.Closer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gpfsUnsafe && opts.DirectIO != loopdev.DirectIOPreferred && opts.DirectIO != loopdev.DirectIOOff {
		return nil, nil, fmt.Errorf("%s: %w: %w", f.Name(), loopdev.ErrDirectIOUnavailable, loopdev.ErrGPFSDirectIOUnsafe)
	}
	if h.noDIO && opts.DirectIO != loopdev.DirectIOPreferred && opts.DirectIO != loopdev.DirectIOOff {
		return nil, nil, loopdev.ErrDirectIOUnavailable
	}
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		return nil, nil, err
	}
	dev := fmt.Sprintf("/dev/loop%d", h.next)
	h.next++
	h.loops[dev] = &fakeLoop{image: f.Name(), file: os.NewFile(uintptr(fd), f.Name()), held: 1}
	h.record("attach %s %s", dev, filepath.Base(filepath.Dir(f.Name())))
	dio := !h.noDIO && !h.gpfsUnsafe && opts.DirectIO != loopdev.DirectIOOff
	return &loopdev.Device{Path: dev, DirectIO: dio, BackingFile: f.Name()}, loopHandle{h, dev}, nil
}

func (h *fakeHost) FindLoops(image string) ([]loopdev.Device, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []loopdev.Device
	for dev, lp := range h.loops {
		if lp.image == image {
			out = append(out, loopdev.Device{Path: dev, BackingFile: image})
		}
	}
	return out, nil
}

func (h *fakeHost) Detach(dev string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("detach %s", dev)
	if lp := h.loops[dev]; lp != nil {
		_ = lp.file.Close()
		delete(h.loops, dev)
	}
	return nil
}

func (h *fakeHost) SetCapacity(dev string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("setcapacity %s", dev)
	return nil
}

func (h *fakeHost) MountExt4(dev, target string, options []string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	lp := h.loops[dev]
	if lp == nil {
		return fmt.Errorf("no device %s", dev)
	}
	lp.held++
	h.mounts[target] = dev
	h.record("mount %s %s", dev, strings.Join(options, ","))
	return nil
}

func (h *fakeHost) BindMount(source, target string, readOnly bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mounts[target] = source
	h.readOnly[target] = readOnly
	h.record("bind ro=%v", readOnly)
	return nil
}

func (h *fakeHost) Unmount(target string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	src, ok := h.mounts[target]
	if !ok {
		return nil
	}
	delete(h.mounts, target)
	delete(h.readOnly, target)
	if strings.HasPrefix(src, "/dev/loop") {
		h.release(src)
	}
	return nil
}

func (h *fakeHost) MountState(path string) (MountState, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.mounts[path]
	return MountState{Mounted: ok, ReadOnly: h.readOnly[path]}, nil
}

func (h *fakeHost) Thaw(path string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("thaw")
	return nil
}

func (h *fakeHost) Trim(path string, minLen uint64) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("trim %d", minLen)
	return 0, nil
}

func (h *fakeHost) Statfs(string) (Usage, error) {
	return Usage{TotalBytes: 100, UsedBytes: 40, AvailableBytes: 60, TotalInodes: 10, UsedInodes: 1, FreeInodes: 9}, nil
}

func (h *fakeHost) has(prefix string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.ContainsFunc(h.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

// fakeRunner fakes e2fsprogs: mkfs.ext4 writes a superblock covering the
// file, the others are recorded.
type fakeRunner struct {
	t     *testing.T
	mu    sync.Mutex
	calls []string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	r.mu.Unlock()
	if name == "mkfs.ext4" {
		path := args[len(args)-1]
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		mmp := slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, "mmp") })
		fakeSuperblock(r.t, path, st.Size(), mmp, false)
	}
	return nil, nil
}

func (r *fakeRunner) ran(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if strings.HasPrefix(c, name+" ") {
			n++
		}
	}
	return n
}

type env struct {
	t      *testing.T
	pool   Pool
	pools  Pools
	ctrl   *ControllerServer
	runner *fakeRunner
	now    time.Time
}

const (
	testPoolShared = "shared"
	testNodeA      = "node-a"
)

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := Pool{Name: testPoolShared, Path: t.TempDir()}
	pools, _ := NewPools([]Pool{pool})
	e := &env{t: t, pool: pool, pools: pools, runner: &fakeRunner{t: t}, now: time.Now()}
	e.ctrl = &ControllerServer{Pools: pools, Runner: e.runner, Log: logr.Discard(), Now: func() time.Time { return e.now }}
	return e
}

func (e *env) node(name string) (*NodeServer, *fakeHost) {
	e.t.Helper()
	h := newFakeHost()
	n := &NodeServer{
		NodeID: name, Pools: e.pools, StateDir: filepath.Join(e.t.TempDir(), name), Host: h, Runner: e.runner,
		LeaseInterval: time.Hour, Log: logr.Discard(), Now: func() time.Time { return e.now },
	}
	if err := n.Start(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	return n, h
}

var rwCap = &csi.VolumeCapability{
	AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
	AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
}

func (e *env) create(name string, size int64, src *csi.VolumeContentSource) (*csi.Volume, error) {
	resp, err := e.ctrl.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:                name,
		CapacityRange:       &csi.CapacityRange{RequiredBytes: size},
		VolumeCapabilities:  []*csi.VolumeCapability{rwCap},
		Parameters:          map[string]string{ParamPool: testPoolShared},
		VolumeContentSource: src,
	})
	if err != nil {
		return nil, err
	}
	return resp.GetVolume(), nil
}

func publish(n *NodeServer, vol *csi.Volume, target string, readOnly bool) error {
	_, err := n.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: vol.GetVolumeId(), TargetPath: target, VolumeCapability: rwCap,
		Readonly: readOnly, VolumeContext: vol.GetVolumeContext(),
	})
	return err
}

func unpublish(n *NodeServer, vol *csi.Volume, target string) error {
	_, err := n.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{VolumeId: vol.GetVolumeId(), TargetPath: target})
	return err
}

func wantCode(t *testing.T, err error, code codes.Code, what string) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("%s: got %v, want %s", what, err, code)
	}
}

func TestCreateVolumeIdempotent(t *testing.T) {
	e := newEnv(t)
	vol, err := e.create("pvc-a", 2<<30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if vol.GetVolumeId() != "shared/pvc-a" || vol.GetCapacityBytes() != 2<<30 {
		t.Fatalf("volume = %+v", vol)
	}
	sb, err := readSuperblock(imagePath(e.pool, "pvc-a"))
	if err != nil || !sb.MMP {
		t.Fatalf("image superblock = %+v, %v", sb, err)
	}
	if _, err := e.create("pvc-a", 2<<30, nil); err != nil {
		t.Fatalf("repeat CreateVolume: %v", err)
	}
	if e.runner.ran("mkfs.ext4") != 1 {
		t.Errorf("mkfs ran %d times", e.runner.ran("mkfs.ext4"))
	}
	_, err = e.create("pvc-a", 2<<30, &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
		Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: "shared/pvc-b"}}})
	wantCode(t, err, codes.AlreadyExists, "CreateVolume with another source")
}

func TestSingleWriterAcrossNodes(t *testing.T) {
	e := newEnv(t)
	vol, err := e.create("pvc-a", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, ha := e.node(testNodeA)
	b, _ := e.node("node-b")
	ta := filepath.Join(t.TempDir(), "a")
	tb := filepath.Join(t.TempDir(), "b")

	if err := publish(a, vol, ta, false); err != nil {
		t.Fatal(err)
	}
	if !ha.has("mount /dev/loop0 noatime,errors=remount-ro,noinit_itable") {
		t.Errorf("unexpected mount calls %v", ha.calls)
	}
	// Idempotent republish of the same target.
	if err := publish(a, vol, ta, false); err != nil {
		t.Fatalf("republish: %v", err)
	}
	wantCode(t, publish(b, vol, tb, false), codes.FailedPrecondition, "second writer")
	wantCode(t, publish(b, vol, tb, true), codes.FailedPrecondition, "raw reader while mounted")
	wantCode(t, publish(a, vol, tb, true), codes.FailedPrecondition, "raw on the writer's node")

	_, err = e.ctrl.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: vol.GetVolumeId()})
	wantCode(t, err, codes.FailedPrecondition, "delete while mounted")
	_, err = e.create("pvc-clone", 0, &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
		Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: vol.GetVolumeId()}}})
	wantCode(t, err, codes.FailedPrecondition, "clone while mounted")

	l, err := readLease(leasePath(e.pool, "pvc-a"))
	if err != nil || l == nil || l.Node != testNodeA {
		t.Fatalf("lease = %+v, %v", l, err)
	}

	if err := unpublish(a, vol, ta); err != nil {
		t.Fatal(err)
	}
	if !ha.has("thaw") || !ha.has("trim 1048576") {
		t.Errorf("teardown calls = %v", ha.calls)
	}
	if l, _ := readLease(leasePath(e.pool, "pvc-a")); l != nil {
		t.Errorf("lease left after unpublish: %+v", l)
	}
	if err := publish(b, vol, tb, false); err != nil {
		t.Fatalf("publish on b after a released: %v", err)
	}
	if err := unpublish(b, vol, tb); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ctrl.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: vol.GetVolumeId()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(volumeDir(e.pool, "pvc-a")); !os.IsNotExist(err) {
		t.Error("volume directory survived DeleteVolume")
	}
}

func TestLeaseBlocksWhenLockIsLost(t *testing.T) {
	e := newEnv(t)
	vol, _ := e.create("pvc-a", 0, nil)
	b, _ := e.node("node-b")
	// node-a heartbeated 10s ago but its lock is gone (e.g. NFS lock state
	// lost on a server failover).
	if err := writeLease(leasePath(e.pool, "pvc-a"), &Lease{Node: testNodeA, Heartbeat: e.now.Add(-10 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "b")
	wantCode(t, publish(b, vol, target, false), codes.FailedPrecondition, "fresh foreign lease")
	wantCode(t, publish(b, vol, target, true), codes.FailedPrecondition, "raw under fresh foreign lease")
	e.now = e.now.Add(time.Minute)
	if err := publish(b, vol, target, false); err != nil {
		t.Fatalf("publish after lease expiry: %v", err)
	}
}

func TestRawPublishBlocksWriter(t *testing.T) {
	e := newEnv(t)
	vol, _ := e.create("pvc-a", 0, nil)
	a, ha := e.node(testNodeA)
	b, _ := e.node("node-b")
	ta := filepath.Join(t.TempDir(), "a")
	ta2 := filepath.Join(t.TempDir(), "a2")
	tb := filepath.Join(t.TempDir(), "b")
	if err := publish(a, vol, ta, true); err != nil {
		t.Fatal(err)
	}
	if err := publish(a, vol, ta2, true); err != nil {
		t.Fatal(err)
	}
	if ha.mounts[ta] != volumeDir(e.pool, "pvc-a") || !ha.readOnly[ta] {
		t.Errorf("raw publish mounted %q ro=%v", ha.mounts[ta], ha.readOnly[ta])
	}
	if ha.has("attach") {
		t.Error("raw publish attached a loop device")
	}
	wantCode(t, publish(b, vol, tb, false), codes.FailedPrecondition, "writer during raw read")
	if err := unpublish(a, vol, ta); err != nil {
		t.Fatal(err)
	}
	wantCode(t, publish(b, vol, tb, false), codes.FailedPrecondition, "writer while one raw target remains")
	if err := unpublish(a, vol, ta2); err != nil {
		t.Fatal(err)
	}
	if err := publish(b, vol, tb, false); err != nil {
		t.Fatalf("writer after raw readers left: %v", err)
	}
}

func TestDirectIORequired(t *testing.T) {
	e := newEnv(t)
	vol, _ := e.create("pvc-a", 0, nil)
	a, ha := e.node(testNodeA)
	ha.noDIO = true
	target := filepath.Join(t.TempDir(), "a")
	wantCode(t, publish(a, vol, target, false), codes.FailedPrecondition, "buffered loop with directIO=required")
	if l, _ := readLease(leasePath(e.pool, "pvc-a")); l != nil {
		t.Error("failed publish left a lease")
	}
	if held, _, _ := probeLock(imagePath(e.pool, "pvc-a")); held {
		t.Error("failed publish left the lock held")
	}
	vol.VolumeContext[ParamDirectIO] = string(loopdev.DirectIOPreferred)
	if err := publish(a, vol, target, false); err != nil {
		t.Fatalf("directIO=preferred: %v", err)
	}
}

func TestGPFSDirectIOGuard(t *testing.T) {
	e := newEnv(t)
	vol, err := e.create("pvc-gpfs", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, ha := e.node(testNodeA)
	ha.gpfsUnsafe = true
	target := filepath.Join(t.TempDir(), "a")

	// directIO=required must be rejected with FailedPrecondition without touching lease/lock/attach.
	wantCode(t, publish(a, vol, target, false), codes.FailedPrecondition, "GPFS with directIO=required")
	if ha.has("attach") {
		t.Errorf("directIO=required on GPFS attempted Attach: %v", ha.calls)
	}
	if l, _ := readLease(leasePath(e.pool, "pvc-gpfs")); l != nil {
		t.Error("failed GPFS publish left a lease")
	}
	if held, _, _ := probeLock(imagePath(e.pool, "pvc-gpfs")); held {
		t.Error("failed GPFS publish left the lock held")
	}

	// directIO=preferred falls back to buffered OpenBacking (dio=false).
	vol.VolumeContext[ParamDirectIO] = string(loopdev.DirectIOPreferred)
	if err := publish(a, vol, target, false); err != nil {
		t.Fatalf("GPFS with directIO=preferred: %v", err)
	}
	if !ha.has("open dio=false") {
		t.Errorf("GPFS fallback did not reopen with dio=false: %v", ha.calls)
	}
	if v := a.get(VolumeID{Pool: testPoolShared, Name: "pvc-gpfs"}); v == nil || v.DirectIO {
		t.Errorf("GPFS preferred volume state = %+v, want DirectIO=false", v)
	}
	if err := unpublish(a, vol, target); err != nil {
		t.Fatal(err)
	}

	// directIO=off opens directly without O_DIRECT.
	vol.VolumeContext[ParamDirectIO] = string(loopdev.DirectIOOff)
	if err := publish(a, vol, target, false); err != nil {
		t.Fatalf("GPFS with directIO=off: %v", err)
	}
	if v := a.get(VolumeID{Pool: testPoolShared, Name: "pvc-gpfs"}); v == nil || v.DirectIO {
		t.Errorf("GPFS directIO=off volume state = %+v, want DirectIO=false", v)
	}
}

func TestRecoveryAfterPluginRestart(t *testing.T) {
	e := newEnv(t)
	vol, _ := e.create("pvc-a", 0, nil)
	a, ha := e.node(testNodeA)
	target := filepath.Join(t.TempDir(), "a")
	if err := publish(a, vol, target, false); err != nil {
		t.Fatal(err)
	}
	// A new plugin process on the same node and the same kernel state.
	restarted := &NodeServer{
		NodeID: testNodeA, Pools: e.pools, StateDir: a.StateDir, Host: ha, Runner: e.runner,
		LeaseInterval: time.Hour, Log: logr.Discard(), Now: func() time.Time { return e.now },
	}
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restarted.get(VolumeID{Pool: testPoolShared, Name: "pvc-a"}) == nil {
		t.Fatal("volume not recovered")
	}
	// The lock still belongs to the loop device, not the dead process.
	if held, excl, _ := probeLock(imagePath(e.pool, "pvc-a")); !held || !excl {
		t.Error("lock lost across plugin restart")
	}
	if err := unpublish(restarted, vol, target); err != nil {
		t.Fatal(err)
	}
	if held, _, _ := probeLock(imagePath(e.pool, "pvc-a")); held {
		t.Error("lock held after unpublish")
	}
}

func TestRecoveryAfterReboot(t *testing.T) {
	e := newEnv(t)
	vol, _ := e.create("pvc-a", 0, nil)
	a, _ := e.node(testNodeA)
	if err := publish(a, vol, filepath.Join(t.TempDir(), "a"), false); err != nil {
		t.Fatal(err)
	}
	// Reboot: state files persist on the host, mounts and devices do not.
	rebooted := &NodeServer{
		NodeID: testNodeA, Pools: e.pools, StateDir: a.StateDir, Host: newFakeHost(), Runner: e.runner,
		LeaseInterval: time.Hour, Log: logr.Discard(),
	}
	if err := rebooted.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rebooted.get(VolumeID{Pool: testPoolShared, Name: "pvc-a"}) != nil {
		t.Error("stale state kept after reboot")
	}
	if l, _ := readLease(leasePath(e.pool, "pvc-a")); l != nil {
		t.Error("stale lease kept after reboot")
	}
}

func TestExpand(t *testing.T) {
	e := newEnv(t)
	vol, _ := e.create("pvc-a", 0, nil)
	a, ha := e.node(testNodeA)
	target := filepath.Join(t.TempDir(), "a")
	if err := publish(a, vol, target, false); err != nil {
		t.Fatal(err)
	}
	resp, err := e.ctrl.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: vol.GetVolumeId(), CapacityRange: &csi.CapacityRange{RequiredBytes: 3 << 30}})
	if err != nil || resp.GetCapacityBytes() != 3<<30 || !resp.GetNodeExpansionRequired() {
		t.Fatalf("ControllerExpandVolume = %+v, %v", resp, err)
	}
	nresp, err := a.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{VolumeId: vol.GetVolumeId(), VolumePath: target})
	if err != nil || nresp.GetCapacityBytes() != 3<<30 {
		t.Fatalf("NodeExpandVolume = %+v, %v", nresp, err)
	}
	if !ha.has("setcapacity /dev/loop0") || e.runner.ran("resize2fs") != 1 {
		t.Errorf("expand calls host=%v runner=%v", ha.calls, e.runner.calls)
	}
}

func TestSnapshotRestoreAndLiveCopyGuard(t *testing.T) {
	e := newEnv(t)
	vol, _ := e.create("pvc-a", 0, nil)
	a, _ := e.node(testNodeA)
	target := filepath.Join(t.TempDir(), "a")
	if err := publish(a, vol, target, false); err != nil {
		t.Fatal(err)
	}
	snap := func(name string, params map[string]string) (*csi.Snapshot, error) {
		resp, err := e.ctrl.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
			Name: name, SourceVolumeId: vol.GetVolumeId(), Parameters: params})
		return resp.GetSnapshot(), err
	}
	// Mounted, no reflink on the test filesystem, no freeze promise: refuse.
	if s, err := snap("snapshot-1", map[string]string{ParamSnapshotStrategy: "copy"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("live copy = %+v, %v", s, err)
	}
	if _, err := snap("snapshot-1", nil); status.Code(err) != codes.FailedPrecondition && err != nil {
		t.Fatalf("live auto snapshot: %v", err)
	}
	const testTrue = "true"
	s, err := snap("snapshot-2", map[string]string{ParamAllowLiveCopy: testTrue})
	if err != nil || !s.GetReadyToUse() || s.GetSnapshotId() != "shared/snap/snapshot-2" {
		t.Fatalf("frozen live copy = %+v, %v", s, err)
	}
	again, err := snap("snapshot-2", map[string]string{ParamAllowLiveCopy: testTrue})
	if err != nil || !again.GetCreationTime().AsTime().Equal(s.GetCreationTime().AsTime()) {
		t.Fatalf("repeat CreateSnapshot = %+v, %v", again, err)
	}

	restored, err := e.create("pvc-r", 4<<30, &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
		Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: s.GetSnapshotId()}}})
	if err != nil || restored.GetCapacityBytes() != 4<<30 {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	// Publishing the larger restored image grows its filesystem.
	b, _ := e.node("node-b")
	if err := publish(b, restored, filepath.Join(t.TempDir(), "b"), false); err != nil {
		t.Fatal(err)
	}
	if e.runner.ran("resize2fs") != 1 {
		t.Errorf("resize2fs ran %d times", e.runner.ran("resize2fs"))
	}
	_, err = e.create("pvc-small", 512<<20, &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
		Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: s.GetSnapshotId()}}})
	if err != nil {
		t.Fatalf("restore at minimum size: %v", err)
	}
	if _, err := e.ctrl.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: s.GetSnapshotId()}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.pool.Path, SnapshotsDir, "snapshot-2")); !os.IsNotExist(err) {
		t.Error("snapshot directory survived DeleteSnapshot")
	}
}

func TestFsckOnRecordedErrors(t *testing.T) {
	e := newEnv(t)
	vol, _ := e.create("pvc-a", 0, nil)
	img := imagePath(e.pool, "pvc-a")
	st, _ := os.Stat(img)
	fakeSuperblock(t, img, st.Size(), true, true)
	a, _ := e.node("node-a")
	if err := publish(a, vol, filepath.Join(t.TempDir(), "a"), false); err != nil {
		t.Fatal(err)
	}
	if e.runner.ran("e2fsck") != 1 {
		t.Errorf("e2fsck ran %d times", e.runner.ran("e2fsck"))
	}
}

func TestVolumeStatsCondition(t *testing.T) {
	e := newEnv(t)
	vol, _ := e.create("pvc-a", 0, nil)
	a, ha := e.node("node-a")
	target := filepath.Join(t.TempDir(), "a")
	if err := publish(a, vol, target, false); err != nil {
		t.Fatal(err)
	}
	get := func() *csi.NodeGetVolumeStatsResponse {
		r, err := a.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: vol.GetVolumeId(), VolumePath: target})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := get(); r.GetVolumeCondition().GetAbnormal() || r.GetUsage()[0].GetUsed() != 40 {
		t.Errorf("healthy stats = %+v", r)
	}
	ha.readOnly[target] = true // ext4 errors=remount-ro fired
	if r := get(); !r.GetVolumeCondition().GetAbnormal() {
		t.Error("read-only remount not reported abnormal")
	}
}

func TestValidateCapability(t *testing.T) {
	block := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
	rwx := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
	}
	xfs := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
	for _, c := range []*csi.VolumeCapability{nil, block, rwx, xfs} {
		if validateCapability(c) == nil {
			t.Errorf("capability %v accepted", c)
		}
	}
	if err := validateCapability(rwCap); err != nil {
		t.Error(err)
	}
}
