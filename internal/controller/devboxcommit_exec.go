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
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
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

	clientset, err := kubernetes.NewForConfig(f.Config)
	if err != nil {
		return fmt.Errorf("build the exec client: %w", err)
	}
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: MainContainerName,
			Command:   flushCommand(),
			Stdout:    true,
			Stderr:    true,
		}, clientscheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(f.Config, "POST", req.URL())
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

// defaultFlushTimeout is generous against a large page cache but short enough
// that a broken helper does not look like a hung commit.
const defaultFlushTimeout = 60 * time.Second

// outputBuffer keeps the tail of the helper's output for the error message.
type outputBuffer struct {
	data []byte
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	const limit = 2048
	b.data = append(b.data, p...)
	if len(b.data) > limit {
		b.data = b.data[len(b.data)-limit:]
	}
	return len(p), nil
}

func (b *outputBuffer) String() string {
	return string(b.data)
}
