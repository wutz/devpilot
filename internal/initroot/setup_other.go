//go:build !linux

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
)

// BaseLower is where the container's initial / is pinned read-only.
const BaseLower = "/run/devpilot/base-lower"

// ErrUnsupported is returned by the kernel-facing helpers on non-Linux hosts.
// They exist only so the package builds and its pure logic can be tested
// during development; devpilot-init itself only ever runs inside a container.
var ErrUnsupported = errors.New("initroot: only supported on linux")

func lgetxattr(string, string) ([]byte, error) { return nil, ErrUnsupported }

// Setup assembles the overlay root and pivots into it (Linux only).
func Setup(Paths) error { return ErrUnsupported }

// DropCapabilities removes the mount-related capabilities (Linux only).
func DropCapabilities() error { return ErrUnsupported }

// Sync flushes dirty pages to the root disk (Linux only).
func Sync() error { return ErrUnsupported }

// OpaqueSnapshot is a handle on upper/ in the real (Linux) implementation.
type OpaqueSnapshot struct{}

// OpenOpaqueSnapshot is only meaningful on Linux.
func OpenOpaqueSnapshot(string) (*OpaqueSnapshot, error) { return nil, ErrUnsupported }

// Refresh is only meaningful on Linux.
func (*OpaqueSnapshot) Refresh() ([]string, error) { return nil, ErrUnsupported }

// WriteGeneration is only meaningful on Linux.
func (*OpaqueSnapshot) WriteGeneration(GenerationMeta) error { return ErrUnsupported }

// Close releases nothing outside Linux.
func (*OpaqueSnapshot) Close() error { return nil }
