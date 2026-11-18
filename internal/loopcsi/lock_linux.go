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

	"golang.org/x/sys/unix"
)

// lockFile takes a whole-file open file description lock without blocking.
//
// OFD locks belong to the open file description, not to the process: they
// survive the plugin closing its descriptor as long as another reference to
// the description exists (the loop device holds one), and two descriptions
// in the same process conflict, which is what lets tests stand in for two
// nodes. NFSv4, Storage Scale and CephFS all forward fcntl locks to their
// cluster-wide lock managers.
func lockFile(f *os.File, exclusive bool) error {
	typ := int16(unix.F_RDLCK)
	if exclusive {
		typ = unix.F_WRLCK
	}
	lk := unix.Flock_t{Type: typ, Whence: io.SeekStart}
	err := unix.FcntlFlock(f.Fd(), unix.F_OFD_SETLK, &lk)
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) {
		return ErrLocked
	}
	if err != nil {
		return fmt.Errorf("F_OFD_SETLK %s: %w", f.Name(), err)
	}
	return nil
}

// probeLock reports whether any other open file description holds a lock on
// path, and whether that lock is exclusive.
func probeLock(path string) (held, exclusive bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = f.Close() }()
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: io.SeekStart}
	if err := unix.FcntlFlock(f.Fd(), unix.F_OFD_GETLK, &lk); err != nil {
		return false, false, fmt.Errorf("F_OFD_GETLK %s: %w", path, err)
	}
	if lk.Type == unix.F_UNLCK {
		return false, false, nil
	}
	return true, lk.Type == unix.F_WRLCK, nil
}
