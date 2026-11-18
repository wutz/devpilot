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

// Package loopcsi implements the loop.devpilot.wutz.dev CSI driver: DevBox
// root disks stored as sparse ext4 images (disk.img) on a shared filesystem
// (IBM Storage Scale, VAST NFS, CephFS) and mounted on the node through a
// loop device with direct I/O.
//
// On-disk layout of one pool (a directory of the shared filesystem mounted
// at the same path on every node and on the controller):
//
//	<pool>/<volume>/disk.img            the ext4 image
//	<pool>/<volume>/disk.img.lease      who has it mounted read-write
//	<pool>/<volume>/volume.json         creation metadata
//	<pool>/.snapshots/<snap>/disk.img   file snapshots (reflink or copy)
//	<pool>/.snapshots/<snap>/meta.json
//	<pool>/<volume>/.snap/<snap>/       CephFS snapshots (snapdir strategy)
//
// Single writer is enforced three times: an OFD write lock on disk.img held
// by the loop device's own file reference, a heartbeat lease file, and ext4
// multi-mount protection inside the image.
package loopcsi

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/wutz/devpilot/internal/loopdev"
)

const (
	// DriverName is the CSI driver name and StorageClass provisioner.
	DriverName = "loop.devpilot.wutz.dev"

	// ImageName is the ext4 image inside a volume directory.
	ImageName = "disk.img"
	// LeaseName is the read-write lease next to the image.
	LeaseName = "disk.img.lease"
	// VolumeMetaName records how a volume was created.
	VolumeMetaName = "volume.json"
	// SnapshotsDir holds file snapshots inside a pool.
	SnapshotsDir = ".snapshots"
	// SnapshotMetaName records the source of a file snapshot.
	SnapshotMetaName = "meta.json"
	// CephSnapDir is the CephFS virtual snapshot directory.
	CephSnapDir = ".snap"

	// MinVolumeSize is the smallest image CreateVolume makes.
	MinVolumeSize int64 = 1 << 30
	sizeAlign     int64 = 1 << 20

	maxIDLength   = 128
	maxNameLength = 90
)

// StorageClass and VolumeSnapshotClass parameters.
const (
	ParamPool             = "pool"
	ParamDirectIO         = "directIO"
	ParamTrimOnUnmount    = "trimOnUnmount"
	ParamTrimMinLength    = "trimMinLength"
	ParamMMP              = "mmp"
	ParamSnapshotStrategy = "snapshotStrategy"
	ParamAllowLiveCopy    = "allowLiveCopy"
)

var (
	poolNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$`)
	nameRE     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
)

// Pool is one shared-filesystem directory volumes are created in.
type Pool struct {
	Name string
	Path string
}

// ParsePool parses a --pool flag value of the form name=/absolute/path.
func ParsePool(raw string) (Pool, error) {
	name, path, ok := strings.Cut(raw, "=")
	if !ok {
		return Pool{}, fmt.Errorf("pool %q must be name=/absolute/path", raw)
	}
	if !poolNameRE.MatchString(name) {
		return Pool{}, fmt.Errorf("pool name %q must be 1-32 lowercase alphanumerics or '-'", name)
	}
	if !filepath.IsAbs(path) {
		return Pool{}, fmt.Errorf("pool %q path %q must be absolute", name, path)
	}
	return Pool{Name: name, Path: filepath.Clean(path)}, nil
}

// Pools indexes configured pools by name.
type Pools map[string]Pool

// NewPools validates a list of pools.
func NewPools(list []Pool) (Pools, error) {
	out := make(Pools, len(list))
	for _, p := range list {
		if _, dup := out[p.Name]; dup {
			return nil, fmt.Errorf("pool %q configured twice", p.Name)
		}
		out[p.Name] = p
	}
	return out, nil
}

// Resolve returns the pool a StorageClass names; with exactly one pool
// configured the parameter may be omitted.
func (p Pools) Resolve(name string) (Pool, error) {
	if name == "" {
		if len(p) == 1 {
			for _, only := range p {
				return only, nil
			}
		}
		return Pool{}, fmt.Errorf("parameter %q is required when %d pools are configured", ParamPool, len(p))
	}
	pool, ok := p[name]
	if !ok {
		return Pool{}, fmt.Errorf("pool %q is not configured on this driver", name)
	}
	return pool, nil
}

// ValidateName checks a volume or snapshot name chosen by the CO. Names are
// used as directory names on the shared filesystem.
func ValidateName(name string) error {
	if len(name) == 0 || len(name) > maxNameLength {
		return fmt.Errorf("name %q must be 1-%d characters", name, maxNameLength)
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("name %q must be lowercase alphanumerics, '-' or '.'", name)
	}
	return nil
}

// VolumeID identifies a volume as <pool>/<name>.
type VolumeID struct {
	Pool string
	Name string
}

func (v VolumeID) String() string { return v.Pool + "/" + v.Name }

// Key is a filesystem-safe form of the ID for node-local state.
func (v VolumeID) Key() string { return v.Pool + "_" + v.Name }

// ParseVolumeID parses <pool>/<name>.
func ParseVolumeID(raw string) (VolumeID, error) {
	pool, name, ok := strings.Cut(raw, "/")
	if !ok || !poolNameRE.MatchString(pool) || ValidateName(name) != nil {
		return VolumeID{}, fmt.Errorf("volume id %q is not <pool>/<name>", raw)
	}
	return VolumeID{Pool: pool, Name: name}, nil
}

// SnapshotKind says where a snapshot lives.
type SnapshotKind string

const (
	// SnapshotFile is a copy or reflink under <pool>/.snapshots.
	SnapshotFile SnapshotKind = "snap"
	// SnapshotCephDir is a CephFS snapshot of the volume directory.
	SnapshotCephDir SnapshotKind = "snapdir"
)

// SnapshotID identifies a snapshot: <pool>/snap/<name> or
// <pool>/snapdir/<volume>/<name>.
type SnapshotID struct {
	Pool   string
	Kind   SnapshotKind
	Volume string // SnapshotCephDir only
	Name   string
}

func (s SnapshotID) String() string {
	if s.Kind == SnapshotCephDir {
		return s.Pool + "/" + string(SnapshotCephDir) + "/" + s.Volume + "/" + s.Name
	}
	return s.Pool + "/" + string(SnapshotFile) + "/" + s.Name
}

// ParseSnapshotID parses a snapshot handle.
func ParseSnapshotID(raw string) (SnapshotID, error) {
	parts := strings.Split(raw, "/")
	bad := fmt.Errorf("snapshot id %q is not <pool>/snap/<name> or <pool>/snapdir/<volume>/<name>", raw)
	if len(parts) < 3 || !poolNameRE.MatchString(parts[0]) {
		return SnapshotID{}, bad
	}
	switch {
	case parts[1] == string(SnapshotFile) && len(parts) == 3 && ValidateName(parts[2]) == nil:
		return SnapshotID{Pool: parts[0], Kind: SnapshotFile, Name: parts[2]}, nil
	case parts[1] == string(SnapshotCephDir) && len(parts) == 4 && ValidateName(parts[2]) == nil && ValidateName(parts[3]) == nil:
		return SnapshotID{Pool: parts[0], Kind: SnapshotCephDir, Volume: parts[2], Name: parts[3]}, nil
	}
	return SnapshotID{}, bad
}

// Paths of a volume inside its pool.
func volumeDir(p Pool, name string) string      { return filepath.Join(p.Path, name) }
func imagePath(p Pool, name string) string      { return filepath.Join(p.Path, name, ImageName) }
func leasePath(p Pool, name string) string      { return filepath.Join(p.Path, name, LeaseName) }
func volumeMetaPath(p Pool, name string) string { return filepath.Join(p.Path, name, VolumeMetaName) }

// snapshotImagePath is where a snapshot's image can be read.
func snapshotImagePath(p Pool, id SnapshotID) string {
	if id.Kind == SnapshotCephDir {
		return filepath.Join(p.Path, id.Volume, CephSnapDir, id.Name, ImageName)
	}
	return filepath.Join(p.Path, SnapshotsDir, id.Name, ImageName)
}

// RoundSize turns a CSI capacity range into an image size: at least
// MinVolumeSize, rounded up to 1 MiB, and within limit when one is set.
func RoundSize(required, limit int64) (int64, error) {
	if required < 0 || limit < 0 {
		return 0, fmt.Errorf("capacity must not be negative")
	}
	size := max(required, MinVolumeSize)
	size = (size + sizeAlign - 1) / sizeAlign * sizeAlign
	if limit > 0 && size > limit {
		if floor := limit / sizeAlign * sizeAlign; floor >= required && floor >= MinVolumeSize {
			return floor, nil
		}
		return 0, fmt.Errorf("no size between %d and %d is a multiple of 1MiB of at least %d bytes", required, limit, MinVolumeSize)
	}
	return size, nil
}

// SnapshotStrategy selects how CreateSnapshot copies an image.
type SnapshotStrategy string

const (
	// StrategyAuto tries a reflink and falls back to a sparse copy.
	StrategyAuto SnapshotStrategy = "auto"
	// StrategyReflink requires FICLONE (XFS, Btrfs, NFS 4.2 servers with CLONE).
	StrategyReflink SnapshotStrategy = "reflink"
	// StrategyCopy always copies (server-side via copy_file_range when the
	// filesystem offers it).
	StrategyCopy SnapshotStrategy = "copy"
	// StrategyCephSnapDir creates a CephFS snapshot of the volume directory.
	StrategyCephSnapDir SnapshotStrategy = "cephfs-snapdir"
)

// VolumeParams are the StorageClass parameters the node needs; they travel
// in the volume context.
type VolumeParams struct {
	Pool          string
	DirectIO      loopdev.DirectIOMode
	TrimOnUnmount bool
	TrimMinLength uint64
	MMP           bool
}

// injectedParamPrefixes are key prefixes the CSI sidecars add to the volume
// and snapshot attributes they pass back. They are not StorageClass
// parameters and must be ignored rather than rejected:
//   - csi.storage.k8s.io/* are provisioner substitutions (pv/name, pvc/name,
//     ...) that appear in the volume context.
//   - storage.kubernetes.io/* is the provisioner identity external-provisioner
//     writes into the PV's volumeAttributes, which kubelet hands back to
//     NodePublishVolume in the volume context.
var injectedParamPrefixes = []string{"csi.storage.k8s.io/", "storage.kubernetes.io/"}

func isInjectedParam(key string) bool {
	for _, prefix := range injectedParamPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// ParseVolumeParams validates StorageClass parameters. Unknown keys are
// rejected so typos do not silently fall back to defaults; keys added by the
// CSI sidecars (see injectedParamPrefixes) are ignored.
func ParseVolumeParams(in map[string]string) (VolumeParams, error) {
	out := VolumeParams{DirectIO: loopdev.DirectIORequired, TrimOnUnmount: true, TrimMinLength: 1 << 20, MMP: true}
	for k, v := range in {
		var err error
		switch k {
		case ParamPool:
			out.Pool = v
		case ParamDirectIO:
			out.DirectIO, err = loopdev.ParseDirectIOMode(v)
		case ParamTrimOnUnmount:
			out.TrimOnUnmount, err = strconv.ParseBool(v)
		case ParamTrimMinLength:
			out.TrimMinLength, err = strconv.ParseUint(v, 10, 64)
		case ParamMMP:
			out.MMP, err = strconv.ParseBool(v)
		default:
			if !isInjectedParam(k) {
				err = fmt.Errorf("unknown parameter")
			}
		}
		if err != nil {
			return VolumeParams{}, fmt.Errorf("parameter %s=%q: %w", k, v, err)
		}
	}
	return out, nil
}

// Context renders the parameters the node plugin reads at publish time.
func (p VolumeParams) Context() map[string]string {
	return map[string]string{
		ParamPool:          p.Pool,
		ParamDirectIO:      string(p.DirectIO),
		ParamTrimOnUnmount: strconv.FormatBool(p.TrimOnUnmount),
		ParamTrimMinLength: strconv.FormatUint(p.TrimMinLength, 10),
	}
}

// SnapshotParams are VolumeSnapshotClass parameters.
type SnapshotParams struct {
	Strategy SnapshotStrategy
	// AllowLiveCopy permits a non-atomic copy of an image that is mounted
	// read-write. Only safe when the caller froze the filesystem for the
	// duration of CreateSnapshot, as the DevPilot controller does.
	AllowLiveCopy bool
}

// ParseSnapshotParams validates VolumeSnapshotClass parameters.
func ParseSnapshotParams(in map[string]string) (SnapshotParams, error) {
	out := SnapshotParams{Strategy: StrategyAuto}
	for k, v := range in {
		var err error
		switch k {
		case ParamSnapshotStrategy:
			switch SnapshotStrategy(v) {
			case StrategyAuto, StrategyReflink, StrategyCopy, StrategyCephSnapDir:
				out.Strategy = SnapshotStrategy(v)
			default:
				err = fmt.Errorf("must be %s, %s, %s or %s", StrategyAuto, StrategyReflink, StrategyCopy, StrategyCephSnapDir)
			}
		case ParamAllowLiveCopy:
			out.AllowLiveCopy, err = strconv.ParseBool(v)
		default:
			if !isInjectedParam(k) {
				err = fmt.Errorf("unknown parameter")
			}
		}
		if err != nil {
			return SnapshotParams{}, fmt.Errorf("parameter %s=%q: %w", k, v, err)
		}
	}
	return out, nil
}
