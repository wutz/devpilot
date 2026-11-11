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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultRootDisk is where the kubelet mounts the root disk PVC in the
// devpilot-init container.
const DefaultRootDisk = "/mnt/.devpilot-rootdisk"

// DefaultCommand keeps the container alive when the image has no long running
// entrypoint of its own.
var DefaultCommand = []string{"sleep", "infinity"}

// OpaqueFlusher publishes overlay metadata the unprivileged commit builder
// cannot read itself. The descriptors it holds stay valid after pivot_root,
// which is why it is opened before the root swap.
type OpaqueFlusher interface {
	Refresh() ([]string, error)
	WriteGeneration(GenerationMeta) error
	Close() error
}

// BootOptions is the fully resolved configuration for one container start.
type BootOptions struct {
	// Root is the root disk mount.
	Root string
	// ImageRef and ImageDigest identify the base image the pod was started
	// from; they are pinned into .meta/base-image.json.
	ImageRef    string
	ImageDigest string
	// Lower is where the initial / is bound read-only.
	Lower string
	// GracePeriod bounds the shutdown wait for the managed command.
	GracePeriod time.Duration
	// Command is the user process to supervise.
	Command Command
	// Logf receives lifecycle messages; nil discards them.
	Logf LogFunc

	// SetupFn, Starter and OpenSnapshot are injectable for tests.
	SetupFn      func(Paths) error
	Starter      Starter
	OpenSnapshot func(string) (OpaqueFlusher, error)
}

func (o BootOptions) logf(msg string, kv ...any) {
	if o.Logf != nil {
		o.Logf(msg, kv...)
	}
}

// Boot runs the init sequence: pin the base image, assemble the overlay,
// pivot into it, supervise the user command and checkpoint on exit.
//
// It returns the exit code of the managed command.
func Boot(opts BootOptions) (int, error) {
	if err := validateBootOptions(opts); err != nil {
		return 1, err
	}
	if opts.SetupFn == nil {
		opts.SetupFn = Setup
	}
	if opts.Starter == nil {
		opts.Starter = DefaultStarter
	}
	if opts.OpenSnapshot == nil {
		opts.OpenSnapshot = func(root string) (OpaqueFlusher, error) {
			return OpenOpaqueSnapshot(root)
		}
	}
	if opts.Command.Env == nil {
		opts.Command.Env = os.Environ()
	}

	// The digest invariant is checked before anything is mounted: if this pod
	// started from a different base image than the one upper/ was created on,
	// mounting would corrupt every hardlink and .so path inside upper/.
	meta, err := PinBaseImage(opts.Root, opts.ImageRef, opts.ImageDigest)
	if err != nil {
		return 1, err
	}
	if err := WriteBaseImageMeta(opts.Root, meta); err != nil {
		return 1, fmt.Errorf("persist base image meta: %w", err)
	}
	opts.logf("Pinned base image", "imageRef", meta.ImageRef, "imageDigest", meta.ImageDigest, "rebaseCount", meta.RebaseCount)

	generation, err := nextGeneration(opts.Root)
	if err != nil {
		return 1, err
	}

	snapshot, err := opts.OpenSnapshot(opts.Root)
	if err != nil {
		return 1, err
	}
	defer func() { _ = snapshot.Close() }()

	if err := snapshot.WriteGeneration(generation); err != nil {
		return 1, err
	}

	paths := Paths{Root: opts.Root, Lower: opts.Lower}
	if err := opts.SetupFn(paths); err != nil {
		return 1, err
	}
	opts.logf("Root filesystem assembled", "upper", paths.Upper(), "merged", paths.Merged())

	// After pivot_root the overlay is /, so the opaque scan goes through the
	// descriptor opened above rather than through the (now hidden) root disk.
	flush := func() error {
		dirs, err := snapshot.Refresh()
		if err != nil {
			return fmt.Errorf("refresh opaque index: %w", err)
		}
		opts.logf("Refreshed opaque directory index", "opaqueDirs", len(dirs))
		return Sync()
	}

	code, runErr := Run(opts.Command, opts.Starter, ShutdownConfig{
		GracePeriod: opts.GracePeriod,
		Flush:       flush,
		Logf:        opts.Logf,
	})

	generation.CleanUnmount = true
	if err := snapshot.WriteGeneration(generation); err != nil {
		opts.logf("Could not mark clean unmount", "err", err.Error())
	}
	return code, runErr
}

func validateBootOptions(opts BootOptions) error {
	var errs []error
	if !filepath.IsAbs(opts.Root) {
		errs = append(errs, fmt.Errorf("root disk path %q must be absolute", opts.Root))
	}
	if opts.ImageRef == "" {
		errs = append(errs, errors.New("base image reference is required"))
	}
	if opts.ImageDigest == "" {
		errs = append(errs, errors.New("base image digest is required"))
	}
	if opts.Lower == "" {
		errs = append(errs, errors.New("lower mount path is required"))
	}
	return errors.Join(errs...)
}

// nextGeneration reads the previous checkpoint and returns the incremented
// one. A missing or unreadable file starts a new sequence rather than failing:
// the checkpoint is diagnostics, not a correctness requirement.
func nextGeneration(root string) (GenerationMeta, error) {
	current, err := ReadGenerationMeta(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return GenerationMeta{}, err
	}
	next := GenerationMeta{MountedAt: time.Now().UTC(), CleanUnmount: false}
	if current != nil {
		next.Generation = current.Generation + 1
	}
	return next, nil
}
