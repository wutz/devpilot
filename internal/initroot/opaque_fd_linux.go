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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// OpaqueSnapshot is an open handle on upper/ plus the .meta directory used to
// publish the opaque directory index. Both directories are opened before
// pivot_root, after which the raw root disk is unreachable by path but the
// descriptors keep working.
type OpaqueSnapshot struct {
	UpperFD int
	MetaFD  int
}

// OpenOpaqueSnapshot opens upper/ and .meta relative to root.
func OpenOpaqueSnapshot(root string) (*OpaqueSnapshot, error) {
	upperFD, err := unix.Open(joinRoot(root, UpperDir), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open upper dir: %w", err)
	}
	metaFD, err := unix.Open(joinRoot(root, MetaDir), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Close(upperFD)
		return nil, fmt.Errorf("open meta dir: %w", err)
	}
	return &OpaqueSnapshot{UpperFD: upperFD, MetaFD: metaFD}, nil
}

// Close releases both descriptors.
func (s *OpaqueSnapshot) Close() error {
	return errors.Join(unix.Close(s.UpperFD), unix.Close(s.MetaFD))
}

// Refresh scans upper/ through the descriptor and rewrites
// .meta/opaque-dirs.json, so the unprivileged commit builder sees exactly the
// opaque directories the running overlay contains.
func (s *OpaqueSnapshot) Refresh() ([]string, error) {
	dirs, err := ScanOpaqueDirsAt(s.UpperFD, "")
	if err != nil {
		return nil, err
	}
	if err := WriteOpaqueIndexAt(s.MetaFD, dirs); err != nil {
		return nil, err
	}
	return dirs, nil
}

// ScanOpaqueDirsAt walks the directory tree below dirFD and returns the paths
// (relative, slash separated) of directories with overlay.opaque=y.
//
// It runs before pivot_root, while CAP_SYS_ADMIN still allows reading
// trusted.overlay.opaque; the commit job later runs without it.
func ScanOpaqueDirsAt(dirFD int, prefix string) ([]string, error) {
	names, err := readDirNames(dirFD)
	if err != nil {
		return nil, fmt.Errorf("read dir %q: %w", prefix, err)
	}
	slices.Sort(names)

	opaque := []string{}
	for _, name := range names {
		childFD, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			// Regular files, symlinks and unreadable directories have no
			// upper layer of their own to recurse into.
			continue
		}
		rel := name
		if prefix != "" {
			rel = prefix + "/" + name
		}
		if isOpaqueFD(childFD) {
			opaque = append(opaque, rel)
		}
		nested, err := ScanOpaqueDirsAt(childFD, rel)
		_ = unix.Close(childFD)
		if err != nil {
			return nil, err
		}
		opaque = append(opaque, nested...)
	}
	slices.Sort(opaque)
	return opaque, nil
}

func readDirNames(fd int) ([]string, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(dup), "upper")
	if dir == nil {
		_ = unix.Close(dup)
		return nil, errors.New("wrap directory descriptor")
	}
	defer dir.Close()

	names, err := dir.Readdirnames(-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return names, nil
}

func isOpaqueFD(fd int) bool {
	for _, attr := range []string{"trusted.overlay.opaque", "user.overlay.opaque"} {
		buf := make([]byte, 8)
		n, err := unix.Fgetxattr(fd, attr, buf)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(buf[:n])) == "y" {
			return true
		}
	}
	return false
}

// WriteGeneration records the generation checkpoint through the open .meta
// descriptor, since the raw root disk is unreachable after pivot_root.
func (s *OpaqueSnapshot) WriteGeneration(meta GenerationMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode generation: %w", err)
	}
	if err := writeFileAt(s.MetaFD, GenerationFile, append(data, '\n')); err != nil {
		return fmt.Errorf("write generation: %w", err)
	}
	return nil
}

// writeFileAt writes data to name inside dirFD via a staged rename.
func writeFileAt(dirFD int, name string, data []byte) error {
	tmpName := name + ".tmp"
	fd, err := unix.Openat(dirFD, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmpName, err)
	}
	if _, err := unix.Write(fd, data); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := unix.Fsync(fd); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := unix.Renameat(dirFD, tmpName, dirFD, name); err != nil {
		return fmt.Errorf("rename %s: %w", tmpName, err)
	}
	return nil
}

// WriteOpaqueIndexAt replaces opaque-dirs.json in the directory behind metaFD.
// The write is staged and renamed so a crash cannot leave a torn index.
func WriteOpaqueIndexAt(metaFD int, dirs []string) error {
	if dirs == nil {
		dirs = []string{}
	}
	data, err := json.MarshalIndent(dirs, "", "  ")
	if err != nil {
		return fmt.Errorf("encode opaque index: %w", err)
	}
	data = append(data, '\n')

	tmpName := OpaqueIndexFile + ".tmp"
	fd, err := unix.Openat(metaFD, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmpName, err)
	}
	if _, err := unix.Write(fd, data); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := unix.Fsync(fd); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := unix.Renameat(metaFD, tmpName, metaFD, OpaqueIndexFile); err != nil {
		return fmt.Errorf("rename %s: %w", tmpName, err)
	}
	return nil
}
