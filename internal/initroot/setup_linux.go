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
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// lgetxattr reads an xattr without following symlinks; used for opaque scans.
func lgetxattr(path, attr string) ([]byte, error) {
	buf := make([]byte, 256)
	n, err := unix.Lgetxattr(path, attr, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// BaseLower is where the container's initial / is pinned read-only.
const BaseLower = "/run/devpilot/base-lower"

// Setup assembles the overlay root, migrates the container's submounts and
// pivots into it. When it returns the process runs inside the persistent
// root and holds no capability beyond what the user image was granted.
func Setup(p Paths) error {
	if err := CheckOverlaySupport(p); err != nil {
		return err
	}
	if err := EnsureLayout(p.Root); err != nil {
		return err
	}

	// Everything below stays inside the container's mount namespace, so a
	// failure cannot leak mounts onto the node.
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}

	if err := os.MkdirAll(p.Lower, 0o755); err != nil {
		return fmt.Errorf("create base-lower: %w", err)
	}
	// MS_BIND without MS_REC: the lowerdir must be the pristine image view,
	// not the image plus the nvidia/kubelet bind mounts layered on top of it.
	if err := unix.Mount("/", p.Lower, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind initial root to base-lower: %w", err)
	}
	if err := unix.Mount("", p.Lower, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("remount base-lower read-only: %w", err)
	}
	// A recursive bind also cloned /run/devpilot and the root disk inside the
	// lower view; detach them so the lower layer is exactly the image.
	for _, dir := range []string{filepath.Join(p.Lower, strings.TrimSuffix(p.Root, "/")), filepath.Join(p.Lower, "/run/devpilot")} {
		_ = unix.Unmount(dir, unix.MNT_DETACH)
	}

	if err := unix.Mount("overlay", p.Merged(), "overlay", 0, p.Options()); err != nil {
		return fmt.Errorf("mount overlay at %s: %w", p.Merged(), err)
	}

	if err := migrateSubmounts(p); err != nil {
		return err
	}
	if err := pivotRoot(p); err != nil {
		return err
	}
	return DropCapabilities()
}

// migrateSubmounts re-binds every mount that lived on the initial root into
// the merged root, parents first. OverlayFS lowerdir does not traverse
// submounts, so without this step the nvidia driver files, /dev/shm and the
// service account token vanish after pivot_root.
func migrateSubmounts(p Paths) error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("open mountinfo: %w", err)
	}
	defer func() { _ = f.Close() }()

	entries, err := ParseMountInfo(f)
	if err != nil {
		return err
	}

	for _, entry := range SubmountsUnderRoot(entries, p.Root, "/run/devpilot") {
		target := filepath.Join(p.Merged(), entry.Target)
		if err := ensurePlaceholder(filepath.Dir(target), filepath.Base(target), entry.FSType); err != nil {
			return err
		}
		if err := unix.Mount(entry.Target, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("bind mount %s: %w", entry.Target, err)
		}
	}
	return nil
}

// ensurePlaceholder creates an empty file or directory at the bind target,
// but only when the path is not already visible through the overlay. The
// image usually provides the parent directory; creating it unconditionally
// would copy it up into upper/ and bloat the commit layer with empty dirs.
func ensurePlaceholder(dir, name, fsType string) error {
	target := filepath.Join(dir, name)
	if _, err := os.Lstat(target); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", target, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create parent of %s: %w", target, err)
	}
	// Character/block devices and sockets must be placeholders of the same
	// kind, otherwise the kernel refuses the bind onto a regular file.
	switch fsType {
	case "devpts":
		return os.Mkdir(target, 0o755)
	default:
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("create placeholder %s: %w", target, err)
		}
		return f.Close()
	}
}

// pivotRoot swaps the merged overlay in as / and lazily unmounts the old
// root. After this the raw upper/ and work/ directories are unreachable from
// inside the container, so a user cannot corrupt the overlay state by hand.
func pivotRoot(p Paths) error {
	oldRoot := filepath.Join(p.Merged(), ".old_root")
	if err := os.MkdirAll(oldRoot, 0o700); err != nil {
		return fmt.Errorf("create .old_root: %w", err)
	}
	if err := unix.PivotRoot(p.Merged(), oldRoot); err != nil {
		return fmt.Errorf("pivot_root to %s: %w", p.Merged(), err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir to new root: %w", err)
	}
	if err := unix.Unmount("/.old_root", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	if err := os.Remove("/.old_root"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old root mountpoint: %w", err)
	}
	return nil
}

// DropCapabilities removes the capabilities devpilot-init needed for mounting
// so the long-running PID 1 (and everything it spawns) cannot remount or
// bypass the new root. Failures are advisory: without CAP_SETPCAP in the
// bounding set the drops are already impossible to regain.
func DropCapabilities() error {
	var errs []error
	for _, capability := range []int{unix.CAP_SYS_ADMIN, unix.CAP_SYS_MODULE, unix.CAP_SYS_PTRACE, unix.CAP_SYS_CHROOT, unix.CAP_SETUID, unix.CAP_SETGID} {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(capability), 0, 0, 0); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.EPERM) {
			errs = append(errs, fmt.Errorf("drop capability %d from bounding set: %w", capability, err))
		}
	}

	caps := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capget(&caps, &data[0]); err != nil {
		return errors.Join(append(errs, fmt.Errorf("read capabilities: %w", err))...)
	}
	mask := uint32(1)<<unix.CAP_SYS_ADMIN | uint32(1)<<unix.CAP_SYS_MODULE | uint32(1)<<unix.CAP_SYS_PTRACE | uint32(1)<<unix.CAP_SYS_CHROOT | uint32(1)<<unix.CAP_SETUID | uint32(1)<<unix.CAP_SETGID
	for i := range data {
		data[i].Effective &^= mask
		data[i].Permitted &^= mask
		data[i].Inheritable &^= mask
	}
	if err := unix.Capset(&caps, &data[0]); err != nil && !errors.Is(err, unix.EPERM) {
		errs = append(errs, fmt.Errorf("clear capabilities: %w", err))
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil && !errors.Is(err, unix.EINVAL) {
		errs = append(errs, fmt.Errorf("clear ambient capabilities: %w", err))
	}
	return errors.Join(errs...)
}

// Sync flushes dirty pages to the root disk before a snapshot or shutdown.
func Sync() error {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Sync()
		return fmt.Errorf("open / for syncfs: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	// syncfs only waits for the filesystem behind /. The new root is the
	// overlay, whose writes land in upper/ on the root disk, so one syncfs
	// covers every file the user touched.
	if err := unix.Syncfs(fd); err != nil {
		unix.Sync()
		return fmt.Errorf("syncfs root: %w", err)
	}
	return nil
}
