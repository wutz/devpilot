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
	"fmt"
	"strings"
)

// Paths describes where the overlay layers live for one mount attempt.
type Paths struct {
	// Root is the root disk PVC mount, e.g. /mnt/.devpilot-rootdisk.
	Root string
	// Lower is the read-only bind mount of the container's initial /.
	Lower string
}

// OverlayOptions renders the data string passed to mount(2) for the overlay
// filesystem.
//
// metacopy=off is mandatory: with metacopy the upper layer can reference lower
// data for chmod/chown, and the unprivileged commit builder would then archive
// metadata-only stubs as zero-byte files.
//
// index=off is mandatory: index=on validates origin file handles, and the root
// disk is re-mounted on a different node whose containerd has unpacked the
// same image into different inodes, which fails with ESTALE.
func OverlayOptions(lower, upper, work string) string {
	opts := []string{
		"lowerdir=" + lower,
		"upperdir=" + upper,
		"workdir=" + work,
		"metacopy=off",
		"index=off",
	}
	return strings.Join(opts, ",")
}

// Options returns the mount data for the configured layout.
func (p Paths) Options() string {
	return OverlayOptions(p.Lower, p.Upper(), p.Work())
}

// Upper is the overlay upperdir path.
func (p Paths) Upper() string { return joinRoot(p.Root, UpperDir) }

// Work is the overlay workdir path.
func (p Paths) Work() string { return joinRoot(p.Root, WorkDir) }

// Merged is the overlay mount target and the future /.
func (p Paths) Merged() string { return joinRoot(p.Root, MergedDir) }

func joinRoot(root, name string) string { return strings.TrimSuffix(root, "/") + "/" + name }

// CheckOverlaySupport validates that the layers satisfy the assumptions the
// kernel makes before mount(2) is attempted, so failures carry a useful
// message instead of a bare EINVAL.
func CheckOverlaySupport(p Paths) error {
	if !strings.HasPrefix(p.Root, "/") {
		return fmt.Errorf("root disk path %q is not absolute", p.Root)
	}
	if p.Lower == "" {
		return fmt.Errorf("lowerdir is empty")
	}
	if p.Upper() == p.Work() {
		return fmt.Errorf("upperdir and workdir must differ")
	}
	if strings.ContainsAny(p.Root, ",\\") {
		return fmt.Errorf("root disk path %q contains characters overlayfs cannot parse", p.Root)
	}
	return nil
}
