//go:build linux

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
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Child processes are started with Setpgid so one signal reaches the whole
// tree, and reaped here rather than through exec.Cmd.Wait: PID 1 must reap
// orphans, and a wait4(-1) racing exec.Cmd.Wait would steal the managed
// child's exit status. Instead every spawned pid is registered, and the
// reaper routes its status to a per-pid channel that Wait reads.
var (
	reapMu      sync.Mutex
	reapWaiters = map[int]chan unix.WaitStatus{}
	reapOnce    sync.Once
)

// DefaultStarter spawns the managed command in its own process group.
func DefaultStarter(cmd Command) (Process, error) {
	if cmd.Path == "" {
		return nil, errors.New("managed command path is empty")
	}
	path, err := exec.LookPath(cmd.Path)
	if err != nil {
		return nil, err
	}
	argv := append([]string{cmd.Path}, cmd.Args...)

	reapMu.Lock()
	defer reapMu.Unlock()

	proc, err := os.StartProcess(path, argv, &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Env:   cmd.Env,
		Sys:   &syscall.SysProcAttr{Setpgid: true},
	})
	if err != nil {
		return nil, err
	}
	status := make(chan unix.WaitStatus, 1)
	reapWaiters[proc.Pid] = status
	return &osProcess{pid: proc.Pid, status: status}, nil
}

type osProcess struct {
	pid    int
	status chan unix.WaitStatus

	once sync.Once
	code int
	err  error
}

func (p *osProcess) Pid() int { return p.pid }

// SignalGroup signals the whole group, so sshd, jupyter and anything a shell
// forked all receive it; signalling only the leader would leave them holding
// the root disk.
func (p *osProcess) SignalGroup(sig os.Signal) error {
	unixSig, ok := sig.(syscall.Signal)
	if !ok {
		return fmt.Errorf("unsupported signal %v", sig)
	}
	return unix.Kill(-p.pid, unixSig)
}

func (p *osProcess) Wait() (int, error) {
	p.once.Do(func() {
		status, ok := <-p.status
		if !ok {
			p.code, p.err = 1, errors.New("managed process status was lost")
			return
		}
		p.code = waitStatusExitCode(status)
	})
	return p.code, p.err
}

func waitStatusExitCode(status unix.WaitStatus) int {
	if status.Signaled() {
		return 128 + int(status.Signal())
	}
	return status.ExitStatus()
}

// startReaper drains children reparented to PID 1 so the container never
// accumulates zombies. It runs once per process lifetime.
func startReaper() {
	reapOnce.Do(func() {
		go func() {
			idle := unix.Timespec{Nsec: 100 * 1000 * 1000}
			for {
				reaped := reapOnceAll()
				if !reaped {
					_ = unix.Nanosleep(&idle, nil)
				}
			}
		}()
	})
}

// reapOnceAll reaps every child that has exited, returning whether it did any
// work. The lock is held across wait4 and registration so a pid cannot exit
// between being spawned and being registered.
func reapOnceAll() bool {
	reapedAny := false
	for {
		var status unix.WaitStatus
		reapMu.Lock()
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if err == nil && pid > 0 {
			if ch, ok := reapWaiters[pid]; ok {
				delete(reapWaiters, pid)
				ch <- status
			}
			reapedAny = true
		}
		reapMu.Unlock()

		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case err != nil && !errors.Is(err, unix.ECHILD):
			return reapedAny
		case err == nil && pid <= 0:
			return reapedAny
		}
	}
}
