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

package loopdev

import (
	"errors"
	"os"
)

// ErrUnsupported is returned on platforms without loop devices.
var ErrUnsupported = errors.New("loop devices are only supported on Linux")

// Attach is not supported outside Linux.
func Attach(string, Options) (*Device, *os.File, error) { return nil, nil, ErrUnsupported }

// AttachFile is not supported outside Linux.
func AttachFile(*os.File, Options) (*Device, *os.File, error) { return nil, nil, ErrUnsupported }

// OpenBacking opens path without O_DIRECT outside Linux so callers and tests
// can exercise locking.
func OpenBacking(path string, readOnly, _ bool) (*os.File, error) {
	if readOnly {
		return os.Open(path)
	}
	return os.OpenFile(path, os.O_RDWR, 0)
}

// Detach is not supported outside Linux.
func Detach(string) error { return ErrUnsupported }

// SetCapacity is not supported outside Linux.
func SetCapacity(string) error { return ErrUnsupported }

// Status is not supported outside Linux.
func Status(string) (*Device, error) { return nil, ErrUnsupported }

// FindByBackingFile is not supported outside Linux.
func FindByBackingFile(string) ([]Device, error) { return nil, ErrUnsupported }

// Thaw is not supported outside Linux.
func Thaw(string) error { return ErrUnsupported }

// Trim is not supported outside Linux.
func Trim(string, uint64) (uint64, error) { return 0, ErrUnsupported }

// Freeze is not supported outside Linux.
func Freeze(string) error { return ErrUnsupported }
