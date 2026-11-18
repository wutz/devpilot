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
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	loopControl = "/dev/loop-control"
	loopMajor   = 7
	// attachRetries bounds the LOOP_CTL_GET_FREE race with other losetup
	// users: the free device can be claimed between the two ioctls.
	attachRetries = 16
)

// Attach opens backing and binds it to a free loop device. See AttachFile.
func Attach(backing string, opts Options) (*Device, *os.File, error) {
	file, err := OpenBacking(backing, opts.ReadOnly, opts.DirectIO != DirectIOOff)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	return AttachFile(file, opts)
}

// AttachFile binds an open backing file to a free loop device. The kernel
// takes its own reference to the file's open file description, so OFD and
// flock locks taken on file stay held for as long as the device is bound,
// even after the caller closes file or exits.
//
// The device is created with LO_FLAGS_AUTOCLEAR, so it detaches itself when
// the last opener goes away: if the caller closes the returned device file
// before a mount takes a reference, the device is released, and after a
// successful mount the device lives exactly as long as the mount. The
// returned device file must be closed by the caller once the device is
// mounted (or on failure).
func AttachFile(file *os.File, opts Options) (*Device, *os.File, error) {
	if opts.BlockSize == 0 {
		opts.BlockSize = DefaultBlockSize
	}
	if opts.DirectIO == "" {
		opts.DirectIO = DirectIORequired
	}
	wantDIO := opts.DirectIO != DirectIOOff
	backing := file.Name()

	if isGPFS, err := isGPFSFile(file); err == nil && isGPFS {
		hasODirect, flagErr := hasODirectFlag(file)
		if flagErr != nil {
			return nil, nil, fmt.Errorf("%s: inspect file flags: %w", backing, flagErr)
		}
		if err := validateGPFSDirectIOMode(hasODirect, opts.DirectIO); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", backing, err)
		}
		wantDIO = false
	}

	var lastErr error
	for range attachRetries {
		num, err := getFree()
		if err != nil {
			return nil, nil, err
		}
		dev, err := openLoop(num, opts.ReadOnly)
		if err != nil {
			return nil, nil, err
		}
		err = configure(dev, file, opts, wantDIO)
		if errors.Is(err, unix.EBUSY) {
			_ = dev.Close()
			lastErr = err
			continue
		}
		if err != nil {
			_ = dev.Close()
			return nil, nil, fmt.Errorf("configure /dev/loop%d for %s: %w", num, backing, err)
		}
		info, err := unix.IoctlLoopGetStatus64(int(dev.Fd()))
		if err != nil {
			_ = unix.IoctlSetInt(int(dev.Fd()), unix.LOOP_CLR_FD, 0)
			_ = dev.Close()
			return nil, nil, fmt.Errorf("read back /dev/loop%d status: %w", num, err)
		}
		d := &Device{
			Path:        dev.Name(),
			Number:      num,
			DirectIO:    info.Flags&unix.LO_FLAGS_DIRECT_IO != 0,
			BackingFile: backing,
		}
		if wantDIO && !d.DirectIO && opts.DirectIO == DirectIORequired {
			_ = unix.IoctlSetInt(int(dev.Fd()), unix.LOOP_CLR_FD, 0)
			_ = dev.Close()
			return nil, nil, fmt.Errorf("%s: %w", backing, ErrDirectIOUnavailable)
		}
		return d, dev, nil
	}
	return nil, nil, fmt.Errorf("no free loop device for %s after %d attempts: %w", backing, attachRetries, lastErr)
}

// OpenBacking opens an image for AttachFile. O_DIRECT on the backing file is
// how current kernels (and losetup --direct-io) request direct I/O;
// filesystems that reject O_DIRECT at open time get a buffered descriptor and
// rely on LO_FLAGS_DIRECT_IO alone, whose effect is verified after
// configuration. On IBM Storage Scale (GPFS), opening a loop backing file
// with O_DIRECT triggers a kernel panic in mmfs26, so dio=true is refused.
func OpenBacking(path string, readOnly, dio bool) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC
	if readOnly {
		flags = unix.O_RDONLY | unix.O_CLOEXEC
	}
	if dio {
		if isGPFS, err := isGPFSPath(path); err == nil && isGPFS {
			return nil, fmt.Errorf("%s: %w", path, ErrGPFSDirectIOUnsafe)
		}
		f, err := os.OpenFile(path, flags|unix.O_DIRECT, 0)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, unix.EINVAL) {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return f, nil
}

func isGPFSPath(path string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false, err
	}
	return int64(st.Type) == GPFSSuperMagic, nil //nolint:unconvert // Type width differs across Linux architectures.
}

func isGPFSFile(f *os.File) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &st); err != nil {
		return false, err
	}
	return int64(st.Type) == GPFSSuperMagic, nil //nolint:unconvert // Type width differs across Linux architectures.
}

func hasODirectFlag(f *os.File) (bool, error) {
	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return false, err
	}
	return flags&unix.O_DIRECT != 0, nil
}

func getFree() (int, error) {
	ctl, err := os.OpenFile(loopControl, os.O_RDWR, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", loopControl, err)
	}
	defer func() { _ = ctl.Close() }()
	n, err := unix.IoctlRetInt(int(ctl.Fd()), unix.LOOP_CTL_GET_FREE)
	if err != nil {
		return 0, fmt.Errorf("LOOP_CTL_GET_FREE: %w", err)
	}
	return n, nil
}

// openLoop opens /dev/loopN, creating the node when the plugin's /dev view
// lacks it (devtmpfs inside a container does not always follow the host).
func openLoop(num int, readOnly bool) (*os.File, error) {
	path := fmt.Sprintf("/dev/loop%d", num)
	flags := os.O_RDWR
	if readOnly {
		flags = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flags|unix.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		if mkErr := unix.Mknod(path, unix.S_IFBLK|0o660, int(unix.Mkdev(loopMajor, uint32(num)))); mkErr != nil && !errors.Is(mkErr, os.ErrExist) {
			return nil, fmt.Errorf("mknod %s: %w", path, mkErr)
		}
		f, err = os.OpenFile(path, flags|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return f, nil
}

func configure(dev, backing *os.File, opts Options, dio bool) error {
	flags := uint32(unix.LO_FLAGS_AUTOCLEAR)
	if opts.ReadOnly {
		flags |= unix.LO_FLAGS_READ_ONLY
	}
	if dio {
		flags |= unix.LO_FLAGS_DIRECT_IO
	}
	cfg := &unix.LoopConfig{
		Fd:   uint32(backing.Fd()),
		Size: opts.BlockSize,
	}
	cfg.Info.Flags = flags
	setFileName(&cfg.Info, backing.Name())

	err := unix.IoctlLoopConfigure(int(dev.Fd()), cfg)
	if err == nil || (!errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOTTY)) {
		return err
	}
	// Kernels before 5.8 have no LOOP_CONFIGURE.
	return configureLegacy(dev, backing, opts, dio)
}

func configureLegacy(dev, backing *os.File, opts Options, dio bool) error {
	fd := int(dev.Fd())
	if err := unix.IoctlSetInt(fd, unix.LOOP_SET_FD, int(backing.Fd())); err != nil {
		return fmt.Errorf("LOOP_SET_FD: %w", err)
	}
	fail := func(step string, err error) error {
		_ = unix.IoctlSetInt(fd, unix.LOOP_CLR_FD, 0)
		return fmt.Errorf("%s: %w", step, err)
	}
	info := &unix.LoopInfo64{Flags: unix.LO_FLAGS_AUTOCLEAR}
	setFileName(info, backing.Name())
	if err := unix.IoctlLoopSetStatus64(fd, info); err != nil {
		return fail("LOOP_SET_STATUS64", err)
	}
	if err := unix.IoctlSetInt(fd, unix.LOOP_SET_BLOCK_SIZE, int(opts.BlockSize)); err != nil {
		return fail("LOOP_SET_BLOCK_SIZE", err)
	}
	if dio {
		// EINVAL here means the kernel refused; the caller reads the flag
		// back and applies the DirectIO policy.
		if err := unix.IoctlSetInt(fd, unix.LOOP_SET_DIRECT_IO, 1); err != nil && !errors.Is(err, unix.EINVAL) {
			return fail("LOOP_SET_DIRECT_IO", err)
		}
	}
	return nil
}

func setFileName(info *unix.LoopInfo64, name string) {
	n := copy(info.File_name[:len(info.File_name)-1], name)
	info.File_name[n] = 0
}

// Detach releases a loop device. A device that is still mounted is only
// marked for autoclear by the kernel and goes away at unmount; a device that
// is already gone is not an error.
func Detach(devPath string) error {
	f, err := os.OpenFile(devPath, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", devPath, err)
	}
	defer func() { _ = f.Close() }()
	err = unix.IoctlSetInt(int(f.Fd()), unix.LOOP_CLR_FD, 0)
	if err != nil && !errors.Is(err, unix.ENXIO) {
		return fmt.Errorf("LOOP_CLR_FD %s: %w", devPath, err)
	}
	return nil
}

// SetCapacity makes the device pick up a grown backing file.
func SetCapacity(devPath string) error {
	f, err := os.OpenFile(devPath, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", devPath, err)
	}
	defer func() { _ = f.Close() }()
	if err := unix.IoctlSetInt(int(f.Fd()), unix.LOOP_SET_CAPACITY, 0); err != nil {
		return fmt.Errorf("LOOP_SET_CAPACITY %s: %w", devPath, err)
	}
	return nil
}

// Status reads the live state of devPath.
func Status(devPath string) (*Device, error) {
	f, err := os.OpenFile(devPath, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", devPath, err)
	}
	defer func() { _ = f.Close() }()
	info, err := unix.IoctlLoopGetStatus64(int(f.Fd()))
	if err != nil {
		return nil, fmt.Errorf("LOOP_GET_STATUS64 %s: %w", devPath, err)
	}
	return &Device{
		Path:        devPath,
		Number:      int(info.Number),
		DirectIO:    info.Flags&unix.LO_FLAGS_DIRECT_IO != 0,
		BackingFile: backingFileOf(int(info.Number)),
	}, nil
}

// FindByBackingFile lists the loop devices whose backing file is the same
// inode as path. Matching is by device and inode number, not by name, so it
// works across mount namespaces and bind mounts.
func FindByBackingFile(path string) ([]Device, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, fmt.Errorf("list /sys/block: %w", err)
	}
	var out []Device
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "loop") {
			continue
		}
		num, err := strconv.Atoi(strings.TrimPrefix(name, "loop"))
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join("/sys/block", name, "loop", "backing_file")); err != nil {
			continue // unbound
		}
		devPath := "/dev/" + name
		f, err := os.OpenFile(devPath, os.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		info, err := unix.IoctlLoopGetStatus64(int(f.Fd()))
		_ = f.Close()
		if err != nil {
			continue
		}
		if info.Device != st.Dev || info.Inode != st.Ino {
			continue
		}
		out = append(out, Device{
			Path:        devPath,
			Number:      num,
			DirectIO:    info.Flags&unix.LO_FLAGS_DIRECT_IO != 0,
			BackingFile: backingFileOf(num),
		})
	}
	return out, nil
}

func backingFileOf(num int) string {
	b, err := os.ReadFile(fmt.Sprintf("/sys/block/loop%d/loop/backing_file", num))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
