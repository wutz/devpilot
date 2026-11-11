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
	"bufio"
	"fmt"
	"io"
	"slices"
	"strings"
)

// MountEntry is one line of /proc/self/mountinfo.
// See proc(5), "Mount information".
type MountEntry struct {
	MountID    int
	ParentID   int
	MajorMinor string
	Root       string
	Target     string
	Options    string
	Optional   string
	FSType     string
	Source     string
	SuperOpts  string
}

// ParseMountInfo parses the proc(5) mountinfo format. Namespaced paths are
// emitted with octal escapes (e.g. "\040" for a space) and are decoded here.
func ParseMountInfo(r io.Reader) ([]MountEntry, error) {
	var entries []MountEntry
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		entry, err := parseMountInfoLine(line)
		if err != nil {
			return nil, fmt.Errorf("mountinfo line %d: %w", lineNo, err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read mountinfo: %w", err)
	}
	return entries, nil
}

func parseMountInfoLine(line string) (MountEntry, error) {
	fields := strings.Fields(line)
	sep := slices.Index(fields, "-")
	if sep < 0 {
		return MountEntry{}, fmt.Errorf("missing optional field separator: %q", line)
	}
	if sep < 6 || len(fields) < sep+2 {
		return MountEntry{}, fmt.Errorf("unexpected field count: %q", line)
	}

	entry := MountEntry{
		Options:    fields[5],
		FSType:     fields[sep+1],
		MountID:    atoi(fields[0]),
		ParentID:   atoi(fields[1]),
		MajorMinor: fields[2],
		Root:       unescapeMountPath(fields[3]),
		Target:     unescapeMountPath(fields[4]),
		Optional:   "",
		Source:     "",
		SuperOpts:  "",
	}
	if sep > 6 {
		entry.Optional = strings.Join(fields[6:sep], " ")
	}
	if len(fields) > sep+2 {
		entry.Source = fields[sep+2]
	}
	if len(fields) > sep+3 {
		entry.SuperOpts = fields[sep+3]
	}
	return entry, nil
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// unescapeMountPath decodes the octal escapes the kernel uses in mountinfo.
// Only the four escapes defined by proc(5) exist.
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			v := int(s[i+1]-'0')<<6 | int(s[i+2]-'0')<<3 | int(s[i+3]-'0')
			b.WriteByte(byte(v))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// SubmountsUnderRoot returns the mounts that must be re-bound into the new
// root before pivot_root, sorted so that parents are always migrated before
// their children. Mounts at or below excluded prefixes are dropped; the root
// mount itself and anything outside root are ignored.
func SubmountsUnderRoot(entries []MountEntry, excluded ...string) []MountEntry {
	out := make([]MountEntry, 0, len(entries))
	for _, e := range entries {
		if e.Target == "/" || !strings.HasPrefix(e.Target, "/") {
			continue
		}
		if isPathUnder(e.Target, excluded...) {
			continue
		}
		out = append(out, e)
	}
	slices.SortStableFunc(out, func(a, b MountEntry) int {
		if da, db := pathDepth(a.Target), pathDepth(b.Target); da != db {
			return da - db
		}
		return strings.Compare(a.Target, b.Target)
	})
	return out
}

// isPathUnder reports whether p is one of prefixes or lives below one.
func isPathUnder(p string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		prefix = strings.TrimSuffix(prefix, "/")
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// pathDepth counts the components of an absolute path: "/" is 0, "/dev" is 1.
func pathDepth(p string) int {
	p = strings.Trim(p, "/")
	if p == "" {
		return 0
	}
	return strings.Count(p, "/") + 1
}
