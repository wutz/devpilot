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
	"io"
	"os"

	"github.com/wutz/devpilot/internal/loopdev"
)

// Usage is filesystem usage for NodeGetVolumeStats.
type Usage struct {
	TotalBytes, AvailableBytes, UsedBytes int64
	TotalInodes, FreeInodes, UsedInodes   int64
}

// MountState describes a path in the mount table.
type MountState struct {
	Mounted  bool
	ReadOnly bool
}

// Host is everything the node plugin does to the kernel. The Linux
// implementation is LinuxHost; tests substitute a fake.
type Host interface {
	OpenBacking(path string, readOnly, directIO bool) (*os.File, error)
	// Attach binds an open image to a loop device and returns the device
	// together with an open handle that keeps it bound until closed.
	Attach(f *os.File, opts loopdev.Options) (*loopdev.Device, io.Closer, error)
	FindLoops(image string) ([]loopdev.Device, error)
	Detach(dev string) error
	SetCapacity(dev string) error
	MountExt4(dev, target string, options []string) error
	BindMount(source, target string, readOnly bool) error
	Unmount(target string) error
	MountState(path string) (MountState, error)
	Thaw(path string) error
	Trim(path string, minLen uint64) (uint64, error)
	Statfs(path string) (Usage, error)
}
