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
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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

func TestFreezeCommandPassesTheHelperTimeout(t *testing.T) {
	argv := freezeCommand(40 * time.Second)
	want := []string{selfExePath, "--freeze", "--freeze-timeout=40s"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Errorf("freezeCommand() = %v, want %v", argv, want)
	}
	if got := thawCommand(); strings.Join(got, " ") != selfExePath+" --thaw" {
		t.Errorf("thawCommand() = %v", got)
	}
}

func TestNewExecFreezerOutlivesTheWindow(t *testing.T) {
	f := NewExecFreezer(nil, 30*time.Second)
	if f.HelperTimeout <= 30*time.Second {
		t.Errorf("HelperTimeout = %s, want more than the 30s window so the controller releases first", f.HelperTimeout)
	}
}

func TestLineWatcherSignalsTheFrozenLine(t *testing.T) {
	w := newLineWatcher(frozenLine)
	// The line may arrive split across writes, after other output.
	for _, chunk := range []string{"devpilot-init: starting\nfro", "zen", "\nthawed\n"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-w.seen:
	default:
		t.Fatal("the frozen line was not detected")
	}
	if !strings.Contains(w.String(), "thawed") {
		t.Errorf("String() = %q, want the full tail", w.String())
	}
}

func TestLineWatcherIgnoresPartialMatches(t *testing.T) {
	w := newLineWatcher(frozenLine)
	_, _ = w.Write([]byte("not frozen\nfrozenish\nfrozen"))
	select {
	case <-w.seen:
		t.Fatal("a line that is not exactly \"frozen\" or lacks its newline was accepted")
	default:
	}
}

func TestExecFreezerRequiresConfig(t *testing.T) {
	var f *ExecFreezer
	if _, err := f.Freeze(context.Background(), &corev1.Pod{}); err == nil {
		t.Error("Freeze() without a rest config succeeded")
	}
	if err := (&ExecFreezer{}).Thaw(context.Background(), &corev1.Pod{}); err == nil {
		t.Error("Thaw() without a rest config succeeded")
	}
}
