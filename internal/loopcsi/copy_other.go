//go:build !linux

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

import "os"

func reflink(_, _ *os.File) error { return errReflinkUnsupported }

func sparseCopy(out, in *os.File) error {
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := out.Truncate(st.Size()); err != nil {
		return err
	}
	return copyRangeSkippingZeros(out, in, 0, st.Size(), make([]byte, copyChunk))
}
