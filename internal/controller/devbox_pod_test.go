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

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	testShell    = "/bin/bash"
	testSleep    = "sleep"
	testInfinity = "infinity"
	testPython   = "python"
	testNvidiaEP = "/opt/nvidia/nvidia_entrypoint.sh"
)

// A shell as the image command is the common case for dev images, and it exits
// at once with stdin on /dev/null: the pod then goes Finished and the DevBox is
// reported Failed. These cases pin the fallback that keeps it attachable.
func TestStartCommand(t *testing.T) {
	tests := []struct {
		name     string
		command  []string
		args     []string
		info     ImageInfo
		expected []string
	}{
		{
			name:     "image without a command sleeps",
			info:     ImageInfo{},
			expected: []string{testSleep, testInfinity},
		},
		{
			name:     "bare bash in the image command falls back to sleep",
			info:     ImageInfo{Entrypoint: []string{testNvidiaEP}, Cmd: []string{testShell}},
			expected: []string{testSleep, testInfinity},
		},
		{
			// nvcr.io/nvidia/pytorch:25.08-py3 is exactly this: the wrapper
			// exec's an empty argument list and the pod exits at once.
			name:     "an entrypoint without a CMD falls back to sleep",
			info:     ImageInfo{Entrypoint: []string{testNvidiaEP}},
			expected: []string{testSleep, testInfinity},
		},
		{
			name:     "absolute path shell is still bare",
			info:     ImageInfo{Cmd: []string{"/usr/bin/zsh"}},
			expected: []string{testSleep, testInfinity},
		},
		{
			name:     "shell with a script keeps its command",
			info:     ImageInfo{Cmd: []string{"sh", "-c", testPython + " train.py"}},
			expected: []string{"sh", "-c", testPython + " train.py"},
		},
		{
			name:     "a long running image command is inherited",
			info:     ImageInfo{Entrypoint: []string{"/entry.sh"}, Cmd: []string{"jupyter-lab", "--no-browser"}},
			expected: []string{"/entry.sh", "jupyter-lab", "--no-browser"},
		},
		{
			name:     "an explicit spec command wins over the image",
			command:  []string{testPython, "-m", "http.server"},
			args:     []string{"8080"},
			info:     ImageInfo{Cmd: []string{testShell}},
			expected: []string{testPython, "-m", "http.server", "8080"},
		},
		{
			name:     "an explicit shell command is not rewritten",
			command:  []string{testShell},
			info:     ImageInfo{},
			expected: []string{testShell},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			box := &devpilotv1alpha1.DevBox{}
			box.Spec.Command = tc.command
			box.Spec.Args = tc.args
			got := startCommand(box, tc.info)
			if len(got) != len(tc.expected) {
				t.Fatalf("startCommand() = %q, want %q", got, tc.expected)
			}
			for i := range got {
				if got[i] != tc.expected[i] {
					t.Fatalf("startCommand() = %q, want %q", got, tc.expected)
				}
			}
		})
	}
}
