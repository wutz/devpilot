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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wutz/devpilot/internal/loopdev"
)

// Publish modes recorded in node state.
const (
	// ModeReadWrite is the ext4 filesystem inside the image, mounted through
	// a loop device. Used by DevBox pods.
	ModeReadWrite = "rw"
	// ModeRaw is the volume directory itself, bind-mounted read-only, so a
	// reader sees disk.img as a file. Every read-only publish uses it: the
	// commit Job parses the image in user space instead of mounting it,
	// which needs no loop device, no privileges and no journal replay.
	ModeRaw = "raw"
)

// Default lease timing.
const (
	DefaultLeaseInterval = 10 * time.Second
	DefaultLeaseTTL      = 45 * time.Second
)

// ext4 mount options of every read-write mount. errors=remount-ro turns a
// storage outage into a read-only root instead of silent corruption;
// noinit_itable skips zeroing inode tables that are holes already.
var ext4MountOptions = []string{"noatime", "errors=remount-ro", "noinit_itable"}

// nodeVolume is the node-local state of one published volume. It is
// persisted so a restarted plugin can unpublish, expand and heartbeat
// volumes it mounted earlier.
type nodeVolume struct {
	VolumeID      string   `json:"volumeID"`
	Mode          string   `json:"mode"`
	Image         string   `json:"image"`
	Lease         string   `json:"lease"`
	Device        string   `json:"device,omitempty"`
	Mount         string   `json:"mount,omitempty"`
	Targets       []string `json:"targets"`
	DirectIO      bool     `json:"directIO,omitempty"`
	TrimOnUnmount bool     `json:"trimOnUnmount,omitempty"`
	TrimMinLength uint64   `json:"trimMinLength,omitempty"`

	acquired      time.Time
	readLock      *os.File
	stopHeartbeat context.CancelFunc
}

// NodeServer implements the CSI node service.
type NodeServer struct {
	csi.UnimplementedNodeServer

	NodeID string
	Pools  Pools
	// StateDir is the plugin's directory under the kubelet root, e.g.
	// /var/lib/kubelet/plugins/loop.devpilot.wutz.dev. Private ext4 mounts
	// live under it, so it must be mounted with Bidirectional propagation.
	StateDir      string
	Host          Host
	Runner        Runner
	LeaseInterval time.Duration
	LeaseTTL      time.Duration
	Log           logr.Logger
	Now           func() time.Time

	bootID  string
	volLock keyedMutex
	mu      sync.Mutex
	vols    map[string]*nodeVolume
}

func (n *NodeServer) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

func (n *NodeServer) stateFile(id VolumeID) string {
	return filepath.Join(n.StateDir, "state", id.Key()+".json")
}

func (n *NodeServer) mountDir(id VolumeID) string {
	return filepath.Join(n.StateDir, "mounts", id.Key())
}

// Start loads node state and resumes heartbeats for volumes that are still
// mounted. It must run before the gRPC server accepts calls.
func (n *NodeServer) Start(ctx context.Context) error {
	if n.LeaseInterval == 0 {
		n.LeaseInterval = DefaultLeaseInterval
	}
	if n.LeaseTTL == 0 {
		n.LeaseTTL = DefaultLeaseTTL
	}
	if n.Runner == nil {
		n.Runner = ExecRunner{}
	}
	n.bootID = readBootID()
	n.vols = map[string]*nodeVolume{}
	for _, d := range []string{"state", "mounts"} {
		if err := os.MkdirAll(filepath.Join(n.StateDir, d), 0o700); err != nil {
			return err
		}
	}
	files, err := filepath.Glob(filepath.Join(n.StateDir, "state", "*.json"))
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := n.recover(ctx, f); err != nil {
			n.Log.Error(err, "Failed to recover volume state", "stateFile", f)
		}
	}
	return nil
}

func (n *NodeServer) recover(ctx context.Context, file string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var v nodeVolume
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("parse %s: %w", file, err)
	}
	id, err := ParseVolumeID(v.VolumeID)
	if err != nil {
		return err
	}
	log := n.Log.WithValues("volumeID", v.VolumeID, "mode", v.Mode)
	var live []string
	for _, t := range v.Targets {
		if st, err := n.Host.MountState(t); err == nil && st.Mounted {
			live = append(live, t)
		}
	}
	v.Targets = live

	switch v.Mode {
	case ModeReadWrite:
		st, err := n.Host.MountState(v.Mount)
		if err != nil {
			return err
		}
		if !st.Mounted {
			// The node rebooted or someone unmounted it: nothing is held.
			log.Info("Dropping state of a volume that is no longer mounted")
			for _, t := range live {
				_ = n.Host.Unmount(t)
			}
			_ = removeLease(v.Lease, n.NodeID)
			_ = os.Remove(v.Mount)
			return os.Remove(file)
		}
		if len(live) == 0 {
			log.Info("Volume has no published targets left; unmounting")
			n.vols[v.VolumeID] = &v
			return n.teardown(ctx, id, &v)
		}
		if l, err := readLease(v.Lease); err == nil && l != nil {
			v.acquired = l.Acquired
		}
		n.startHeartbeat(&v)
		log.Info("Recovered read-write volume", "device", v.Device, "targets", len(live))
	case ModeRaw:
		if len(live) == 0 {
			return os.Remove(file)
		}
		f, err := os.Open(v.Image)
		if err != nil {
			return err
		}
		if err := lockFile(f, false); err != nil {
			// Keep serving: the commit reader is already running, and the
			// conflicting writer is caught by its own lease and MMP checks.
			log.Error(err, "Could not re-acquire the shared lock of a raw volume")
		}
		v.readLock = f
		log.Info("Recovered raw volume", "targets", len(live))
	default:
		return fmt.Errorf("unknown mode %q", v.Mode)
	}
	n.vols[v.VolumeID] = &v
	return n.save(id, &v)
}

func (n *NodeServer) save(id VolumeID, v *nodeVolume) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFileAtomic(n.stateFile(id), b)
}

func (n *NodeServer) forget(id VolumeID) {
	n.mu.Lock()
	delete(n.vols, id.String())
	n.mu.Unlock()
	_ = os.Remove(n.stateFile(id))
}

func (n *NodeServer) get(id VolumeID) *nodeVolume {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.vols[id.String()]
}

func (n *NodeServer) put(v *nodeVolume) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.vols[v.VolumeID] = v
}

// NodeGetInfo reports the node name as node ID.
func (n *NodeServer) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: n.NodeID}, nil
}

// NodeGetCapabilities advertises expansion, stats and volume condition.
// There is no STAGE: kubelet does not pass the read-only flag to
// NodeStageVolume, and it decides between a read-write and a raw publish.
func (n *NodeServer) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	caps := []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		csi.NodeServiceCapability_RPC_VOLUME_CONDITION,
	}
	out := make([]*csi.NodeServiceCapability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &csi.NodeServiceCapability{Type: &csi.NodeServiceCapability_Rpc{Rpc: &csi.NodeServiceCapability_RPC{Type: c}}})
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: out}, nil
}

// NodePublishVolume mounts the ext4 filesystem (read-write) or the volume
// directory (read-only, raw) at the target path.
func (n *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	id, err := ParseVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	target := req.GetTargetPath()
	if target == "" || !filepath.IsAbs(target) {
		return nil, status.Error(codes.InvalidArgument, "target_path must be an absolute path")
	}
	if err := validateCapability(req.GetVolumeCapability()); err != nil {
		return nil, err
	}
	pool, err := n.Pools.Resolve(id.Pool)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	params, err := ParseVolumeParams(req.GetVolumeContext())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	unlock := n.volLock.Lock(id.String())
	defer unlock()
	log := n.Log.WithValues("volumeID", id.String(), "target", target)

	if err := os.MkdirAll(target, 0o750); err != nil {
		return nil, status.Errorf(codes.Internal, "create target: %v", err)
	}
	readOnly := req.GetReadonly() ||
		req.GetVolumeCapability().GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY ||
		req.GetVolumeCapability().GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
	mode := ModeReadWrite
	if readOnly {
		mode = ModeRaw
	}

	if v := n.get(id); v != nil {
		if v.Mode != mode {
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %s is already published %s on this node; a %s publish must wait until it is unpublished", id, v.Mode, mode)
		}
		return &csi.NodePublishVolumeResponse{}, n.addTarget(id, v, pool, target)
	}

	var v *nodeVolume
	if mode == ModeRaw {
		v, err = n.publishRaw(id, pool, target)
	} else {
		v, err = n.publishReadWrite(ctx, id, pool, params, target)
	}
	if err != nil {
		log.Error(err, "Publish failed", "mode", mode)
		return nil, err
	}
	log.Info("Published volume", "mode", mode, "device", v.Device, "directIO", v.DirectIO)
	return &csi.NodePublishVolumeResponse{}, nil
}

func (n *NodeServer) addTarget(id VolumeID, v *nodeVolume, pool Pool, target string) error {
	if slices.Contains(v.Targets, target) {
		if st, err := n.Host.MountState(target); err == nil && st.Mounted {
			return nil
		}
		v.Targets = slices.DeleteFunc(v.Targets, func(t string) bool { return t == target })
	}
	src, ro := v.Mount, false
	if v.Mode == ModeRaw {
		src, ro = volumeDir(pool, id.Name), true
	}
	if err := n.Host.BindMount(src, target, ro); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	v.Targets = append(v.Targets, target)
	if err := n.save(id, v); err != nil {
		return status.Errorf(codes.Internal, "save state: %v", err)
	}
	return nil
}

func (n *NodeServer) publishRaw(id VolumeID, pool Pool, target string) (*nodeVolume, error) {
	img := imagePath(pool, id.Name)
	f, err := os.Open(img)
	if errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.NotFound, "volume %s has no %s", id, ImageName)
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	keep := false
	defer func() {
		if !keep {
			_ = f.Close()
		}
	}()
	if err := lockFile(f, false); err != nil {
		if errors.Is(err, ErrLocked) {
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %s is mounted read-write; a raw read-only publish needs it stopped or a snapshot", id)
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	lease, err := readLease(leasePath(pool, id.Name))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if lease.ForeignAndFresh(n.NodeID, n.now(), n.LeaseTTL) {
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume %s has a live read-write lease from node %s although its lock is free; refusing a raw publish", id, lease.Node)
	}
	if err := n.Host.BindMount(volumeDir(pool, id.Name), target, true); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	v := &nodeVolume{
		VolumeID: id.String(),
		Mode:     ModeRaw,
		Image:    img,
		Lease:    leasePath(pool, id.Name),
		Targets:  []string{target},
		readLock: f,
	}
	if err := n.save(id, v); err != nil {
		_ = n.Host.Unmount(target)
		return nil, status.Errorf(codes.Internal, "save state: %v", err)
	}
	keep = true
	n.put(v)
	return v, nil
}

func (n *NodeServer) publishReadWrite(ctx context.Context, id VolumeID, pool Pool, params VolumeParams, target string) (*nodeVolume, error) {
	img := imagePath(pool, id.Name)
	lp := leasePath(pool, id.Name)
	sb, err := readSuperblock(img)
	if errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.NotFound, "volume %s has no %s", id, ImageName)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "volume %s: %v", id, err)
	}

	// Guard 1: an OFD write lock on the very descriptor the loop device will
	// hold, so the lock lives exactly as long as the device is bound.
	f, err := n.Host.OpenBacking(img, false, params.DirectIO != loopdev.DirectIOOff)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer func() { _ = f.Close() }()
	if err := lockFile(f, true); err != nil {
		if errors.Is(err, ErrLocked) {
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %s is in use (mounted on another node or read by a commit); retrying", id)
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	// Guard 2: the heartbeat lease.
	now := n.now()
	lease, err := readLease(lp)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if lease.ForeignAndFresh(n.NodeID, now, n.LeaseTTL) {
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume %s has a live lease from node %s (heartbeat %s); the shared filesystem lock may be ineffective",
			id, lease.Node, lease.Heartbeat.Format(time.RFC3339))
	}
	v := &nodeVolume{
		VolumeID:      id.String(),
		Mode:          ModeReadWrite,
		Image:         img,
		Lease:         lp,
		Mount:         n.mountDir(id),
		TrimOnUnmount: params.TrimOnUnmount,
		TrimMinLength: params.TrimMinLength,
		acquired:      now,
	}
	if err := writeLease(lp, n.lease(v, now)); err != nil {
		return nil, status.Errorf(codes.Internal, "write lease: %v", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = removeLease(lp, n.NodeID)
		}
	}()

	if sb.Errors {
		if err := n.fsck(ctx, img); err != nil {
			return nil, status.Errorf(codes.Internal, "volume %s recorded filesystem errors and e2fsck could not repair them: %v", id, err)
		}
	}

	dev, devHandle, err := n.Host.Attach(f, loopdev.Options{DirectIO: params.DirectIO})
	if err != nil {
		if errors.Is(err, loopdev.ErrDirectIOUnavailable) {
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %s: %v; set the StorageClass parameter directIO=preferred to accept buffered I/O", id, err)
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	v.Device, v.DirectIO = dev.Path, dev.DirectIO
	if err := os.MkdirAll(v.Mount, 0o700); err != nil {
		_ = devHandle.Close()
		return nil, status.Error(codes.Internal, err.Error())
	}
	// Guard 3: ext4 MMP. With MMP enabled this mount waits about
	// 2*mmp_update_interval+1 seconds and fails if another node updates
	// the MMP block meanwhile.
	err = n.Host.MountExt4(dev.Path, v.Mount, ext4MountOptions)
	// From here the mount holds the device; closing our handle lets
	// LO_FLAGS_AUTOCLEAR detach it (and drop the lock) at unmount, or right
	// now when the mount failed.
	_ = devHandle.Close()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "volume %s: %v", id, err)
	}
	cleanupMount := func() {
		_ = n.Host.Unmount(v.Mount)
		_ = os.Remove(v.Mount)
	}
	if err := n.growFilesystem(ctx, v, sb); err != nil {
		cleanupMount()
		return nil, status.Errorf(codes.Internal, "volume %s: %v", id, err)
	}
	if err := n.Host.BindMount(v.Mount, target, false); err != nil {
		cleanupMount()
		return nil, status.Error(codes.Internal, err.Error())
	}
	v.Targets = []string{target}
	if err := n.save(id, v); err != nil {
		_ = n.Host.Unmount(target)
		cleanupMount()
		return nil, status.Errorf(codes.Internal, "save state: %v", err)
	}
	ok = true
	n.put(v)
	n.startHeartbeat(v)
	return v, nil
}

// growFilesystem runs resize2fs when the image is larger than the
// filesystem: after an offline ControllerExpandVolume, or for a volume
// restored from a smaller snapshot.
func (n *NodeServer) growFilesystem(ctx context.Context, v *nodeVolume, sb Superblock) error {
	st, err := os.Stat(v.Image)
	if err != nil {
		return err
	}
	if st.Size() <= sb.Size() {
		return nil
	}
	n.Log.Info("Growing filesystem to image size", "volumeID", v.VolumeID, "from", sb.Size(), "to", st.Size())
	_, err = n.Runner.Run(ctx, "resize2fs", v.Device)
	return err
}

// fsck repairs an image whose superblock records errors. Exit codes below 4
// mean the filesystem is consistent now.
func (n *NodeServer) fsck(ctx context.Context, img string) error {
	n.Log.Info("Running e2fsck -p on an image with recorded errors", "image", img)
	_, err := n.Runner.Run(ctx, "e2fsck", "-p", img)
	if code := exitCode(err); code > 0 && code < 4 {
		return nil
	}
	return err
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	return -1
}

func (n *NodeServer) lease(v *nodeVolume, now time.Time) *Lease {
	return &Lease{
		Node:      n.NodeID,
		BootID:    n.bootID,
		VolumeID:  v.VolumeID,
		Device:    v.Device,
		Acquired:  v.acquired,
		Heartbeat: now,
	}
}

func (n *NodeServer) startHeartbeat(v *nodeVolume) {
	ctx, cancel := context.WithCancel(context.Background())
	v.stopHeartbeat = cancel
	go func() {
		t := time.NewTicker(n.LeaseInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := writeLease(v.Lease, n.lease(v, n.now())); err != nil {
					n.Log.Error(err, "Failed to renew lease", "volumeID", v.VolumeID)
				}
			}
		}
	}()
}

// NodeUnpublishVolume unmounts a target; the last target of a read-write
// volume also trims, unmounts and detaches the image and drops the lease.
func (n *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	id, err := ParseVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target_path is required")
	}
	unlock := n.volLock.Lock(id.String())
	defer unlock()

	if err := n.Host.Unmount(target); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.Internal, "remove target: %v", err)
	}
	v := n.get(id)
	if v == nil {
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}
	v.Targets = slices.DeleteFunc(v.Targets, func(t string) bool { return t == target })
	if len(v.Targets) > 0 {
		if err := n.save(id, v); err != nil {
			return nil, status.Errorf(codes.Internal, "save state: %v", err)
		}
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}
	if err := n.teardown(ctx, id, v); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	n.Log.Info("Unpublished volume", "volumeID", id.String(), "mode", v.Mode)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (n *NodeServer) teardown(_ context.Context, id VolumeID, v *nodeVolume) error {
	log := n.Log.WithValues("volumeID", id.String())
	if v.Mode == ModeRaw {
		if v.readLock != nil {
			_ = v.readLock.Close()
		}
		n.forget(id)
		return nil
	}
	if v.stopHeartbeat != nil {
		v.stopHeartbeat()
	}
	// A freeze helper killed with SIGKILL leaves the filesystem frozen, and
	// unmounting a frozen filesystem blocks; thaw defensively.
	if err := n.Host.Thaw(v.Mount); err != nil {
		log.Error(err, "Thaw before unmount failed")
	}
	if v.TrimOnUnmount {
		trimmed, err := n.Host.Trim(v.Mount, v.TrimMinLength)
		if err != nil {
			log.Info("Trim before unmount failed; the image keeps its allocated blocks", "err", err.Error())
		} else {
			log.V(1).Info("Trimmed free blocks", "bytes", trimmed)
		}
	}
	if err := n.Host.Unmount(v.Mount); err != nil {
		if v.stopHeartbeat != nil {
			n.startHeartbeat(v)
		}
		return err
	}
	_ = os.Remove(v.Mount)
	// Autoclear detached the device at unmount. Detach leftovers that are
	// still bound to this exact image (matched by inode, so a device number
	// reused by another volume is never touched).
	if loops, err := n.Host.FindLoops(v.Image); err == nil {
		for _, d := range loops {
			if err := n.Host.Detach(d.Path); err != nil {
				log.Error(err, "Detach failed", "device", d.Path)
			}
		}
	}
	if err := removeLease(v.Lease, n.NodeID); err != nil {
		log.Error(err, "Failed to remove lease")
	}
	n.forget(id)
	return nil
}

// NodeExpandVolume grows the loop device and the mounted filesystem after
// ControllerExpandVolume grew the image.
func (n *NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	id, err := ParseVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	unlock := n.volLock.Lock(id.String())
	defer unlock()
	v := n.get(id)
	if v == nil {
		return nil, status.Errorf(codes.NotFound, "volume %s is not published on this node", id)
	}
	st, err := os.Stat(v.Image)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if v.Mode == ModeRaw {
		return &csi.NodeExpandVolumeResponse{CapacityBytes: st.Size()}, nil
	}
	dev, err := n.liveDevice(v)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := n.Host.SetCapacity(dev); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if _, err := n.Runner.Run(ctx, "resize2fs", dev); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	n.Log.Info("Expanded volume", "volumeID", id.String(), "bytes", st.Size())
	return &csi.NodeExpandVolumeResponse{CapacityBytes: st.Size()}, nil
}

// liveDevice returns the loop device currently backing the volume.
func (n *NodeServer) liveDevice(v *nodeVolume) (string, error) {
	loops, err := n.Host.FindLoops(v.Image)
	if err != nil {
		return "", err
	}
	switch len(loops) {
	case 0:
		return "", fmt.Errorf("no loop device is bound to %s", v.Image)
	case 1:
		return loops[0].Path, nil
	}
	return "", fmt.Errorf("%d loop devices are bound to %s", len(loops), v.Image)
}

// NodeGetVolumeStats reports usage and flags a filesystem that ext4
// remounted read-only after an I/O error.
func (n *NodeServer) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	id, err := ParseVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	path := req.GetVolumePath()
	ms, err := n.Host.MountState(path)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !ms.Mounted {
		return nil, status.Errorf(codes.NotFound, "%s is not mounted", path)
	}
	u, err := n.Host.Statfs(path)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	cond := &csi.VolumeCondition{Abnormal: false, Message: "ok"}
	if v := n.get(id); v != nil && v.Mode == ModeReadWrite && ms.ReadOnly {
		cond = &csi.VolumeCondition{Abnormal: true, Message: "the filesystem was remounted read-only after an error; stop the DevBox so e2fsck can run on the next start"}
	}
	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{Unit: csi.VolumeUsage_BYTES, Total: u.TotalBytes, Available: u.AvailableBytes, Used: u.UsedBytes},
			{Unit: csi.VolumeUsage_INODES, Total: u.TotalInodes, Available: u.FreeInodes, Used: u.UsedInodes},
		},
		VolumeCondition: cond,
	}, nil
}

// validateCapability accepts filesystem volumes with single-node or
// read-only access modes.
func validateCapability(c *csi.VolumeCapability) error {
	if c == nil {
		return status.Error(codes.InvalidArgument, "volume_capability is required")
	}
	if c.GetBlock() != nil {
		return status.Error(codes.InvalidArgument, "raw block volumes are not supported")
	}
	if m := c.GetMount(); m != nil && m.GetFsType() != "" && !strings.EqualFold(m.GetFsType(), "ext4") {
		return status.Errorf(codes.InvalidArgument, "fsType %q is not supported; images are ext4", m.GetFsType())
	}
	switch c.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
		return nil
	}
	return status.Errorf(codes.InvalidArgument, "access mode %s is not supported: an ext4 image has a single writer", c.GetAccessMode().GetMode())
}

// keyedMutex serialises operations per volume.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedEntry
}

type keyedEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedEntry{}
	}
	e := k.locks[key]
	if e == nil {
		e = &keyedEntry{}
		k.locks[key] = e
	}
	e.refs++
	k.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
