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
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// JupyterDir is where the operator binds the token Secret. Like SSHDir it sits
// outside /run/devpilot, so migrateSubmounts carries it into the pivoted root.
const JupyterDir = "/run/devpilot-jupyter"

// DefaultJupyterPort is JupyterLab's own default port.
const DefaultJupyterPort int32 = 8888

// jupyterStopTimeout bounds the graceful shutdown of JupyterLab on pod
// termination. The pod's own grace period is the outer limit; this only keeps
// the kernel processes from outliving it.
const jupyterStopTimeout = 10 * time.Second

// jupyterBinaries are tried in order. "jupyter lab" is the subcommand form the
// python package installs; the wrapper script is a shortcut some images ship.
var jupyterBinaries = []struct {
	path string
	args []string
}{
	{path: "jupyter-lab"},
	{path: "jupyter", args: []string{"lab"}},
}

// JupyterOptions configures the hosted JupyterLab. A nil Jupyter in BootOptions
// disables it, which is what happens when spec.access.jupyter is absent or off.
type JupyterOptions struct {
	// Port is the port JupyterLab binds on all interfaces.
	Port int32
	// TokenFile holds the login token; it is read once at start.
	TokenFile string
	// RootDir is the notebook root, /root by default.
	RootDir string
	// Env is the environment the server inherits. Nil inherits this process's.
	Env []string
	// Start is injectable for tests; nil runs the real process.
	Start func(ctx context.Context, argv []string, env []string, logf LogFunc) error
}

// Resolve keeps every default in one place so the flag wiring in main stays a
// straight assignment.
func (o *JupyterOptions) Resolve() {
	if o.Port == 0 {
		o.Port = DefaultJupyterPort
	}
	if o.TokenFile == "" {
		o.TokenFile = JupyterDir + "/token"
	}
	if o.RootDir == "" {
		o.RootDir = "/root"
	}
	if o.Env == nil {
		o.Env = os.Environ()
	}
}

// jupyterArgv renders the server command line. The token is deliberately not in
// it: it travels in the environment, so /proc/<pid>/cmdline never shows it.
func (o JupyterOptions) jupyterArgv(binary string, lead []string) []string {
	argv := append(append([]string{binary}, lead...), o.labFlags()...)
	return argv
}

// labFlags are the flags every launch form shares.
func (o JupyterOptions) labFlags() []string {
	return []string{
		"--no-browser",
		"--ip=0.0.0.0",
		"--port=" + strconv.Itoa(int(o.Port)),
		"--allow-root",
		"--ServerApp.root_dir=" + o.RootDir,
	}
}

// startJupyter launches JupyterLab in the background.
//
// It runs after pivot_root, so notebooks, kernels and every file the server
// touches live in the persistent root. A failure is logged and otherwise
// ignored: an image without JupyterLab, or a missing token, must not stop the
// user's own process.
func startJupyter(ctx context.Context, o JupyterOptions, logf LogFunc) {
	o.Resolve()
	start := o.Start
	if start == nil {
		start = runProcess
	}

	token, err := readToken(o.TokenFile)
	if err != nil {
		logf("Could not read the Jupyter token; the server stays off", "path", o.TokenFile, "err", err.Error())
		return
	}

	for _, candidate := range jupyterBinaries {
		if _, err := exec.LookPath(candidate.path); err != nil {
			continue
		}
		env := append(append([]string{}, o.Env...), "JUPYTER_TOKEN="+token)
		argv := o.jupyterArgv(candidate.path, candidate.args)
		go func() {
			if err := start(ctx, argv, env, logf); err != nil {
				logf("JupyterLab stopped", "err", err.Error())
			}
		}()
		return
	}
	logf("JupyterLab is not installed in this image; spec.access.jupyter stays off",
		"tried", strings.Join([]string{"jupyter-lab", "jupyter"}, ", "))
}

// readToken reads the login token and refuses an empty one: an unauthenticated
// server behind a public host is worse than none.
func readToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return token, nil
}

// runProcess runs the server until it exits or ctx is cancelled. It gets its
// own process group so that cancelling the context at pod shutdown takes the
// kernel child processes with it.
func runProcess(ctx context.Context, argv []string, env []string, logf LogFunc) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", argv[0], err)
	}
	logf("Started JupyterLab", "pid", cmd.Process.Pid, "port", jupyterPortArgument(argv))

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// A TERM to the group lets the kernels shut down and flush their state
		// before the pod's grace period runs out.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
			return nil
		}
		select {
		case <-done:
		case <-time.After(jupyterStopTimeout):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
}

// jupyterPortArgument extracts the port from argv for the log line.
func jupyterPortArgument(argv []string) string {
	for _, arg := range argv {
		if port, ok := strings.CutPrefix(arg, "--port="); ok {
			return port
		}
	}
	return ""
}
