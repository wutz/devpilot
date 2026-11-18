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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrLocked means another open file description, on this node or another,
// holds a conflicting lock on the image.
var ErrLocked = errors.New("disk.img is locked by another user")

// Lease is the heartbeat file of a read-write mount. It is the second
// single-writer guard: it still works when the lock manager of the shared
// filesystem lost state (NFS server failover without lock reclaim, a pool
// mounted with nolock), and it tells operators which node has a volume.
type Lease struct {
	Node      string    `json:"node"`
	BootID    string    `json:"bootID,omitempty"`
	VolumeID  string    `json:"volumeID"`
	Device    string    `json:"device,omitempty"`
	Acquired  time.Time `json:"acquired"`
	Heartbeat time.Time `json:"heartbeat"`
}

// Fresh reports whether the lease holder heartbeated within ttl. Lease times
// come from the holder's clock, so nodes need NTP; the TTL is several
// heartbeats long to absorb skew.
func (l *Lease) Fresh(now time.Time, ttl time.Duration) bool {
	return now.Sub(l.Heartbeat) < ttl
}

// ForeignAndFresh reports whether the lease belongs to a live holder on
// another node. A lease written by this node, whether by an earlier plugin
// process or before a reboot, is never foreign: the OFD lock already proved
// nobody else holds the image.
func (l *Lease) ForeignAndFresh(node string, now time.Time, ttl time.Duration) bool {
	if l == nil || l.Node == node {
		return false
	}
	return l.Fresh(now, ttl)
}

// readLease returns nil without error when there is no lease.
func readLease(path string) (*Lease, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var l Lease
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("parse lease %s: %w", path, err)
	}
	return &l, nil
}

// writeLease replaces the lease atomically. rename(2) is atomic on NFS,
// Storage Scale and CephFS, so readers never see a torn lease.
func writeLease(path string, l *Lease) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// removeLease deletes the lease if node still owns it.
func removeLease(path, node string) error {
	l, err := readLease(path)
	if err != nil || l == nil || l.Node != node {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// readBootID identifies the current kernel boot, so a lease written by this
// node before a crash is recognised as stale.
func readBootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
