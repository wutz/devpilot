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
	"path/filepath"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// volumeMeta is volume.json.
type volumeMeta struct {
	Name           string    `json:"name"`
	Pool           string    `json:"pool"`
	CapacityBytes  int64     `json:"capacityBytes"`
	Created        time.Time `json:"created"`
	SourceSnapshot string    `json:"sourceSnapshot,omitempty"`
	SourceVolume   string    `json:"sourceVolume,omitempty"`
	CopyMethod     string    `json:"copyMethod,omitempty"`
}

// snapshotMeta is meta.json of a file snapshot.
type snapshotMeta struct {
	Name         string    `json:"name"`
	SourceVolume string    `json:"sourceVolume"`
	SizeBytes    int64     `json:"sizeBytes"`
	Created      time.Time `json:"created"`
	CopyMethod   string    `json:"copyMethod"`
}

// ControllerServer implements the CSI controller service. It runs as a
// single replica next to csi-provisioner, csi-resizer and csi-snapshotter,
// with every pool mounted at the same path as on the nodes.
type ControllerServer struct {
	csi.UnimplementedControllerServer

	Pools    Pools
	Runner   Runner
	LeaseTTL time.Duration
	Log      logr.Logger
	Now      func() time.Time

	volLock keyedMutex
}

func (c *ControllerServer) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *ControllerServer) runner() Runner {
	if c.Runner == nil {
		return ExecRunner{}
	}
	return c.Runner
}

func (c *ControllerServer) leaseTTL() time.Duration {
	if c.LeaseTTL == 0 {
		return DefaultLeaseTTL
	}
	return c.LeaseTTL
}

// ControllerGetCapabilities advertises provisioning, snapshots, clones and
// expansion. Publish is not needed: attachRequired is false and the node
// enforces single-writer with locks.
func (c *ControllerServer) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	caps := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
		csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
	}
	out := make([]*csi.ControllerServiceCapability, 0, len(caps))
	for _, t := range caps {
		out = append(out, &csi.ControllerServiceCapability{Type: &csi.ControllerServiceCapability_Rpc{Rpc: &csi.ControllerServiceCapability_RPC{Type: t}}})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: out}, nil
}

// inUse reports why a volume must not be deleted, cloned or copied live: a
// lock on its image (a read-write mount or a raw reader) or a fresh lease.
func (c *ControllerServer) inUse(p Pool, name string) (string, error) {
	held, exclusive, err := probeLock(imagePath(p, name))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if held {
		if exclusive {
			return "mounted read-write", nil
		}
		return "read by a raw (commit) publish", nil
	}
	l, err := readLease(leasePath(p, name))
	if err != nil {
		return "", err
	}
	if l != nil && l.Fresh(c.now(), c.leaseTTL()) {
		return "leased by node " + l.Node, nil
	}
	return "", nil
}

// CreateVolume creates an empty image or copies a snapshot or volume.
func (c *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	name := req.GetName()
	if err := ValidateName(name); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume_capabilities is required")
	}
	for _, vc := range req.GetVolumeCapabilities() {
		if err := validateCapability(vc); err != nil {
			return nil, err
		}
	}
	params, err := ParseVolumeParams(req.GetParameters())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	pool, err := c.Pools.Resolve(params.Pool)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	params.Pool = pool.Name
	size, err := RoundSize(req.GetCapacityRange().GetRequiredBytes(), req.GetCapacityRange().GetLimitBytes())
	if err != nil {
		return nil, status.Error(codes.OutOfRange, err.Error())
	}
	id := VolumeID{Pool: pool.Name, Name: name}
	if len(id.String()) > maxIDLength {
		return nil, status.Errorf(codes.InvalidArgument, "volume id %q is longer than %d bytes", id, maxIDLength)
	}
	unlock := c.volLock.Lock(id.String())
	defer unlock()
	log := c.Log.WithValues("volumeID", id.String())

	src := req.GetVolumeContentSource()
	meta := volumeMeta{Name: name, Pool: pool.Name, Created: c.now().UTC()}
	if s := src.GetSnapshot(); s != nil {
		meta.SourceSnapshot = s.GetSnapshotId()
	}
	if v := src.GetVolume(); v != nil {
		meta.SourceVolume = v.GetVolumeId()
	}

	if existing, err := readJSON[volumeMeta](volumeMetaPath(pool, name)); err == nil {
		if existing.SourceSnapshot != meta.SourceSnapshot || existing.SourceVolume != meta.SourceVolume {
			return nil, status.Errorf(codes.AlreadyExists, "volume %s exists with a different content source", id)
		}
		return c.volumeResponse(id, pool, params, src)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err := os.MkdirAll(volumeDir(pool, name), 0o750); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	img := imagePath(pool, name)
	switch {
	case meta.SourceSnapshot != "":
		method, err := c.restoreSnapshot(meta.SourceSnapshot, img, size)
		if err != nil {
			return nil, err
		}
		meta.CopyMethod = string(method)
	case meta.SourceVolume != "":
		method, err := c.cloneVolume(meta.SourceVolume, img, size)
		if err != nil {
			return nil, err
		}
		meta.CopyMethod = string(method)
	default:
		if err := createImage(ctx, c.runner(), img, size, params.MMP); err != nil {
			return nil, status.Errorf(codes.Internal, "create image: %v", err)
		}
	}
	st, err := os.Stat(img)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	meta.CapacityBytes = st.Size()
	if err := writeJSON(volumeMetaPath(pool, name), meta); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	log.Info("Created volume", "bytes", meta.CapacityBytes, "sourceSnapshot", meta.SourceSnapshot,
		"sourceVolume", meta.SourceVolume, "copyMethod", meta.CopyMethod)
	return c.volumeResponse(id, pool, params, src)
}

func (c *ControllerServer) volumeResponse(id VolumeID, pool Pool, params VolumeParams, src *csi.VolumeContentSource) (*csi.CreateVolumeResponse, error) {
	st, err := os.Stat(imagePath(pool, id.Name))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.CreateVolumeResponse{Volume: &csi.Volume{
		VolumeId:      id.String(),
		CapacityBytes: st.Size(),
		VolumeContext: params.Context(),
		ContentSource: src,
	}}, nil
}

func (c *ControllerServer) restoreSnapshot(raw, img string, size int64) (CopyMethod, error) {
	sid, err := ParseSnapshotID(raw)
	if err != nil {
		return "", status.Error(codes.NotFound, err.Error())
	}
	sp, err := c.Pools.Resolve(sid.Pool)
	if err != nil {
		return "", status.Error(codes.NotFound, err.Error())
	}
	return c.copyInto(snapshotImagePath(sp, sid), img, size)
}

func (c *ControllerServer) cloneVolume(raw, img string, size int64) (CopyMethod, error) {
	vid, err := ParseVolumeID(raw)
	if err != nil {
		return "", status.Error(codes.NotFound, err.Error())
	}
	vp, err := c.Pools.Resolve(vid.Pool)
	if err != nil {
		return "", status.Error(codes.NotFound, err.Error())
	}
	why, err := c.inUse(vp, vid.Name)
	if err != nil {
		return "", status.Error(codes.Internal, err.Error())
	}
	if why != "" {
		return "", status.Errorf(codes.FailedPrecondition,
			"source volume %s is %s; clone a stopped volume or create a snapshot first", vid, why)
	}
	return c.copyInto(imagePath(vp, vid.Name), img, size)
}

func (c *ControllerServer) copyInto(srcImg, img string, size int64) (CopyMethod, error) {
	st, err := os.Stat(srcImg)
	if errors.Is(err, os.ErrNotExist) {
		return "", status.Errorf(codes.NotFound, "source image %s does not exist", srcImg)
	}
	if err != nil {
		return "", status.Error(codes.Internal, err.Error())
	}
	if st.Size() > size {
		return "", status.Errorf(codes.OutOfRange, "requested %d bytes is smaller than the source (%d bytes)", size, st.Size())
	}
	method, err := copyImage(srcImg, img, StrategyAuto)
	if err != nil {
		return "", status.Error(codes.Internal, err.Error())
	}
	if err := growImage(img, size); err != nil {
		return "", status.Error(codes.Internal, err.Error())
	}
	return method, nil
}

// DeleteVolume removes the volume directory. It refuses while the image is
// in use or while CephFS snapshots of it exist.
func (c *ControllerServer) DeleteVolume(_ context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	id, err := ParseVolumeID(req.GetVolumeId())
	if err != nil {
		// An ID this driver never issued cannot exist.
		return &csi.DeleteVolumeResponse{}, nil //nolint:nilerr // CSI: deleting an unknown volume succeeds.
	}
	pool, err := c.Pools.Resolve(id.Pool)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	unlock := c.volLock.Lock(id.String())
	defer unlock()
	dir := volumeDir(pool, id.Name)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return &csi.DeleteVolumeResponse{}, nil
	}
	why, err := c.inUse(pool, id.Name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if why != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is %s", id, why)
	}
	if snaps := cephSnapshots(dir); len(snaps) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s still has CephFS snapshots %v", id, snaps)
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	c.Log.Info("Deleted volume", "volumeID", id.String())
	return &csi.DeleteVolumeResponse{}, nil
}

// cephSnapshots lists the snapshots taken of dir itself. Entries starting
// with '_' are snapshots of an ancestor directory and are not ours.
func cephSnapshots(dir string) []string {
	entries, err := os.ReadDir(filepath.Join(dir, CephSnapDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "_") {
			out = append(out, e.Name())
		}
	}
	return out
}

// ControllerExpandVolume grows the image file. The node grows the loop
// device and the filesystem.
func (c *ControllerServer) ControllerExpandVolume(_ context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	id, err := ParseVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	pool, err := c.Pools.Resolve(id.Pool)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	size, err := RoundSize(req.GetCapacityRange().GetRequiredBytes(), req.GetCapacityRange().GetLimitBytes())
	if err != nil {
		return nil, status.Error(codes.OutOfRange, err.Error())
	}
	unlock := c.volLock.Lock(id.String())
	defer unlock()
	img := imagePath(pool, id.Name)
	if err := growImage(img, size); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, status.Errorf(codes.NotFound, "volume %s does not exist", id)
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	st, err := os.Stat(img)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	c.Log.Info("Expanded image", "volumeID", id.String(), "bytes", st.Size())
	return &csi.ControllerExpandVolumeResponse{CapacityBytes: st.Size(), NodeExpansionRequired: true}, nil
}

// ValidateVolumeCapabilities confirms capabilities this driver supports.
func (c *ControllerServer) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	id, err := ParseVolumeID(req.GetVolumeId())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	pool, err := c.Pools.Resolve(id.Pool)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if _, err := os.Stat(imagePath(pool, id.Name)); err != nil {
		return nil, status.Errorf(codes.NotFound, "volume %s does not exist", id)
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume_capabilities is required")
	}
	for _, vc := range req.GetVolumeCapabilities() {
		if err := validateCapability(vc); err != nil {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: err.Error()}, nil //nolint:nilerr // reported in Message per CSI.
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
		VolumeContext:      req.GetVolumeContext(),
		VolumeCapabilities: req.GetVolumeCapabilities(),
		Parameters:         req.GetParameters(),
	}}, nil
}

// CreateSnapshot snapshots a volume synchronously; the snapshot is ready
// when the call returns.
//
// A reflink or a CephFS snapshot is atomic and therefore crash-consistent
// even while the volume is mounted. A copy of a mounted image is not, so it
// is refused unless the VolumeSnapshotClass sets allowLiveCopy, which the
// DevPilot controller's class does because it freezes the filesystem around
// CreateSnapshot (the freeze then lasts as long as the copy).
func (c *ControllerServer) CreateSnapshot(_ context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	name := req.GetName()
	if err := ValidateName(name); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	vid, err := ParseVolumeID(req.GetSourceVolumeId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	pool, err := c.Pools.Resolve(vid.Pool)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	sp, err := ParseSnapshotParams(req.GetParameters())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	unlock := c.volLock.Lock(vid.String())
	defer unlock()
	srcImg := imagePath(pool, vid.Name)
	if _, err := os.Stat(srcImg); err != nil {
		return nil, status.Errorf(codes.NotFound, "source volume %s does not exist", vid)
	}
	log := c.Log.WithValues("sourceVolumeID", vid.String(), "snapshot", name, "strategy", sp.Strategy)

	if sp.Strategy == StrategyCephSnapDir {
		sid := SnapshotID{Pool: pool.Name, Kind: SnapshotCephDir, Volume: vid.Name, Name: name}
		if len(sid.String()) > maxIDLength {
			return nil, status.Errorf(codes.InvalidArgument, "snapshot id %q is longer than %d bytes", sid, maxIDLength)
		}
		dir := filepath.Join(volumeDir(pool, vid.Name), CephSnapDir, name)
		if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, status.Errorf(codes.Internal, "create CephFS snapshot %s: %v", dir, err)
		}
		st, err := os.Stat(filepath.Join(dir, ImageName))
		if err != nil {
			return nil, status.Errorf(codes.Internal, "CephFS snapshot %s has no image (is this CephFS with snapshots enabled?): %v", dir, err)
		}
		log.Info("Created CephFS snapshot")
		return snapshotResponse(sid, vid, st.Size(), st.ModTime()), nil
	}

	sid := SnapshotID{Pool: pool.Name, Kind: SnapshotFile, Name: name}
	sdir := filepath.Join(pool.Path, SnapshotsDir, name)
	metaPath := filepath.Join(sdir, SnapshotMetaName)
	if m, err := readJSON[snapshotMeta](metaPath); err == nil {
		if m.SourceVolume != vid.String() {
			return nil, status.Errorf(codes.AlreadyExists, "snapshot %s exists for volume %s", name, m.SourceVolume)
		}
		return snapshotResponse(sid, vid, m.SizeBytes, m.Created), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, status.Error(codes.Internal, err.Error())
	}

	strategy := sp.Strategy
	why, err := c.inUse(pool, vid.Name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if why != "" && !sp.AllowLiveCopy {
		if strategy == StrategyCopy {
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %s is %s and a copy would be torn; use reflink/cephfs-snapdir, stop the volume, or set %s with a filesystem freeze",
				vid, why, ParamAllowLiveCopy)
		}
		strategy = StrategyReflink // atomic or nothing
	}
	if err := os.MkdirAll(sdir, 0o750); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	method, err := copyImage(srcImg, filepath.Join(sdir, ImageName), strategy)
	if err != nil {
		_ = os.RemoveAll(sdir)
		if errors.Is(err, errReflinkUnsupported) && why != "" {
			return nil, status.Errorf(codes.FailedPrecondition,
				"volume %s is %s, the filesystem has no reflink, and %s is not set", vid, why, ParamAllowLiveCopy)
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	st, err := os.Stat(filepath.Join(sdir, ImageName))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	m := snapshotMeta{Name: name, SourceVolume: vid.String(), SizeBytes: st.Size(), Created: c.now().UTC(), CopyMethod: string(method)}
	if err := writeJSON(metaPath, m); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	log.Info("Created snapshot", "copyMethod", method, "bytes", st.Size(), "live", why != "")
	return snapshotResponse(sid, vid, m.SizeBytes, m.Created), nil
}

func snapshotResponse(sid SnapshotID, vid VolumeID, size int64, created time.Time) *csi.CreateSnapshotResponse {
	return &csi.CreateSnapshotResponse{Snapshot: &csi.Snapshot{
		SnapshotId:     sid.String(),
		SourceVolumeId: vid.String(),
		SizeBytes:      size,
		CreationTime:   timestamppb.New(created),
		ReadyToUse:     true,
	}}
}

// DeleteSnapshot removes a file snapshot or a CephFS snapshot.
func (c *ControllerServer) DeleteSnapshot(_ context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	sid, err := ParseSnapshotID(req.GetSnapshotId())
	if err != nil {
		return &csi.DeleteSnapshotResponse{}, nil //nolint:nilerr // CSI: deleting an unknown snapshot succeeds.
	}
	pool, err := c.Pools.Resolve(sid.Pool)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if sid.Kind == SnapshotCephDir {
		unlock := c.volLock.Lock(VolumeID{Pool: sid.Pool, Name: sid.Volume}.String())
		defer unlock()
		dir := filepath.Join(volumeDir(pool, sid.Volume), CephSnapDir, sid.Name)
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, status.Errorf(codes.Internal, "remove CephFS snapshot %s: %v", dir, err)
		}
	} else if err := os.RemoveAll(filepath.Join(pool.Path, SnapshotsDir, sid.Name)); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	c.Log.Info("Deleted snapshot", "snapshotID", sid.String())
	return &csi.DeleteSnapshotResponse{}, nil
}

func readJSON[T any](path string) (T, error) {
	var v T
	b, err := os.ReadFile(path)
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, fmt.Errorf("parse %s: %w", path, err)
	}
	return v, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'))
}
