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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// RootDiskPVCSuffix is appended to the DevBox name for the root disk claim.
	RootDiskPVCSuffix = "-rootdisk"
	// PodSuffix is appended to the DevBox name for the running pod.
	PodSuffix = "-pod"

	// LabelManagedBy marks every object the operator owns.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// LabelDevBoxName points back at the owning DevBox.
	LabelDevBoxName = "devpilot.wutz.dev/devbox"
	// ValueManagedBy is the LabelManagedBy value.
	ValueManagedBy = "devpilot"

	// AnnotationSpecHash fingerprints the pod template so an in-place
	// incompatible spec change triggers a pod replacement.
	AnnotationSpecHash = "devpilot.wutz.dev/spec-hash"
	// AnnotationAction carries a one-shot stop/start/restart/reset-rootdisk.
	// The controller removes it once applied, so the same verb can be sent
	// again later without being mistaken for a replay.
	AnnotationAction = "devpilot.wutz.dev/action"

	// InitContainerName copies the devpilot-init binary into the shared volume.
	InitContainerName = "devpilot-init-install"
	// MainContainerName runs the user command under devpilot-init.
	MainContainerName = "devbox"
	// InitVolumeName is the emptyDir holding the helper binary.
	InitVolumeName = "devpilot-init"
	// InitMountPath is where the emptyDir is mounted in both containers.
	InitMountPath = "/run/devpilot"
	// InitBinaryPath is the helper's final path inside the main container.
	InitBinaryPath = InitMountPath + "/devpilot-init"
	// RootDiskMountPath must match initroot.DefaultRootDisk.
	RootDiskMountPath = "/mnt/.devpilot-rootdisk"

	// RootDiskVolumeName is the pod volume backed by the root disk claim.
	RootDiskVolumeName = "rootdisk"
	// ShmVolumeName is the memory backed /dev/shm volume.
	ShmVolumeName = "shm"

	// DefaultGPUNodeLabel selects GPU nodes by model on bz1-dev1.
	DefaultGPUNodeLabel = "scclabs.dev/gpu"
	// ZoneLabel is the standard zone topology label.
	ZoneLabel = "topology.kubernetes.io/zone"
	// GPUResourceName is the NVIDIA extended resource.
	GPUResourceName corev1.ResourceName = "nvidia.com/gpu"

	// ConditionReady summarises whether the DevBox is usable.
	ConditionReady = "Ready"
	// ConditionRootDiskReady tracks the root disk claim.
	ConditionRootDiskReady = "RootDiskReady"

	requeueWhileStarting = 5 * time.Second

	// capabilityAll is the capability set that drops every Linux capability.
	capabilityAll corev1.Capability = "ALL"
	// capabilityDACReadSearch bypasses read and search permission checks
	// without granting any write access to the files it unlocks.
	capabilityDACReadSearch corev1.Capability = "DAC_READ_SEARCH"
)

// action is a parsed devpilot.wutz.dev/action value.
type action struct {
	verb string
	// raw is the annotation value that was consumed.
	raw string
}

const (
	actionStop          = "stop"
	actionStart         = "start"
	actionRestart       = "restart"
	actionResetRootDisk = "reset-rootdisk"
)

// parseAction splits "verb=<timestamp>" and validates the verb. The timestamp
// is informational: the handled annotation is what prevents replays.
func parseAction(raw string) (action, error) {
	verb, _, _ := strings.Cut(strings.TrimSpace(raw), "=")
	verb = strings.TrimSpace(verb)
	switch verb {
	case actionStop, actionStart, actionRestart, actionResetRootDisk:
		return action{verb: verb, raw: raw}, nil
	}
	return action{}, fmt.Errorf("unsupported action %q: want one of %s, %s, %s, %s",
		raw, actionStop, actionStart, actionRestart, actionResetRootDisk)
}

// canReset reports whether the lifecycle allows wiping the root disk.
func canReset(phase devpilotv1alpha1.DevBoxPhase) bool {
	switch phase {
	case devpilotv1alpha1.PhaseStopped, devpilotv1alpha1.PhaseInterrupted, devpilotv1alpha1.PhaseFailed:
		return true
	default:
		return false
	}
}

// podReady reports the PodReady condition.
func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// pendingStep picks the most informative StartingStep from the pod status.
func pendingStep(pod *corev1.Pod) (devpilotv1alpha1.StartingStep, string) {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
			if cond.Reason == corev1.PodReasonUnschedulable {
				return devpilotv1alpha1.StartingStepAllocatingResources, "Waiting for a node: " + cond.Message
			}
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Waiting == nil {
			continue
		}
		switch status.State.Waiting.Reason {
		case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
			return devpilotv1alpha1.StartingStepPullingImage, status.State.Waiting.Message
		case "CreateContainerConfigError", "CreateContainerError":
			return devpilotv1alpha1.StartingStepMountingStorage, status.State.Waiting.Message
		}
	}
	if pod.Spec.NodeName != "" {
		return devpilotv1alpha1.StartingStepStartingContainers, "Waiting for the devbox container to become ready"
	}
	return devpilotv1alpha1.StartingStepAllocatingResources, "Waiting for the pod to be scheduled"
}

// podFailureMessage collects the most useful failure text from a pod.
func podFailureMessage(pod *corev1.Pod) string {
	var parts []string
	if pod.Status.Message != "" {
		parts = append(parts, pod.Status.Message)
	}
	if pod.Status.Reason != "" {
		parts = append(parts, pod.Status.Reason)
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if term := status.State.Terminated; term != nil && term.Message != "" {
			parts = append(parts, status.Name+": "+term.Message)
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if term := status.State.Terminated; term != nil && term.Message != "" {
			parts = append(parts, status.Name+": "+term.Message)
		}
	}
	if len(parts) == 0 {
		return "the devbox pod failed"
	}
	return strings.Join(parts, ": ")
}

// shmSize computes the /dev/shm sizeLimit from shmRatio and the memory limit.
func shmSize(spec devpilotv1alpha1.DevBoxResources) (*apiresource.Quantity, error) {
	ratio := spec.ShmRatio
	if ratio == "" {
		ratio = "0.5"
	}
	memory := spec.Memory
	if memory.IsZero() {
		memory = apiresource.MustParse("8Gi")
	}
	fraction, err := strconv.ParseFloat(ratio, 64)
	if err != nil {
		return nil, fmt.Errorf("parse shmRatio %q: %w", ratio, err)
	}
	if fraction < 0 || fraction > 1 {
		return nil, fmt.Errorf("shmRatio %q must be between 0 and 1", ratio)
	}
	size := apiresource.NewQuantity(int64(float64(memory.Value())*fraction), apiresource.BinarySI)
	return size, nil
}

// containerResources returns limits and requests. They are equal so a GPU
// devbox always gets Guaranteed QoS and is never evicted for memory pressure.
func containerResources(spec devpilotv1alpha1.DevBoxResources) (corev1.ResourceList, corev1.ResourceList) {
	cpu := spec.CPU
	if cpu.IsZero() {
		cpu = apiresource.MustParse("4")
	}
	memory := spec.Memory
	if memory.IsZero() {
		memory = apiresource.MustParse("8Gi")
	}
	limits := corev1.ResourceList{
		corev1.ResourceCPU:    cpu,
		corev1.ResourceMemory: memory,
	}
	if spec.GPUs > 0 {
		gpus := *apiresource.NewQuantity(int64(spec.GPUs), apiresource.DecimalSI)
		limits[GPUResourceName] = gpus
	}
	return limits, limits.DeepCopy()
}

// nodeSelectors merges the user's selector with the GPU model and zone
// constraints implied by the spec. User entries win on conflict.
func nodeSelectors(box *devpilotv1alpha1.DevBox) map[string]string {
	out := map[string]string{}
	if box.Spec.Resources.GPUs > 0 && box.Spec.Resources.GPUModel != "" {
		out[DefaultGPUNodeLabel] = box.Spec.Resources.GPUModel
	}
	if box.Spec.Zone != "" {
		out[ZoneLabel] = box.Spec.Zone
	}
	maps.Copy(out, box.Spec.NodeSelector)
	if len(out) == 0 {
		return nil
	}
	return out
}

// specHash fingerprints everything the pod template depends on. A change that
// cannot be applied in place replaces the pod; the root disk survives.
func specHash(box *devpilotv1alpha1.DevBox, pvcName string, info ImageInfo) (string, error) {
	payload := struct {
		Image       string
		Digest      string
		Entrypoint  []string
		Cmd         []string
		Command     []string
		Args        []string
		Env         []corev1.EnvVar
		Resources   devpilotv1alpha1.DevBoxResources
		Volumes     []devpilotv1alpha1.DevBoxVolume
		NodeSelect  map[string]string
		Zone        string
		Tolerations []corev1.Toleration
		InitImage   string
		PVC         string
		// SSH only records whether the server runs, never the keys themselves:
		// authorized_keys is read from the mounted secret per connection, so
		// editing the list must not replace a running pod.
		SSH bool
		// Jupyter records whether the server runs and on which port. The port is
		// a flag devpilot-init reads once, so changing it replaces the pod; the
		// token is read from the mounted secret, so a new one does not.
		Jupyter     bool
		JupyterPort int32
	}{
		// ImageRef rather than spec.image: a pending rebase starts the pod from
		// the committed image before spec.image is updated to match.
		Image:       info.ImageRef,
		Digest:      info.Digest,
		Entrypoint:  info.Entrypoint,
		Cmd:         info.Cmd,
		Command:     box.Spec.Command,
		Args:        box.Spec.Args,
		Env:         box.Spec.Env,
		Resources:   box.Spec.Resources,
		Volumes:     box.Spec.Volumes,
		NodeSelect:  box.Spec.NodeSelector,
		Zone:        box.Spec.Zone,
		Tolerations: box.Spec.Tolerations,
		InitImage:   "",
		PVC:         pvcName,
		SSH:         sshEnabled(box),
		Jupyter:     jupyterEnabled(box),
		JupyterPort: jupyterPort(box),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal pod spec: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8]), nil
}

// epochNow is the timestamp recorded in action annotations.
func epochNow() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

// nowUTC is the single clock the controller reads, so tests can reason about
// status timestamps without stubbing time.Now everywhere.
func nowUTC() metav1.Time {
	return metav1.NewTime(time.Now().UTC())
}

// podHash reads the spec fingerprint recorded on an existing pod. A pod
// created before this controller version has none, which forces one replace.
func podHash(pod *corev1.Pod) string {
	if pod.Annotations == nil {
		return ""
	}
	return pod.Annotations[AnnotationSpecHash]
}
