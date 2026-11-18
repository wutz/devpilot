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

// Package loopdev attaches image files to Linux loop block devices.
//
// It exists for the shared-storage root disk backend: every DevBox root disk
// is an ext4 image (disk.img) on GPFS, VAST NFS or CephFS, and the loop CSI
// node plugin exposes it as a block device the kernel can mount. Two loop
// properties matter for that use and are therefore enforced rather than
// requested:
//
//   - Direct I/O on the backing file. Buffered loop I/O over a network
//     filesystem caches every page twice and can deadlock memory reclaim, and
//     buffered loop I/O on IBM Storage Scale is subject to APAR IJ60020 on
//     kernels with "loop: stop using vfs_iter_{read,write} for buffered I/O".
//     The kernel silently drops LO_FLAGS_DIRECT_IO when the logical block
//     size is smaller than the backing file's DIO alignment, so the flag is
//     read back after the device is configured.
//   - A logical block size equal to the filesystem block size (4096), which
//     is what makes direct I/O possible in the first place.
package loopdev

import (
	"errors"
	"fmt"
)

// DefaultBlockSize is the logical block size of every attached device. It
// matches mkfs.ext4 -b 4096 and the DIO alignment of the supported backends.
const DefaultBlockSize = 4096

// GPFSSuperMagic is the Linux statfs filesystem type magic for IBM Storage
// Scale (GPFS).
const GPFSSuperMagic = 0x47504653

// DirectIOMode says how strictly direct I/O is required.
type DirectIOMode string

const (
	// DirectIORequired detaches the device and fails when the kernel did not
	// enable direct I/O. This is the default.
	DirectIORequired DirectIOMode = "required"
	// DirectIOPreferred keeps a buffered device and reports it.
	DirectIOPreferred DirectIOMode = "preferred"
	// DirectIOOff never asks for direct I/O. Only for local test backends
	// or filesystems where kernel loop direct I/O is unsafe.
	DirectIOOff DirectIOMode = "off"
)

// ParseDirectIOMode validates a StorageClass parameter value.
func ParseDirectIOMode(raw string) (DirectIOMode, error) {
	switch DirectIOMode(raw) {
	case "", DirectIORequired:
		return DirectIORequired, nil
	case DirectIOPreferred, DirectIOOff:
		return DirectIOMode(raw), nil
	}
	return "", fmt.Errorf("directIO %q must be %q, %q or %q", raw, DirectIORequired, DirectIOPreferred, DirectIOOff)
}

// Options configures one attachment.
type Options struct {
	// BlockSize is the logical block size; 0 means DefaultBlockSize.
	BlockSize uint32
	// DirectIO selects the direct I/O policy; empty means DirectIORequired.
	DirectIO DirectIOMode
	// ReadOnly attaches the device read-only.
	ReadOnly bool
}

// Device is one attached loop device.
type Device struct {
	// Path is the device node, e.g. /dev/loop7.
	Path string
	// Number is the loop index.
	Number int
	// DirectIO reports whether the kernel actually enabled direct I/O.
	DirectIO bool
	// BackingFile is the image the device reads.
	BackingFile string
}

// ErrDirectIOUnavailable is returned when direct I/O is required but the
// kernel kept the device buffered.
var ErrDirectIOUnavailable = errors.New("the kernel did not enable direct I/O on the loop device")

// ErrGPFSDirectIOUnsafe is returned when direct I/O is requested on an IBM
// Storage Scale (GPFS) backing file. Linux loop kworker threads issue async DIO
// with current->mm == NULL, triggering a kernel NULL pointer dereference in
// mmfs26 (dioRdWr) when O_DIRECT is enabled.
var ErrGPFSDirectIOUnsafe = errors.New("direct I/O on a loop device over GPFS is unsafe (triggers mmfs26 dioRdWr NULL dereference)")

// validateGPFSDirectIOMode enforces the GPFS safety invariant: never attach a
// loop device with O_DIRECT or LO_FLAGS_DIRECT_IO on a GPFS backing file.
func validateGPFSDirectIOMode(hasODirect bool, mode DirectIOMode) error {
	if hasODirect {
		return ErrGPFSDirectIOUnsafe
	}
	switch mode {
	case DirectIOPreferred, DirectIOOff:
		return nil
	default:
		return fmt.Errorf("%w: %w", ErrDirectIOUnavailable, ErrGPFSDirectIOUnsafe)
	}
}
