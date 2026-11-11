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

package commit

import (
	"os"
	"syscall"
)

// osStat is os.Stat, kept behind a wrapper so the scanner's other logic stays
// portable.
func osStat(name string) (os.FileInfo, error) { return os.Stat(name) }

// readlink reads a symlink target without following it.
func readlink(name string) (string, error) { return os.Readlink(name) }

// openRegular opens a file for reading. The O_NOFOLLOW flag stops a symlink
// swapped in after the walk from redirecting the read outside the layer.
func openRegular(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NOATIME, 0)
}

// inodeOf returns the inode number, which identifies hardlinks.
func inodeOf(info os.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Ino, true
}
