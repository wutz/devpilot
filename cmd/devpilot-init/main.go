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

// Command devpilot-init is the PID 1 of a DevBox pod. It pins the base image
// digest, assembles the persistent OverlayFS root from the root disk PVC,
// migrates the container's initial submounts (GPU drivers, /dev/shm, service
// account tokens), pivots into the overlay and then supervises the user's
// command until the kubelet sends SIGTERM.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wutz/devpilot/internal/initroot"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run parses the command line and boots the container. It returns the exit
// code to hand to the kubelet: the managed command's own code, or 1 when the
// root could not be assembled.
//
// Flags must precede "--"; everything after it is the user's command, which is
// how the operator passes the base image's original entrypoint and args.
func run(args []string, stderr *os.File) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return runWithContext(ctx, args, stderr)
}

func runWithContext(ctx context.Context, args []string, stderr *os.File) int {
	return runIOWithContext(ctx, args, os.Stdin, os.Stdout, stderr)
}

// runIO is run with the standard streams injectable; only the freeze helper
// talks on stdin and stdout.
func runIO(args []string, stdin io.Reader, stdout io.Writer, stderr *os.File) int {
	return runIOWithContext(context.Background(), args, stdin, stdout, stderr)
}

func runIOWithContext(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr *os.File) int {
	logf := func(msg string, kv ...any) {
		_, _ = fmt.Fprintf(stderr, "devpilot-init: %s\n", initroot.FormatMessage(msg, kv...))
	}

	fs := flag.NewFlagSet("devpilot-init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		root        string
		imageRef    string
		imageDigest string
		lower       string
		installPath string
		gracePeriod time.Duration
		flush       bool
		freeze      bool
		thaw        bool
		freezeLimit time.Duration
		rebase      bool
		noop        bool
		hold        bool
		sshListen   string
		sshHostKey  string
		sshAuthKeys string
		sshShell    string
		jupyterPort int
		jupyterTok  string
	)
	fs.StringVar(&installPath, "install", "",
		"Copy this binary to the given path and exit; used by the init container that populates the shared volume.")
	fs.StringVar(&root, "root", initroot.DefaultRootDisk,
		"Mount point of the root disk PVC that holds upper/, work/ and merged/.")
	fs.StringVar(&imageRef, "image-ref", os.Getenv("DEVPILOT_BASE_IMAGE"),
		"Base image reference the pod was scheduled from; recorded in .meta/base-image.json.")
	fs.StringVar(&imageDigest, "image-digest", os.Getenv("DEVPILOT_BASE_IMAGE_DIGEST"),
		"Base image digest; must match the pinned digest once upper/ holds data.")
	fs.StringVar(&lower, "lower", initroot.BaseLower,
		"Path the container's initial / is bound to read-only and used as lowerdir.")
	fs.DurationVar(&gracePeriod, "grace-period", 30*time.Second,
		"How long the managed command gets to exit after SIGTERM before its process group is killed.")
	fs.BoolVar(&rebase, "rebase", false,
		"Discard upper/ and pin the pod's base image as the new lower layer; set by the operator after a rebase commit.")
	fs.BoolVar(&flush, "flush", false,
		"Flush the root filesystem and exit; used by the operator before taking an online commit snapshot.")
	fs.BoolVar(&noop, "noop", false,
		"Exit 0 immediately; used by one-shot image pre-pull and pre-warm Pods after the kubelet pulls the image.")
	fs.BoolVar(&hold, "hold", false,
		"Sleep until SIGTERM/SIGINT; used by pinned ImagePrewarm sentinel Pods to prevent kubelet image GC.")
	fs.BoolVar(&freeze, "freeze", false,
		"Freeze the root disk filesystem, print \"frozen\", and thaw once stdin delivers a line or EOF; "+
			"used by the operator around an online commit snapshot. Exit 0: released via stdin; "+
			"2: never frozen; 3: thawed by timeout or signal (the snapshot is invalid); 4: thaw failed.")
	fs.BoolVar(&thaw, "thaw", false,
		"Thaw the root disk filesystem and exit; a filesystem that is not frozen counts as thawed.")
	fs.DurationVar(&freezeLimit, "freeze-timeout", initroot.DefaultFreezeTimeout,
		"Longest time --freeze keeps the filesystem frozen before thawing on its own.")
	fs.StringVar(&sshListen, "ssh-listen", "",
		"Address the built-in SSH server binds, e.g. \":22\"; empty disables it.")
	fs.StringVar(&sshHostKey, "ssh-host-key", "",
		"Path to the OpenSSH private host key; defaults to <ssh-dir>/ssh_host_ed25519_key.")
	fs.StringVar(&sshAuthKeys, "ssh-authorized-keys", "",
		"Path to the authorized_keys file; defaults to <ssh-dir>/authorized_keys.")
	fs.StringVar(&sshShell, "ssh-shell", "",
		"Login shell for SSH sessions; empty means /bin/bash.")
	fs.IntVar(&jupyterPort, "jupyter-port", 0,
		"Port JupyterLab binds on 0.0.0.0; 0 disables the hosted server.")
	fs.StringVar(&jupyterTok, "jupyter-token-file", "",
		"File holding the JupyterLab login token; the server is skipped when it cannot be read.")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if noop {
		logf("Noop complete")
		return 0
	}

	if hold {
		logf("Holding sentinel pod until signal")
		<-ctx.Done()
		logf("Released sentinel hold")
		return 0
	}

	if freeze && thaw {
		logf("--freeze and --thaw are mutually exclusive")
		return 1
	}

	// A strict online commit freezes the root disk around the snapshot. Like
	// --flush this runs as an exec'd second instance: it reaches the root disk
	// through the descriptors PID 1 opened before pivot_root, and it carries
	// the container's CAP_SYS_ADMIN, which PID 1 itself dropped.
	if freeze {
		return initroot.FreezeRootDisk(initroot.FreezeOptions{
			Timeout: freezeLimit,
			Stdin:   stdin,
			Stdout:  stdout,
			Logf:    logf,
		})
	}
	if thaw {
		if err := initroot.ThawRootDisk(); err != nil {
			logf("Could not thaw the root filesystem", "err", err.Error())
			return 1
		}
		logf("Thawed the root filesystem")
		return 0
	}

	// An online commit snapshots the root disk while the pod keeps running, so
	// the writes still in the page cache have to reach the block device first.
	// This runs as a second invocation of the same binary: /proc/1/exe is the
	// only stable path to it after pivot_root.
	if flush {
		if err := initroot.Sync(); err != nil {
			logf("Could not flush the root filesystem", "err", err.Error())
			return 1
		}
		logf("Flushed the root filesystem")
		return 0
	}

	// The init container only stages the helper into the shared emptyDir; it
	// never assembles a root and exits as soon as the copy is in place.
	if installPath != "" {
		if err := initroot.Install(installPath); err != nil {
			logf("Could not install devpilot-init", "err", err.Error())
			return 1
		}
		logf("Installed devpilot-init", "path", installPath)
		return 0
	}

	argv := fs.Args()
	if len(argv) == 0 {
		argv = initroot.DefaultCommand
	}

	// A nil SSH leaves the image to run its own sshd, which is the case for
	// every DevBox whose spec.access.ssh is absent.
	var sshOptions *initroot.SSHOptions
	if sshListen != "" {
		sshOptions = &initroot.SSHOptions{
			Listen:             sshListen,
			HostKeyPath:        sshHostKey,
			AuthorizedKeysPath: sshAuthKeys,
			Shell:              sshShell,
		}
	}

	// A nil Jupyter leaves the image to serve its own notebooks; the operator
	// only sets the flags when spec.access.jupyter asks for the hosted one.
	var jupyterOptions *initroot.JupyterOptions
	if jupyterPort > 0 {
		jupyterOptions = &initroot.JupyterOptions{
			Port:      int32(jupyterPort),
			TokenFile: jupyterTok,
		}
	}

	code, err := initroot.Boot(initroot.BootOptions{
		Root:        root,
		ImageRef:    imageRef,
		ImageDigest: imageDigest,
		Lower:       lower,
		Rebase:      rebase,
		GracePeriod: gracePeriod,
		Command:     initroot.Command{Path: argv[0], Args: argv[1:]},
		SSH:         sshOptions,
		Jupyter:     jupyterOptions,
		Logf:        logf,
	})
	if err != nil {
		logf("Startup failed", "err", err.Error())
		return 1
	}
	return code
}
