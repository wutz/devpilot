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
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Filesystem ioctls golang.org/x/sys does not export (linux/fs.h).
const (
	ioctlFIFREEZE = 0xC0045877 // _IOWR('X', 119, int)
	ioctlFITHAW   = 0xC0045878 // _IOWR('X', 120, int)
	ioctlFITRIM   = 0xC0185879 // _IOWR('X', 121, struct fstrim_range)
)

type fstrimRange struct {
	Start  uint64
	Len    uint64
	MinLen uint64
}

// Thaw unfreezes the filesystem mounted at mountpoint. A filesystem that is
// not frozen is not an error: the node plugin calls Thaw defensively before
// every unmount because a freeze helper killed with SIGKILL leaves the
// filesystem frozen and an unmount of a frozen filesystem hangs.
func Thaw(mountpoint string) error {
	f, err := os.Open(mountpoint)
	if err != nil {
		return fmt.Errorf("open %s: %w", mountpoint, err)
	}
	defer func() { _ = f.Close() }()
	err = unix.IoctlSetInt(int(f.Fd()), ioctlFITHAW, 0)
	if err != nil && !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("FITHAW %s: %w", mountpoint, err)
	}
	return nil
}

// Trim discards free filesystem blocks so the loop device punches holes in
// the backing image. It returns the number of bytes the filesystem reported
// as trimmed. Backends without hole punching return an error the caller is
// expected to log and ignore.
func Trim(mountpoint string, minLen uint64) (uint64, error) {
	f, err := os.Open(mountpoint)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", mountpoint, err)
	}
	defer func() { _ = f.Close() }()
	r := fstrimRange{Len: ^uint64(0), MinLen: minLen}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), ioctlFITRIM, uintptr(unsafe.Pointer(&r)))
	if errno != 0 {
		return 0, fmt.Errorf("FITRIM %s: %w", mountpoint, errno)
	}
	return r.Len, nil
}

// Freeze freezes the filesystem mounted at mountpoint. Production freezes run
// inside the DevBox pod (devpilot-init --freeze); this exists for tests and
// for operators debugging a node.
func Freeze(mountpoint string) error {
	f, err := os.Open(mountpoint)
	if err != nil {
		return fmt.Errorf("open %s: %w", mountpoint, err)
	}
	defer func() { _ = f.Close() }()
	if err := unix.IoctlSetInt(int(f.Fd()), ioctlFIFREEZE, 0); err != nil {
		return fmt.Errorf("FIFREEZE %s: %w", mountpoint, err)
	}
	return nil
}
