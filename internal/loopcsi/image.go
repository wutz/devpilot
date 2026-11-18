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
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

// Runner executes e2fsprogs. Tests replace it.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner runs commands from PATH.
type ExecRunner struct{}

// Run executes name and returns its combined output.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %v: %w: %s", name, args, err, bytes.TrimSpace(out))
	}
	return out, nil
}

// ext4 superblock fields used by the driver (fs/ext4/ext4.h).
const (
	sbOffset         = 1024
	sbSize           = 1024
	sbMagic          = 0xEF53
	sbStateErrors    = 0x0002
	incompatRecover  = 0x0004
	incompat64Bit    = 0x0080
	incompatMMP      = 0x0100
	offBlocksCountLo = 0x04
	offLogBlockSize  = 0x18
	offMagic         = 0x38
	offState         = 0x3A
	offIncompat      = 0x60
	offBlocksCountHi = 0x150
)

// Superblock is the part of an ext4 superblock the driver acts on.
type Superblock struct {
	BlockSize   uint64
	BlocksCount uint64
	// Errors is set when the kernel recorded a filesystem error; the image
	// gets e2fsck -p before its next mount.
	Errors bool
	// NeedsRecovery is set while the journal has unreplayed transactions.
	NeedsRecovery bool
	MMP           bool
}

// Size is the filesystem size in bytes.
func (s Superblock) Size() int64 { return int64(s.BlockSize * s.BlocksCount) }

// errNoExt4 means the file does not start with an ext4 superblock.
var errNoExt4 = errors.New("no ext4 superblock")

// readSuperblock parses the primary superblock of an image or device.
func readSuperblock(path string) (Superblock, error) {
	f, err := os.Open(path)
	if err != nil {
		return Superblock{}, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, sbSize)
	if _, err := f.ReadAt(buf, sbOffset); err != nil {
		if errors.Is(err, io.EOF) {
			return Superblock{}, errNoExt4
		}
		return Superblock{}, fmt.Errorf("read superblock of %s: %w", path, err)
	}
	return parseSuperblock(buf)
}

func parseSuperblock(buf []byte) (Superblock, error) {
	le := binary.LittleEndian
	if le.Uint16(buf[offMagic:]) != sbMagic {
		return Superblock{}, errNoExt4
	}
	logBS := le.Uint32(buf[offLogBlockSize:])
	if logBS > 6 {
		return Superblock{}, fmt.Errorf("ext4 block size 1024<<%d is out of range", logBS)
	}
	incompat := le.Uint32(buf[offIncompat:])
	count := uint64(le.Uint32(buf[offBlocksCountLo:]))
	if incompat&incompat64Bit != 0 {
		count |= uint64(le.Uint32(buf[offBlocksCountHi:])) << 32
	}
	return Superblock{
		BlockSize:     1024 << logBS,
		BlocksCount:   count,
		Errors:        le.Uint16(buf[offState:])&sbStateErrors != 0,
		NeedsRecovery: incompat&incompatRecover != 0,
		MMP:           incompat&incompatMMP != 0,
	}, nil
}

// mmpUpdateInterval is how often the kernel rewrites the MMP block. A mount
// waits about twice this long to make sure no other node is updating it.
const mmpUpdateInterval = 5

// createImage makes a sparse ext4 image of size bytes at path. It is
// idempotent: an existing image is kept (and grown if it is smaller). The
// image is built under a temporary name and renamed into place, so a crash
// never leaves a half-formatted disk.img that a retry would accept.
//
// Inode tables and the journal are left lazily initialised: the file is all
// holes, which read as zeros, so the zeroing the kernel would otherwise do
// after the first mount is redundant and is disabled with noinit_itable at
// mount time.
func createImage(ctx context.Context, run Runner, path string, size int64, mmp bool) error {
	if _, err := readSuperblock(path); err == nil {
		return growImage(path, size)
	} else if !errors.Is(err, errNoExt4) && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := path + ".creating"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return fmt.Errorf("truncate %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	features := "^orphan_file"
	ext := "lazy_itable_init=1,lazy_journal_init=1,nodiscard"
	if mmp {
		features += ",mmp"
		ext += ",mmp_update_interval=" + strconv.Itoa(mmpUpdateInterval)
	}
	if _, err := run.Run(ctx, "mkfs.ext4", "-F", "-q", "-b", "4096", "-m", "0", "-O", features, "-E", ext, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := syncPath(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// growImage extends an image file to at least size. The filesystem inside
// is grown by the node plugin (resize2fs) on the next publish or expand.
func growImage(path string, size int64) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Size() >= size {
		return nil
	}
	if err := os.Truncate(path, size); err != nil {
		return fmt.Errorf("grow %s to %d: %w", path, size, err)
	}
	return nil
}

func syncPath(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

// CopyMethod reports how copyImage produced the destination.
type CopyMethod string

const (
	// CopyReflink shared extents with FICLONE.
	CopyReflink CopyMethod = "reflink"
	// CopySparse copied data extents only.
	CopySparse CopyMethod = "copy"
)

// errReflinkUnsupported means the filesystem has no FICLONE between the two
// files.
var errReflinkUnsupported = errors.New("reflink is not supported by this filesystem")

// copyImage copies src to dst through a temporary file that is renamed into
// place, so dst either does not exist or is complete.
func copyImage(src, dst string, strategy SnapshotStrategy) (CopyMethod, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".copying"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	fail := func(err error) (CopyMethod, error) {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	method := CopySparse
	done := false
	if strategy == StrategyAuto || strategy == StrategyReflink {
		err := reflink(out, in)
		switch {
		case err == nil:
			method, done = CopyReflink, true
		case errors.Is(err, errReflinkUnsupported) && strategy == StrategyAuto:
		default:
			return fail(fmt.Errorf("reflink %s: %w", src, err))
		}
	}
	if !done {
		if err := sparseCopy(out, in); err != nil {
			return fail(fmt.Errorf("copy %s: %w", src, err))
		}
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return method, nil
}

const copyChunk = 1 << 20

// copyRangeSkippingZeros copies [off, end) from in to out with pread/pwrite,
// leaving all-zero chunks as holes in out. out must already have its final
// size so skipped chunks read back as zeros.
func copyRangeSkippingZeros(out, in *os.File, off, end int64, buf []byte) error {
	for off < end {
		n := min(int64(len(buf)), end-off)
		chunk := buf[:n]
		if _, err := in.ReadAt(chunk, off); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if !isZero(chunk) {
			if _, err := out.WriteAt(chunk, off); err != nil {
				return err
			}
		}
		off += n
	}
	return nil
}

func isZero(b []byte) bool {
	for len(b) >= 8 {
		if binary.LittleEndian.Uint64(b) != 0 {
			return false
		}
		b = b[8:]
	}
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
