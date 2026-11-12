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
	"context"
	"os"

	"github.com/wutz/devpilot/internal/sshhost"
)

// SSHDir is where the operator binds the per-DevBox host key Secret. It sits
// outside /run/devpilot on purpose: migrateSubmounts deliberately leaves that
// prefix out of the merged root, so a key mounted there would be invisible to
// the server after pivot_root.
const SSHDir = "/run/devpilot-ssh"

// DefaultSSHListen is the port the built-in server takes. Port 22 rather than a
// high port so an unmodified `ssh <host>` works, and so the Gateway's SSHRoute
// can forward to the same service port the user's own sshd would use.
const DefaultSSHListen = ":22"

// SSHOptions configures the built-in SSH server. A nil SSH in BootOptions
// disables it, which is what happens when spec.access.ssh is absent or the
// image is expected to run its own sshd.
type SSHOptions struct {
	// Listen is the address to bind, ":22" by default.
	Listen string
	// HostKeyPath and AuthorizedKeysPath are files inside the Secret mount.
	HostKeyPath        string
	AuthorizedKeysPath string
	// Shell is the interactive login shell; empty means /bin/bash.
	Shell string
	// Env is the environment sessions inherit. Nil inherits this process's.
	Env []string
	// Serve is injectable for tests; nil means sshhost.Serve.
	Serve func(ctx context.Context, cfg sshhost.Config) error
}

// Resolve keeps every default in one place so the flag wiring in main stays a
// straight assignment.
func (o *SSHOptions) Resolve() {
	if o.Listen == "" {
		o.Listen = DefaultSSHListen
	}
	if o.HostKeyPath == "" {
		o.HostKeyPath = SSHDir + "/ssh_host_ed25519_key"
	}
	if o.AuthorizedKeysPath == "" {
		o.AuthorizedKeysPath = SSHDir + "/authorized_keys"
	}
	if o.Env == nil {
		o.Env = os.Environ()
	}
}

// startSSH loads the key material and serves in the background.
//
// It runs after pivot_root, so a session's working directory and every path it
// touches are the persistent root, not the image's throwaway one. A failure is
// logged and otherwise ignored: a broken SSH endpoint must not stop the user's
// own process, which is still reachable through the pod's other means.
func startSSH(ctx context.Context, o SSHOptions, logf LogFunc) {
	o.Resolve()
	serve := o.Serve
	if serve == nil {
		serve = sshhost.Serve
	}

	cfg := sshhost.Config{
		Addr:  o.Listen,
		Shell: o.Shell,
		Env:   o.Env,
		Logf:  logf,
	}
	if err := cfg.Load(o.HostKeyPath, o.AuthorizedKeysPath); err != nil {
		logf("Could not load SSH key material; the built-in server stays off", "err", err.Error())
		return
	}

	go func() {
		if err := serve(ctx, cfg); err != nil {
			logf("SSH server stopped", "err", err.Error())
		}
	}()
}
