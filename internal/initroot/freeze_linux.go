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

package initroot

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// FIFREEZE and FITHAW are _IOWR('X', 119, int) and _IOWR('X', 120, int) from
// <linux/fs.h>. golang.org/x/sys/unix does not export them; the encoding is
// the same on every architecture devpilot ships for (amd64, arm64).
const (
	ioctlFIFREEZE = 0xc0045877
	ioctlFITHAW   = 0xc0045878
)

// FindRootDiskMeta returns the root disk's .meta directory as reached through
// the descriptors devpilot-init (PID 1) keeps open after pivot_root.
func FindRootDiskMeta(procRoot string) (*os.File, error) {
	return findRootDiskMeta(procRoot, linuxRootDiskInspector)
}

var linuxRootDiskInspector = rootDiskInspector{
	openDir: func(path string) (*os.File, error) {
		// O_NONBLOCK is belt and braces: O_DIRECTORY already refuses a FIFO
		// before it is opened, so the open can never wait for a writer.
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), path), nil
	},
	fsMagic: func(f *os.File) (int64, error) {
		var st unix.Statfs_t
		if err := unix.Fstatfs(int(f.Fd()), &st); err != nil {
			return 0, err
		}
		return int64(st.Type), nil //nolint:unconvert // Type is int32 on 32-bit architectures
	},
	hasRegular: func(f *os.File, name string) bool {
		var st unix.Stat_t
		if err := unix.Fstatat(int(f.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return false
		}
		return st.Mode&unix.S_IFMT == unix.S_IFREG
	},
}

// freezeFS flushes the filesystem behind dir and freezes it. FIFREEZE syncs
// as well, but the explicit syncfs moves the bulk of the writeback out of the
// frozen window, where every writer in the DevBox blocks.
func freezeFS(dir *os.File) error {
	fd := int(dir.Fd())
	if err := unix.Syncfs(fd); err != nil {
		return fmt.Errorf("syncfs root disk: %w", err)
	}
	if err := unix.IoctlSetInt(fd, ioctlFIFREEZE, 0); err != nil {
		return fmt.Errorf("FIFREEZE root disk: %w", err)
	}
	return nil
}

// thawFS releases a frozen filesystem. EINVAL is what the kernel returns for
// a filesystem that is not frozen, which is the state thawing aims for.
func thawFS(dir *os.File) error {
	if err := unix.IoctlSetInt(int(dir.Fd()), ioctlFITHAW, 0); err != nil && !errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("FITHAW root disk: %w", err)
	}
	return nil
}
