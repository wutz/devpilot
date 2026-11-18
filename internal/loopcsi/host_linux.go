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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/wutz/devpilot/internal/loopdev"
)

// LinuxHost is the production Host.
type LinuxHost struct{}

var _ Host = LinuxHost{}

// NewHost returns the production Host.
func NewHost() (Host, error) { return LinuxHost{}, nil }

// OpenBacking opens an image for attaching.
func (LinuxHost) OpenBacking(path string, readOnly, directIO bool) (*os.File, error) {
	return loopdev.OpenBacking(path, readOnly, directIO)
}

// Attach binds f to a loop device.
func (LinuxHost) Attach(f *os.File, opts loopdev.Options) (*loopdev.Device, io.Closer, error) {
	d, h, err := loopdev.AttachFile(f, opts)
	if err != nil {
		return nil, nil, err
	}
	return d, h, nil
}

// FindLoops lists devices backed by image.
func (LinuxHost) FindLoops(image string) ([]loopdev.Device, error) {
	return loopdev.FindByBackingFile(image)
}

// Detach releases dev.
func (LinuxHost) Detach(dev string) error { return loopdev.Detach(dev) }

// SetCapacity refreshes the size of dev.
func (LinuxHost) SetCapacity(dev string) error { return loopdev.SetCapacity(dev) }

// MountExt4 mounts dev at target.
func (LinuxHost) MountExt4(dev, target string, options []string) error {
	var flags uintptr
	var data []string
	for _, o := range options {
		switch o {
		case "noatime":
			flags |= unix.MS_NOATIME
		case "nodev":
			flags |= unix.MS_NODEV
		case "nosuid":
			flags |= unix.MS_NOSUID
		case "ro":
			flags |= unix.MS_RDONLY
		default:
			data = append(data, o)
		}
	}
	if err := unix.Mount(dev, target, "ext4", flags, strings.Join(data, ",")); err != nil {
		return fmt.Errorf("mount %s on %s: %w", dev, target, err)
	}
	return nil
}

// BindMount binds source onto target, optionally read-only.
func (LinuxHost) BindMount(source, target string, readOnly bool) error {
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s on %s: %w", source, target, err)
	}
	if readOnly {
		// A bind mount ignores MS_RDONLY; it takes a remount.
		if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
			_ = unix.Unmount(target, 0)
			return fmt.Errorf("remount %s read-only: %w", target, err)
		}
	}
	return nil
}

// Unmount unmounts target; a path that is not mounted or does not exist is
// not an error.
func (LinuxHost) Unmount(target string) error {
	err := unix.Unmount(target, 0)
	if err == nil || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
		return nil
	}
	return fmt.Errorf("unmount %s: %w", target, err)
}

// MountState looks path up in /proc/self/mountinfo.
func (LinuxHost) MountState(path string) (MountState, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return MountState{}, nil
	}
	if err != nil {
		return MountState{}, err
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return MountState{}, err
	}
	defer func() { _ = f.Close() }()
	return parseMountInfo(f, resolved)
}

// Thaw unfreezes the filesystem at path.
func (LinuxHost) Thaw(path string) error { return loopdev.Thaw(path) }

// Trim discards free blocks of the filesystem at path.
func (LinuxHost) Trim(path string, minLen uint64) (uint64, error) { return loopdev.Trim(path, minLen) }

// Statfs reports usage of the filesystem at path.
func (LinuxHost) Statfs(path string) (Usage, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return Usage{}, err
	}
	bs := int64(s.Bsize) //nolint:unconvert // Bsize is int32 on some architectures.
	return Usage{
		TotalBytes:     int64(s.Blocks) * bs,
		AvailableBytes: int64(s.Bavail) * bs,
		UsedBytes:      int64(s.Blocks-s.Bfree) * bs,
		TotalInodes:    int64(s.Files),
		FreeInodes:     int64(s.Ffree),
		UsedInodes:     int64(s.Files - s.Ffree),
	}, nil
}
