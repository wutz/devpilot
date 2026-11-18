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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

// ExecFlusher reaches the devpilot-init that runs as PID 1 of the DevBox pod.
// The helper binary is copied into an emptyDir that is detached from the
// overlay's lower layer, so /proc/1/exe is the only path to it after
// pivot_root; re-executing it with --flush syncs the filesystem behind the
// overlay, which is the root disk the snapshot will capture.
type ExecFlusher struct {
	// Config builds the exec clients. Nil means rest.InClusterConfig is used
	// through a client built per cluster; the manager config is preferred.
	Config *rest.Config
	// Timeout bounds the flush. A hung exec must not stall the commit forever.
	Timeout time.Duration
}

// flushCommand is the argv handed to the DevBox container.
//
// It must not be InitBinaryPath: that path lives in the emptyDir the init
// container staged, which pivot_root leaves outside the new root, so exec
// fails with "no such file or directory". /proc/1/exe is the running helper
// itself and survives the pivot.
func flushCommand() []string {
	return []string{selfExePath, "--flush"}
}

// selfExePath is PID 1 of the main container, which is always devpilot-init.
const selfExePath = "/proc/1/exe"

// Flush runs the flush in the main container of the pod. Anything other than a
// zero exit is reported so the commit fails instead of snapshotting a disk with
// unpublished writes.
func (f *ExecFlusher) Flush(ctx context.Context, pod *corev1.Pod) error {
	if f == nil || f.Config == nil {
		return errors.New("no rest config for the flush exec")
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = defaultFlushTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	exec, err := podExecutor(f.Config, pod, flushCommand(), false)
	if err != nil {
		return fmt.Errorf("open the flush exec: %w", err)
	}
	var stderr outputBuffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stderr,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("flush exec on pod %s: %w: %s", pod.Name, err, stderr.String())
	}
	return nil
}

// podExecutor opens a pods/exec stream to the main container running argv.
func podExecutor(config *rest.Config, pod *corev1.Pod, argv []string, stdin bool) (remotecommand.Executor, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("build the exec client: %w", err)
	}
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: MainContainerName,
			Command:   argv,
			Stdin:     stdin,
			Stdout:    true,
			Stderr:    true,
		}, clientscheme.ParameterCodec)
	return remotecommand.NewSPDYExecutor(config, "POST", req.URL())
}

// defaultFlushTimeout is generous against a large page cache but short enough
// that a broken helper does not look like a hung commit.
const defaultFlushTimeout = 60 * time.Second

// outputBuffer keeps the tail of the helper's output for the error message.
// The exec streams write from their own goroutines while a freeze session may
// read the tail for an error, hence the lock.
type outputBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	const limit = 2048
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > limit {
		b.data = b.data[len(b.data)-limit:]
	}
	return len(p), nil
}

func (b *outputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

// ExecFreezer freezes the root disk filesystem of a running DevBox through the
// devpilot-init helper. Like the flush it re-executes /proc/1/exe, here with
// --freeze and stdin attached: the helper freezes, prints "frozen", and holds
// the freeze until stdin closes, or until its own --freeze-timeout expires.
type ExecFreezer struct {
	// Config builds the exec clients.
	Config *rest.Config
	// StartTimeout bounds the wait for "frozen". Zero means
	// defaultFreezeStartTimeout.
	StartTimeout time.Duration
	// HelperTimeout is passed as --freeze-timeout, so a lost controller cannot
	// keep the DevBox frozen longer than this. Zero means
	// defaultFreezeHelperTimeout.
	HelperTimeout time.Duration
	// ThawTimeout bounds the wait for the helper to exit after the release,
	// and the standalone --thaw. Zero means defaultFreezeThawTimeout.
	ThawTimeout time.Duration
}

const (
	// defaultFreezeStartTimeout covers the exec round trip, the syncfs before
	// the freeze and FIFREEZE itself.
	defaultFreezeStartTimeout = 15 * time.Second
	// defaultFreezeHelperTimeout matches the helper's own default.
	defaultFreezeHelperTimeout = 60 * time.Second
	// defaultFreezeThawTimeout bounds how long a release may take to land.
	defaultFreezeThawTimeout = 15 * time.Second
	// freezeHelperMargin is how much longer the helper waits than the
	// controller's freeze window, so the controller is the one that releases
	// the freeze in the normal case and the helper's timeout only catches a
	// controller that went away.
	freezeHelperMargin = 10 * time.Second

	// freezeExitForced is the helper's exit code when its timeout or a signal
	// thawed the filesystem before the release (initroot.FreezeExitForced).
	freezeExitForced = 3
)

// NewExecFreezer returns an ExecFreezer whose helper outlives the controller's
// freeze window by a fixed margin.
func NewExecFreezer(config *rest.Config, window time.Duration) *ExecFreezer {
	return &ExecFreezer{Config: config, HelperTimeout: window + freezeHelperMargin}
}

// freezeCommand and thawCommand are the argv handed to the DevBox container.
func freezeCommand(timeout time.Duration) []string {
	return []string{selfExePath, "--freeze", "--freeze-timeout=" + timeout.String()}
}

func thawCommand() []string {
	return []string{selfExePath, "--thaw"}
}

func orDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// Freeze starts the helper and returns once it reported "frozen". On any
// failure the exec stream is torn down, which closes the helper's stdin and
// makes it thaw on its own if it got as far as freezing.
func (f *ExecFreezer) Freeze(ctx context.Context, pod *corev1.Pod) (FreezeSession, error) {
	if f == nil || f.Config == nil {
		return nil, errors.New("no rest config for the freeze exec")
	}
	startTimeout := orDefault(f.StartTimeout, defaultFreezeStartTimeout)
	helperTimeout := orDefault(f.HelperTimeout, defaultFreezeHelperTimeout)
	thawTimeout := orDefault(f.ThawTimeout, defaultFreezeThawTimeout)

	exec, err := podExecutor(f.Config, pod, freezeCommand(helperTimeout), true)
	if err != nil {
		return nil, fmt.Errorf("open the freeze exec: %w", err)
	}

	// The stream inherits ctx on purpose: if the reconcile is cancelled, the
	// stream closes, the helper reads EOF and thaws immediately.
	streamCtx, cancel := context.WithTimeout(ctx, startTimeout+helperTimeout+thawTimeout)
	stdinR, stdinW := io.Pipe()
	stdout := newLineWatcher(frozenLine)
	stderr := &outputBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- exec.StreamWithContext(streamCtx, remotecommand.StreamOptions{
			Stdin:  stdinR,
			Stdout: stdout,
			Stderr: stderr,
		})
	}()
	session := &execFreezeSession{
		pod: pod.Name, stdin: stdinW, done: done, cancel: cancel,
		stdout: stdout, stderr: stderr, thawTimeout: thawTimeout,
	}

	timer := time.NewTimer(startTimeout)
	defer timer.Stop()
	select {
	case <-stdout.seen:
		return session, nil
	case err := <-done:
		session.abort()
		if err == nil {
			err = errors.New("the helper exited without freezing")
		}
		return nil, fmt.Errorf("freeze exec on pod %s: %w: %s", pod.Name, err, session.output())
	case <-timer.C:
		session.abort()
		return nil, fmt.Errorf("freeze exec on pod %s did not report %q within %s: %s",
			pod.Name, frozenLine, startTimeout, session.output())
	case <-ctx.Done():
		session.abort()
		return nil, fmt.Errorf("freeze exec on pod %s: %w", pod.Name, ctx.Err())
	}
}

// Thaw runs the standalone --thaw. It is the safety net for a session whose
// stream broke while the filesystem may still be frozen.
func (f *ExecFreezer) Thaw(ctx context.Context, pod *corev1.Pod) error {
	if f == nil || f.Config == nil {
		return errors.New("no rest config for the thaw exec")
	}
	ctx, cancel := context.WithTimeout(ctx, orDefault(f.ThawTimeout, defaultFreezeThawTimeout))
	defer cancel()

	exec, err := podExecutor(f.Config, pod, thawCommand(), false)
	if err != nil {
		return fmt.Errorf("open the thaw exec: %w", err)
	}
	var output outputBuffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &output,
		Stderr: &output,
	}); err != nil {
		return fmt.Errorf("thaw exec on pod %s: %w: %s", pod.Name, err, output.String())
	}
	return nil
}

// frozenLine is what the helper prints once FIFREEZE succeeded
// (initroot.FrozenLine).
const frozenLine = "frozen"

// execFreezeSession is one running --freeze helper.
type execFreezeSession struct {
	pod         string
	stdin       *io.PipeWriter
	done        <-chan error
	cancel      context.CancelFunc
	stdout      *lineWatcher
	stderr      *outputBuffer
	thawTimeout time.Duration

	once sync.Once
	err  error
}

// Thaw releases the freeze by closing the helper's stdin and waits for it to
// exit. Only exit 0 proves the filesystem stayed frozen until this release;
// anything else means the freeze ended early and a snapshot cut during the
// session cannot be trusted.
func (s *execFreezeSession) Thaw(ctx context.Context) error {
	s.once.Do(func() {
		_ = s.stdin.Close()
		timer := time.NewTimer(s.thawTimeout)
		defer timer.Stop()
		select {
		case err := <-s.done:
			s.err = s.classify(err)
		case <-timer.C:
			s.err = fmt.Errorf("freeze helper on pod %s did not exit within %s of the release: %s",
				s.pod, s.thawTimeout, s.output())
		case <-ctx.Done():
			s.err = fmt.Errorf("freeze helper on pod %s: %w", s.pod, ctx.Err())
		}
		s.cancel()
	})
	return s.err
}

func (s *execFreezeSession) classify(err error) error {
	if err == nil {
		return nil
	}
	var exitErr utilexec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitStatus() == freezeExitForced {
		return fmt.Errorf("freeze helper on pod %s thawed on its own (timeout or signal) before the release: %s",
			s.pod, s.output())
	}
	return fmt.Errorf("freeze helper on pod %s: %w: %s", s.pod, err, s.output())
}

// abort tears the stream down without waiting; the helper thaws on EOF.
func (s *execFreezeSession) abort() {
	s.once.Do(func() {
		_ = s.stdin.Close()
		s.cancel()
		s.err = errors.New("freeze session aborted")
	})
}

func (s *execFreezeSession) output() string {
	return strings.TrimSpace(s.stdout.String() + "\n" + s.stderr.String())
}

// lineWatcher records the helper's stdout and signals the first time a given
// line appears.
type lineWatcher struct {
	want string
	seen chan struct{}

	mu      sync.Mutex
	partial []byte
	tail    outputBuffer
	closed  bool
}

func newLineWatcher(want string) *lineWatcher {
	return &lineWatcher{want: want, seen: make(chan struct{})}
}

func (w *lineWatcher) Write(p []byte) (int, error) {
	_, _ = w.tail.Write(p)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return len(p), nil
	}
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(w.partial[:i]))
		w.partial = w.partial[i+1:]
		if line == w.want {
			w.closed = true
			w.partial = nil
			close(w.seen)
			break
		}
	}
	// A helper that never prints a newline must not grow this unbounded.
	if len(w.partial) > 4096 {
		w.partial = w.partial[len(w.partial)-4096:]
	}
	return len(p), nil
}

func (w *lineWatcher) String() string {
	return w.tail.String()
}
