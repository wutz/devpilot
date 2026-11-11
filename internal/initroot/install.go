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
	"io"
	"os"
	"path/filepath"
)

// Install copies the running binary to dest with mode 0755.
//
// The operator runs devpilot-init in a tiny init container that copies the
// binary into an emptyDir shared with the main container, so the user's image
// needs neither a shell nor cp.
func Install(dest string) error {
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("install path %q must be absolute", dest)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate own binary: %w", err)
	}
	return copyExecutable(self, dest)
}

// copyExecutable copies src to dest and makes dest executable. It is not
// atomic on purpose: the emptyDir is created fresh for every pod.
func copyExecutable(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create install directory: %w", err)
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s to %s: %w", src, dest, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dest, err)
	}
	// OpenFile honours the umask, so restore the executable bits explicitly.
	if err := os.Chmod(dest, 0o755); err != nil {
		return fmt.Errorf("chmod %s: %w", dest, err)
	}
	return nil
}
