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
	"bufio"
	"io"
	"strconv"
	"strings"
)

// parseMountInfo returns the state of the last (topmost) mount at path.
func parseMountInfo(r io.Reader, path string) (MountState, error) {
	var st MountState
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 {
			continue
		}
		if unescapeMountPath(fields[4]) != path {
			continue
		}
		st.Mounted = true
		st.ReadOnly = false
		for o := range strings.SplitSeq(fields[5], ",") {
			if o == "ro" {
				st.ReadOnly = true
			}
		}
		// Super options (after the " - " separator) carry the ro an ext4
		// errors=remount-ro switched to.
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" && i+3 < len(fields) {
				for o := range strings.SplitSeq(fields[i+3], ",") {
					if o == "ro" {
						st.ReadOnly = true
					}
				}
				break
			}
		}
	}
	return st, sc.Err()
}

func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
