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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// ConditionPreempted records that the platform took the devbox back. It is
	// separate from Ready so a console can show the cause without parsing the
	// message, and it stays True until the devbox runs again.
	ConditionPreempted = "Preempted"

	// outOfServiceTaint is what a cluster administrator or a cloud controller puts
	// on a node before evicting everything from it, which is how a preemptible
	// reclaim announces itself before the pods are actually deleted.
	outOfServiceTaint = "node.kubernetes.io/out-of-service"
)

// eventf records an Event when the manager gave the reconciler a recorder. The
// envtest suite builds reconcilers by hand, so the recorder can be nil.
func (r *DevBoxReconciler) eventf(obj runtime.Object, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(obj, nil, eventType, reason, reason, message)
}

// preempted reports whether the pod itself says the platform took its node
// back. A node drained for maintenance shows up as an out-of-service condition
// on the pod, and a reclaim usually shows up as an eviction.
func preempted(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	if pod.Status.Reason == "Evicted" || pod.Status.Reason == "Preempted" {
		return true
	}
	return pod.Status.Phase == corev1.PodFailed &&
		strings.Contains(strings.ToLower(pod.Status.Message), "out of service")
}

// nodeOutOfService reports whether an administrator or a cloud controller has
// tainted the node for eviction, which is how a reclaim announces itself before
// the pods are actually deleted.
func nodeOutOfService(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == outOfServiceTaint && taint.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

// reclaim reports whether the platform took the node back from this
// preemptible devbox, and the message to record when it did. Only a devbox
// that believes it is on a node is a candidate: once it is interrupted its
// node is cleared, which is what keeps a later start from being interrupted by
// the node it already left. A non-preemptible devbox is never interrupted; a
// lost pod fails instead.
func (r *DevBoxReconciler) reclaim(ctx context.Context, box *devpilotv1alpha1.DevBox, pod *corev1.Pod) (string, bool) {
	if !box.Spec.Preemptible {
		return "", false
	}
	switch box.Status.Phase {
	case devpilotv1alpha1.PhaseRunning, devpilotv1alpha1.PhaseStarting:
	default:
		return "", false
	}
	if preempted(pod) {
		return "The preemptible node was reclaimed; the root disk is retained", true
	}
	// The pod can vanish before the controller sees why, so the node in status
	// is the remaining evidence. A missing node reads as nothing to do: the
	// controller was not granted enough access to tell, and guessing would
	// interrupt devboxes on every run.
	if box.Status.NodeName == "" {
		return "", false
	}
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: box.Status.NodeName}, node); err != nil {
		return "", false
	}
	if nodeOutOfService(node) {
		return "Node " + box.Status.NodeName + " was taken out of service; the root disk is retained", true
	}
	return "", false
}

// markInterrupted moves the devbox to Interrupted and records why. The root
// disk is untouched: an interrupted devbox is one the platform took, not one
// the user released, so it restarts on the same overlay.
func (r *DevBoxReconciler) markInterrupted(box *devpilotv1alpha1.DevBox, reason, message string) {
	now := nowUTC()
	box.Status.Phase = devpilotv1alpha1.PhaseInterrupted
	box.Status.StartingStep = devpilotv1alpha1.StartingStepNone
	box.Status.InterruptedAt = &now
	if box.Status.NonRunningSince == nil {
		box.Status.NonRunningSince = &now
	}
	// The devbox occupies no node while interrupted; where the last segment ran
	// is already in the ledger.
	box.Status.NodeName = ""
	box.Status.PodName = ""
	box.Status.PrepulledNode = ""
	box.Status.ImageCacheState = devpilotv1alpha1.ImageCacheStateNone
	box.Status.Message = message
	r.setCondition(box, ConditionReady, metav1ConditionFalse, reason, message)
	r.setCondition(box, ConditionPreempted, metav1ConditionTrue, reason, message)
	r.eventf(box, corev1.EventTypeWarning, "Interrupted", message)
}

// resumeInterrupted lets the start and restart actions take a devbox out of the
// interrupted resting state. The phase becomes Pending and the next reconcile
// schedules a pod on whatever node the scheduler finds; if the preemptible pool
// has no capacity the devbox stays in Starting and reports why, rather than
// silently falling back to a node that was taken away.
func (r *DevBoxReconciler) resumeInterrupted(box *devpilotv1alpha1.DevBox) {
	if box.Status.Phase != devpilotv1alpha1.PhaseInterrupted {
		return
	}
	box.Status.Phase = devpilotv1alpha1.PhasePending
	box.Status.Message = "Restarting after an interruption"
	r.clearCondition(box, ConditionPreempted)
}
