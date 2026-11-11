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
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Config carries the operator-wide knobs that are not derived from a DevBox.
// It is populated from manager flags so the same binary can serve clusters
// with different storage backends and image registries.
type Config struct {
	// InitImage is the fully qualified reference of the devpilot-init helper
	// binary image, injected as an init container into every DevBox pod.
	InitImage string
	// CommitImage is the fully qualified reference of the devpilot-commit image
	// that DevBoxCommit build Jobs run. Empty disables commits.
	CommitImage string
	// RegistrySecret names a docker config secret in the commit namespace. When
	// set, a build Job mounts it and pushes with those credentials.
	RegistrySecret string
	// DefaultSnapshotClasses maps a root disk StorageClass to the
	// VolumeSnapshotClass used for online commits.
	DefaultSnapshotClasses map[string]string
}

// ParseSnapshotClassMapping parses "storageClass=snapshotClass,..." into a map.
// An empty input yields an empty, non-nil map so callers never have to nil-check.
func ParseSnapshotClassMapping(raw string) (map[string]string, error) {
	out := map[string]string{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out, nil
	}
	for pair := range strings.SplitSeq(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, found := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !found || key == "" || value == "" {
			return nil, fmt.Errorf("invalid snapshot class mapping %q: want <storageClass>=<snapshotClass>", pair)
		}
		if existing, ok := out[key]; ok && existing != value {
			return nil, fmt.Errorf("conflicting snapshot class mappings for storage class %q: %q and %q", key, existing, value)
		}
		out[key] = value
	}
	return out, nil
}

// SnapshotClass returns the VolumeSnapshotClass configured for storageClass.
func (c Config) SnapshotClass(storageClass string) (string, error) {
	if storageClass == "" {
		return "", errors.New("storage class is empty")
	}
	class, ok := c.DefaultSnapshotClasses[storageClass]
	if !ok {
		return "", fmt.Errorf("no snapshot class configured for storage class %q", storageClass)
	}
	return class, nil
}

// SnapshotClassMapping renders the map back into the canonical flag format,
// sorted by storage class so the output is stable across restarts.
func (c Config) SnapshotClassMapping() string {
	keys := make([]string, 0, len(c.DefaultSnapshotClasses))
	for k := range c.DefaultSnapshotClasses {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+c.DefaultSnapshotClasses[k])
	}
	return strings.Join(pairs, ",")
}

// Validate reports configuration problems the manager must not start with.
func (c Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.InitImage) == "" {
		errs = append(errs, errors.New("devpilot init image must be set"))
	}
	return errors.Join(errs...)
}
