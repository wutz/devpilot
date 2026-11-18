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
	"os"

	"golang.org/x/sys/unix"
)

func reflink(out, in *os.File) error {
	err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOTTY), errors.Is(err, unix.EXDEV),
		errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
		return errReflinkUnsupported
	}
	return err
}

// sparseCopy copies only the data extents of in. Extents come from
// SEEK_DATA/SEEK_HOLE and are copied with copy_file_range, which NFS 4.2
// turns into a server-side COPY. Filesystems that implement SEEK_HOLE
// generically report one data extent covering the file; when that disagrees
// with the allocated block count the copy falls back to reading the file and
// skipping zero chunks, so a sparse 50 GiB image never becomes 50 GiB of
// written zeros.
func sparseCopy(out, in *os.File) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(in.Fd()), &st); err != nil {
		return err
	}
	size := st.Size
	if err := out.Truncate(size); err != nil {
		return err
	}
	buf := make([]byte, copyChunk)
	firstHole, err := unix.Seek(int(in.Fd()), 0, unix.SEEK_HOLE)
	holesReal := err == nil && (firstHole < size || st.Blocks*512 >= size*9/10)
	if !holesReal {
		return copyRangeSkippingZeros(out, in, 0, size, buf)
	}
	off := int64(0)
	for off < size {
		data, err := unix.Seek(int(in.Fd()), off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break // only a hole remains
		}
		if err != nil {
			return err
		}
		hole, err := unix.Seek(int(in.Fd()), data, unix.SEEK_HOLE)
		if err != nil {
			return err
		}
		if err := copyRange(out, in, data, hole, buf); err != nil {
			return err
		}
		off = hole
	}
	return nil
}

func copyRange(out, in *os.File, off, end int64, buf []byte) error {
	for off < end {
		roff, woff := off, off
		n, err := unix.CopyFileRange(int(in.Fd()), &roff, int(out.Fd()), &woff, int(end-off), 0)
		if err != nil {
			if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
				return copyRangeSkippingZeros(out, in, off, end, buf)
			}
			return err
		}
		if n == 0 {
			return copyRangeSkippingZeros(out, in, off, end, buf)
		}
		off += int64(n)
	}
	return nil
}
