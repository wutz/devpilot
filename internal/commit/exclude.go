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

package commit

import (
	"fmt"
	"path"
	"strings"
)

// defaultExcludeRules carry runtime state and platform injected secrets that
// must never be frozen into an image (docs/persistence-and-commit.md §3.4).
//
// The paths are relative to the layer root, which is the container root after
// pivot_root, so "etc/hosts" is /etc/hosts inside the devbox.
var defaultExcludeRules = []string{
	// runtime and pseudo file systems
	"tmp/**", "var/tmp/**", "run/**", "var/run/**", "proc/**", "sys/**", "dev/**",
	".old_root/**",

	// files the kubelet and the container runtime inject per pod. A 0 byte
	// nvidia-smi or libcuda.so placeholder left behind by the NVIDIA toolkit
	// would otherwise shadow the real driver mount in the new image.
	"etc/hosts", "etc/hostname", "etc/resolv.conf",

	// platform injected credentials and per-instance state
	"etc/ssh/ssh_host_*_key", "etc/ssh/ssh_host_*_key.pub",
	"run/secrets/**", "var/run/secrets/**",
	"root/.local/share/jupyter/runtime/**",
}

// packageCacheRules are dropped when CleanPackageCache is set. They are large,
// cheap to recreate, and never part of a reproducible environment.
var packageCacheRules = []string{
	"var/cache/apt/archives/**",
	"root/.cache/pip/**",
	"home/*/.cache/pip/**",
	"root/.cache/uv/**",
	"root/.npm/_cacache/**",
}

// excludeRule is one compiled glob. A rule that ends in "/**" matches the
// directory itself and everything below it, so skipping a directory skips its
// whole subtree.
type excludeRule struct {
	pattern  string
	subtree  bool
	matched  func(string) bool
	original string
}

// compileExcludes merges the defaults, the optional cache rules and the
// caller's globs. A bad pattern is a user error worth reporting, not something
// to skip silently.
func compileExcludes(extra []string, cleanPackageCache bool) ([]excludeRule, error) {
	patterns := make([]string, 0, len(defaultExcludeRules)+len(packageCacheRules)+len(extra))
	patterns = append(patterns, defaultExcludeRules...)
	if cleanPackageCache {
		patterns = append(patterns, packageCacheRules...)
	}
	patterns = append(patterns, extra...)

	rules := make([]excludeRule, 0, len(patterns))
	for _, raw := range patterns {
		pattern := strings.TrimPrefix(strings.TrimSpace(raw), "/")
		if pattern == "" {
			continue
		}
		rule := excludeRule{pattern: pattern, original: raw}
		if base, ok := strings.CutSuffix(pattern, "/**"); ok {
			rule.subtree = true
			rule.matched = func(p string) bool { return p == base || strings.HasPrefix(p, base+"/") }
		} else {
			if _, err := path.Match(pattern, "probe"); err != nil {
				return nil, fmt.Errorf("invalid exclude pattern %q: %w", raw, err)
			}
			rule.matched = func(p string) bool {
				if ok, _ := path.Match(pattern, p); ok {
					return true
				}
				// "dir/*" should also hide dir itself so an empty directory is
				// not created just to hold nothing.
				if base, ok := strings.CutSuffix(pattern, "/*"); ok && p == base {
					return true
				}
				return false
			}
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// excludesMatched reports whether a layer relative path is excluded.
func excludesMatched(rules []excludeRule, rel string) bool {
	for _, rule := range rules {
		if rule.matched(rel) {
			return true
		}
	}
	return false
}

// ExcludePatterns returns the default patterns, for the CLI's --help output and
// for the reconciler's status messages.
func ExcludePatterns() []string {
	out := make([]string, len(defaultExcludeRules))
	copy(out, defaultExcludeRules)
	return out
}
