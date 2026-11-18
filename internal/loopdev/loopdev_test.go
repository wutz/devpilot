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
	"testing"
)

func TestValidateGPFSDirectIOMode(t *testing.T) {
	tests := []struct {
		name       string
		hasODirect bool
		mode       DirectIOMode
		wantErr    []error
	}{
		{
			name:       "O_DIRECT fd is always refused even when directIO=off",
			hasODirect: true,
			mode:       DirectIOOff,
			wantErr:    []error{ErrGPFSDirectIOUnsafe},
		},
		{
			name:       "O_DIRECT fd is refused when directIO=preferred",
			hasODirect: true,
			mode:       DirectIOPreferred,
			wantErr:    []error{ErrGPFSDirectIOUnsafe},
		},
		{
			name:    "required is refused",
			mode:    DirectIORequired,
			wantErr: []error{ErrDirectIOUnavailable, ErrGPFSDirectIOUnsafe},
		},
		{
			name:    "empty defaults to required and is refused",
			mode:    "",
			wantErr: []error{ErrDirectIOUnavailable, ErrGPFSDirectIOUnsafe},
		},
		{
			name: "preferred is allowed buffered",
			mode: DirectIOPreferred,
		},
		{
			name: "off stays buffered",
			mode: DirectIOOff,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGPFSDirectIOMode(tt.hasODirect, tt.mode)
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error matching %v, got nil", tt.wantErr)
			}
			for _, target := range tt.wantErr {
				if !errors.Is(err, target) {
					t.Fatalf("error %q does not match %v", err, target)
				}
			}
		})
	}
}
