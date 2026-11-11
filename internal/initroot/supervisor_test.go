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
	"os"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeProcess stands in for the managed command so the supervisor can be
// exercised without spawning anything.
type fakeProcess struct {
	pid    int
	exits  chan int
	sigs   chan os.Signal
	sigsMu sync.Mutex
}

func newFakeProcess() *fakeProcess {
	return &fakeProcess{pid: 4242, exits: make(chan int, 4), sigs: make(chan os.Signal, 8)}
}

func (p *fakeProcess) Pid() int { return p.pid }

func (p *fakeProcess) SignalGroup(sig os.Signal) error {
	p.sigsMu.Lock()
	defer p.sigsMu.Unlock()
	p.sigs <- sig
	return nil
}

func (p *fakeProcess) Wait() (int, error) { return <-p.exits, nil }

func (p *fakeProcess) signals() []os.Signal {
	p.sigsMu.Lock()
	defer p.sigsMu.Unlock()
	var out []os.Signal
	for {
		select {
		case sig := <-p.sigs:
			out = append(out, sig)
		default:
			return out
		}
	}
}

type runResult struct {
	code int
	err  error
}

func containsSignal(signals []os.Signal, want os.Signal) bool {
	return slices.Contains(signals, want)
}

// startSupervisor runs Run in the background with a signal channel the test
// controls, and returns the result channel plus the fake child.
func startSupervisor(t *testing.T, cfg ShutdownConfig) (<-chan runResult, *fakeProcess, chan os.Signal) {
	t.Helper()
	child := newFakeProcess()
	sigs := make(chan os.Signal, 8)
	cfg.Signals = sigs
	cfg.Logf = func(msg string, kv ...any) { t.Log(FormatMessage(msg, kv...)) }
	if cfg.Flush == nil {
		// Never fall through to the real Sync: syncfs on the test host is
		// unbounded and would make these tests depend on runner disk pressure.
		cfg.Flush = func() error { return nil }
	}

	result := make(chan runResult, 1)
	go func() {
		code, err := Run(Command{Path: "/usr/bin/sleep", Args: []string{"infinity"}},
			func(Command) (Process, error) { return child, nil }, cfg)
		result <- runResult{code: code, err: err}
	}()
	return result, child, sigs
}

func waitResult(t *testing.T, result <-chan runResult) runResult {
	t.Helper()
	select {
	case r := <-result:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not return")
		return runResult{}
	}
}

func TestRunFlushesWhenCommandExits(t *testing.T) {
	flushed := make(chan struct{}, 1)
	result, child, _ := startSupervisor(t, ShutdownConfig{
		Flush: func() error { flushed <- struct{}{}; return nil },
	})

	child.exits <- 7
	got := waitResult(t, result)
	if got.err != nil {
		t.Fatalf("Run() error = %v", got.err)
	}
	if got.code != 7 {
		t.Errorf("Run() = %d, want the command's exit code 7", got.code)
	}
	select {
	case <-flushed:
	default:
		t.Error("Run() did not flush before returning")
	}
}

func TestRunForwardsShutdownSignalAndReturnsChildCode(t *testing.T) {
	result, child, sigs := startSupervisor(t, ShutdownConfig{GracePeriod: 2 * time.Second})

	sigs <- syscall.SIGTERM
	deadline := time.Now().Add(5 * time.Second)
	for len(child.signals()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("SIGTERM was not forwarded to the process group")
		}
		time.Sleep(5 * time.Millisecond)
	}

	child.exits <- 0
	got := waitResult(t, result)
	if got.err != nil || got.code != 0 {
		t.Errorf("Run() = (%d, %v), want (0, nil)", got.code, got.err)
	}
}

func TestRunKillsProcessGroupAfterGracePeriod(t *testing.T) {
	result, child, sigs := startSupervisor(t, ShutdownConfig{GracePeriod: 20 * time.Millisecond})

	sigs <- syscall.SIGTERM
	var seen []os.Signal
	deadline := time.Now().Add(5 * time.Second)
	for !containsSignal(seen, syscall.SIGKILL) {
		seen = append(seen, child.signals()...)
		if time.Now().After(deadline) {
			t.Fatalf("grace period escalation did not happen; signals = %v", seen)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !containsSignal(seen, syscall.SIGTERM) {
		t.Errorf("signals = %v, want SIGTERM forwarded before SIGKILL", seen)
	}

	child.exits <- 137
	got := waitResult(t, result)
	if got.code != 137 {
		t.Errorf("Run() = %d, want 137 after SIGKILL", got.code)
	}
}

func TestRunForwardsSIGHUPWithoutStopping(t *testing.T) {
	result, child, sigs := startSupervisor(t, ShutdownConfig{})

	sigs <- syscall.SIGHUP
	deadline := time.Now().Add(5 * time.Second)
	for len(child.signals()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("SIGHUP was not forwarded")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The supervisor must still be supervising after the reload signal.
	child.exits <- 3
	got := waitResult(t, result)
	if got.code != 3 {
		t.Errorf("Run() = %d, want 3", got.code)
	}
}

func TestRunFailsWithoutStarter(t *testing.T) {
	if code, err := Run(Command{Path: "/bin/true"}, nil, ShutdownConfig{}); err == nil || code != 1 {
		t.Errorf("Run() = (%d, %v), want (1, error)", code, err)
	}
}

func TestRunReportsStartFailure(t *testing.T) {
	code, err := Run(Command{Path: "/bin/true"}, func(Command) (Process, error) {
		return nil, os.ErrPermission
	}, ShutdownConfig{})
	if err == nil || code != 1 {
		t.Errorf("Run() = (%d, %v), want (1, error)", code, err)
	}
}
