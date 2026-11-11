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
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// LogFunc receives lifecycle messages: a Kubernetes-style message plus
// alternating keys and values. devpilot-init renders them to stderr; tests
// route them into the test log.
type LogFunc func(msg string, kv ...any)

// FormatMessage renders a structured message for plain stderr output:
// "Started managed process pid=42 path=/bin/sh".
func FormatMessage(msg string, kv ...any) string {
	var b strings.Builder
	b.WriteString(msg)
	for i := 0; i < len(kv); i += 2 {
		if i+1 == len(kv) {
			fmt.Fprintf(&b, " !BADKEY=%v", kv[i])
			break
		}
		fmt.Fprintf(&b, " %v=%v", kv[i], kv[i+1])
	}
	return b.String()
}

// Command describes the process devpilot-init supervises after pivot_root.
type Command struct {
	Path string
	Args []string
	Env  []string
}

// Process is one child process. The interface exists so the supervisor's
// signal handling and shutdown sequence can be tested on any platform.
type Process interface {
	// Pid returns the child process id.
	Pid() int
	// SignalGroup delivers sig to the child's process group.
	SignalGroup(sig os.Signal) error
	// Wait blocks until the child exits and returns its exit code.
	Wait() (int, error)
}

// Starter spawns the managed process in its own process group.
type Starter func(Command) (Process, error)

// ShutdownConfig controls what happens on SIGTERM.
type ShutdownConfig struct {
	// GracePeriod is how long children get to exit after SIGTERM.
	GracePeriod time.Duration
	// Flush persists overlay state and pushes dirty pages to the root disk.
	// Nil means Sync() only; devpilot-init supplies a closure that also
	// refreshes the opaque directory index.
	Flush func() error
	// Logf receives lifecycle messages as a message plus key/value pairs,
	// following the Kubernetes logging conventions. Nil discards them.
	Logf LogFunc
	// Signals overrides the OS signal source. Nil means the real one; tests
	// inject a channel so they never signal the test binary itself.
	Signals <-chan os.Signal
}

func (c ShutdownConfig) logf(msg string, kv ...any) {
	if c.Logf != nil {
		c.Logf(msg, kv...)
	}
}

// Run is the PID 1 loop: forward signals to the child's process group, reap
// orphans, and on shutdown refresh the opaque index and sync the root disk
// before exiting. It returns the child's exit code.
func Run(cmd Command, start Starter, cfg ShutdownConfig) (int, error) {
	if start == nil {
		return 1, errors.New("no process starter configured")
	}
	if cfg.GracePeriod <= 0 {
		cfg.GracePeriod = 30 * time.Second
	}

	sigs := cfg.Signals
	if sigs == nil {
		osSigs := make(chan os.Signal, 8)
		signal.Notify(osSigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGHUP)
		defer signal.Stop(osSigs)
		sigs = osSigs
	}

	child, err := start(cmd)
	if err != nil {
		return 1, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	cfg.logf("Started managed process", "pid", child.Pid(), "path", cmd.Path)

	// Orphans are reparented to PID 1. The starter owns reaping because only
	// it can distinguish orphans from the managed child, whose exit status
	// must reach Wait() intact.
	startReaper()

	done := make(chan int, 1)
	go func() {
		code, _ := child.Wait()
		done <- code
	}()

	for {
		select {
		case code := <-done:
			reportFlush(cfg, flush(cfg))
			return code, nil
		case sig := <-sigs:
			switch sig {
			case syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT:
				code := shutdown(child, done, sig, cfg)
				reportFlush(cfg, flush(cfg))
				return code, nil
			case syscall.SIGHUP:
				cfg.logf("Forwarding SIGHUP to process group", "pid", child.Pid())
				_ = child.SignalGroup(sig)
			}
		}
	}
}

// shutdown asks the child to stop, waits for the grace period and then kills
// the whole process group so no straggler keeps the PVC mounted.
func shutdown(child Process, done <-chan int, sig os.Signal, cfg ShutdownConfig) int {
	cfg.logf("Received shutdown signal, stopping managed process", "signal", sig.String(), "pid", child.Pid())
	if err := child.SignalGroup(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
		cfg.logf("Could not forward shutdown signal", "err", err.Error())
	}

	timer := time.NewTimer(cfg.GracePeriod)
	defer timer.Stop()
	select {
	case code := <-done:
		return code
	case <-timer.C:
		cfg.logf("Grace period expired, killing process group", "pid", child.Pid())
		if err := child.SignalGroup(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			cfg.logf("Could not kill process group", "err", err.Error())
		}
		return <-done
	}
}

// flush runs the configured shutdown hook, defaulting to a bare sync so the
// supervisor is usable without a root disk.
func flush(cfg ShutdownConfig) error {
	if cfg.Flush != nil {
		return cfg.Flush()
	}
	return Sync()
}

func reportFlush(cfg ShutdownConfig, err error) {
	if err != nil {
		cfg.logf("Flush before exit failed; recent changes may not be on the root disk", "err", err.Error())
		return
	}
	cfg.logf("Flushed root disk")
}

// RunContext runs the supervisor until ctx is cancelled, then treats the
// cancellation as SIGTERM.
func RunContext(ctx context.Context, cmd Command, start Starter, cfg ShutdownConfig) (int, error) {
	go func() {
		<-ctx.Done()
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()
	return Run(cmd, start, cfg)
}
