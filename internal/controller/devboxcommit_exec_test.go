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

package controller

import (
	"testing"
)

// The flush runs inside the container's own mount namespace, after devpilot-init
// replaced / with the overlay. The emptyDir that holds InitBinaryPath is not
// part of that overlay, so a flush command that names it fails with
// "no such file or directory" on every online commit.
func TestFlushCommandUsesTheRunningHelper(t *testing.T) {
	argv := flushCommand()
	if len(argv) != 2 {
		t.Fatalf("flushCommand() = %v, want a path and --flush", argv)
	}
	if argv[0] != selfExePath {
		t.Errorf("flushCommand()[0] = %q, want %q", argv[0], selfExePath)
	}
	if argv[0] == InitBinaryPath {
		t.Errorf("flushCommand()[0] = %q, but that path is gone after pivot_root", argv[0])
	}
	if argv[1] != "--flush" {
		t.Errorf("flushCommand()[1] = %q, want --flush", argv[1])
	}
}
