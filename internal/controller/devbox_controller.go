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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

// DevBoxReconciler reconciles a DevBox object.
type DevBoxReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config Config
	// Images resolves base image digests and configs; nil means
	// RegistryResolver, and tests inject a fake to stay offline.
	Images ImageResolver
}

// +kubebuilder:rbac:groups=devpilot.wutz.dev,resources=devboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=devpilot.wutz.dev,resources=devboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=devpilot.wutz.dev,resources=devboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one DevBox toward its desired state: spec.desiredState
// selects between the running and stopped halves of the state machine, and
// the action annotation is consumed before either.
func (r *DevBoxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	box := &devpilotv1alpha1.DevBox{}
	if err := r.Get(ctx, req.NamespacedName, box); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !box.DeletionTimestamp.IsZero() {
		// The pod and the claim carry owner references, so garbage collection
		// releases the disk without help from the controller.
		return ctrl.Result{}, nil
	}
	if r.Images == nil {
		r.Images = RegistryResolver{}
	}

	original := box.DeepCopy()

	if handled, err := r.applyAction(ctx, box); err != nil {
		box.Status.Phase = devpilotv1alpha1.PhaseFailed
		box.Status.Message = err.Error()
		return ctrl.Result{}, r.patchStatus(ctx, original, box)
	} else if handled {
		return ctrl.Result{}, r.patchStatus(ctx, original, box)
	}

	result, err := r.reconcileLifecycle(ctx, box)
	log.V(1).Info("Reconciled DevBox", "phase", box.Status.Phase, "desiredState", box.Spec.DesiredState)
	if statusErr := r.patchStatus(ctx, original, box); statusErr != nil {
		return result, statusErr
	}
	return result, err
}

// reconcileLifecycle ensures the root disk claim, then drives the pod.
func (r *DevBoxReconciler) reconcileLifecycle(ctx context.Context, box *devpilotv1alpha1.DevBox) (ctrl.Result, error) {
	pvc, err := r.ensureRootDisk(ctx, box)
	if err != nil {
		r.setFailed(ctx, box, "RootDiskFailed", err)
		return ctrl.Result{}, err
	}
	if box.Status.RootDiskPVC != pvc.Name {
		box.Status.RootDiskPVC = pvc.Name
	}

	pod, err := r.currentPod(ctx, box)
	if err != nil {
		return ctrl.Result{}, err
	}

	if box.Spec.DesiredState == devpilotv1alpha1.DesiredStateStopped {
		return r.reconcileStopped(ctx, box, pod)
	}
	return r.reconcileRunning(ctx, box, pvc, pod)
}

// reconcileStopped deletes the pod and lands in Stopped once it is gone.
func (r *DevBoxReconciler) reconcileStopped(ctx context.Context, box *devpilotv1alpha1.DevBox, pod *corev1.Pod) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if pod == nil {
		if box.Status.Phase != devpilotv1alpha1.PhaseStopped {
			now := nowUTC()
			box.Status.Phase = devpilotv1alpha1.PhaseStopped
			box.Status.StartingStep = devpilotv1alpha1.StartingStepNone
			box.Status.PodName = ""
			box.Status.NodeName = ""
			box.Status.StoppedAt = &now
			if box.Status.NonRunningSince == nil {
				box.Status.NonRunningSince = &now
			}
			box.Status.Message = "Stopped; the GPU is released and the root disk is retained"
			r.setCondition(box, ConditionReady, metav1ConditionFalse, "Stopped", box.Status.Message)
			log.Info("DevBox stopped", "rootDiskPVC", box.Status.RootDiskPVC)
		}
		return ctrl.Result{}, nil
	}

	box.Status.Phase = devpilotv1alpha1.PhaseStopping
	box.Status.Message = "Waiting for the pod to terminate"
	if !pod.DeletionTimestamp.IsZero() {
		return ctrl.Result{RequeueAfter: requeueWhileStarting}, nil
	}
	log.Info("Deleting DevBox pod", "pod", pod.Name)
	if err := r.Delete(ctx, pod, client.PropagationPolicy(metav1DeleteBackground)); err != nil && !isNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete pod %s: %w", pod.Name, err)
	}
	return ctrl.Result{RequeueAfter: requeueWhileStarting}, nil
}

// reconcileRunning starts the pod or reflects its progress into status.
func (r *DevBoxReconciler) reconcileRunning(ctx context.Context, box *devpilotv1alpha1.DevBox, pvc *corev1.PersistentVolumeClaim, pod *corev1.Pod) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if pvc.Status.Phase != corev1.ClaimBound {
		box.Status.Phase = devpilotv1alpha1.PhasePending
		box.Status.StartingStep = devpilotv1alpha1.StartingStepAllocatingResources
		box.Status.Message = "Waiting for the root disk claim to be bound"
		r.setCondition(box, ConditionRootDiskReady, metav1ConditionFalse, "Provisioning", box.Status.Message)
		return ctrl.Result{RequeueAfter: requeueWhileStarting}, nil
	}
	r.setCondition(box, ConditionRootDiskReady, metav1ConditionTrue, "Bound", "The root disk claim is bound")

	info, err := r.resolveImage(ctx, box)
	if err != nil {
		r.setFailed(ctx, box, "ImageResolutionFailed", err)
		return ctrl.Result{}, err
	}

	// A pending rebase switches the base image and clears upper/ on this start,
	// so the pod runs devpilot-init with --rebase. The flag is derived from
	// status on every reconcile, never from spec, so it survives a failed
	// image pull without ever landing in the spec hash.
	rebase := box.Status.PendingRebaseImage != ""

	desired, err := r.buildPod(box, pvc, info, rebase)
	if err != nil {
		r.setFailed(ctx, box, "PodSpecInvalid", err)
		return ctrl.Result{}, err
	}

	if pod == nil {
		log.Info("Creating DevBox pod", "pod", desired.Name)
		if err := r.Create(ctx, desired); err != nil && !isAlreadyExists(err) {
			r.setFailed(ctx, box, "PodCreateFailed", err)
			return ctrl.Result{}, fmt.Errorf("create pod %s: %w", desired.Name, err)
		}
		box.Status.PodName = desired.Name
		box.Status.Phase = devpilotv1alpha1.PhaseStarting
		box.Status.StartingStep = devpilotv1alpha1.StartingStepPullingImage
		box.Status.Message = "Pulling the base image"
		return ctrl.Result{RequeueAfter: requeueWhileStarting}, nil
	}

	if podHash(pod) != desired.Annotations[AnnotationSpecHash] {
		box.Status.Phase = devpilotv1alpha1.PhaseStarting
		box.Status.Message = "Restarting the pod to apply the new spec"
		if pod.DeletionTimestamp.IsZero() {
			log.Info("Replacing outdated DevBox pod", "pod", pod.Name)
			if err := r.Delete(ctx, pod, client.PropagationPolicy(metav1DeleteBackground)); err != nil && !isNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete outdated pod %s: %w", pod.Name, err)
			}
		}
		return ctrl.Result{RequeueAfter: requeueWhileStarting}, nil
	}

	return r.observePod(ctx, box, pod, info)
}

// observePod maps the pod's state into the DevBox phase.
func (r *DevBoxReconciler) observePod(ctx context.Context, box *devpilotv1alpha1.DevBox, pod *corev1.Pod, info ImageInfo) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	box.Status.PodName = pod.Name
	if pod.Spec.NodeName != "" {
		box.Status.NodeName = pod.Spec.NodeName
	}

	switch pod.Status.Phase {
	case corev1.PodRunning:
		// devpilot-init cleared upper/ before this container started, so the
		// root disk now belongs to the committed image. The spec moves to it
		// in the same reconcile the pin does, so a recycled pod can never run
		// --rebase twice for the same commit.
		if box.Status.PendingRebaseImage != "" {
			if err := r.consumeRebase(ctx, box, info); err != nil {
				return ctrl.Result{}, err
			}
		}
		if !podReady(pod) {
			box.Status.Phase = devpilotv1alpha1.PhaseStarting
			box.Status.StartingStep = devpilotv1alpha1.StartingStepStartingContainers
			box.Status.Message = "Starting the devpilot-init supervisor"
			return ctrl.Result{RequeueAfter: requeueWhileStarting}, nil
		}
		box.Status.Phase = devpilotv1alpha1.PhaseRunning
		box.Status.StartingStep = devpilotv1alpha1.StartingStepNone
		box.Status.Message = ""
		box.Status.StoppedAt = nil
		box.Status.NonRunningSince = nil
		box.Status.ScheduledReleaseTime = nil
		box.Status.InterruptedAt = nil
		r.setCondition(box, ConditionReady, metav1ConditionTrue, "PodReady", "The devbox is running on "+pod.Spec.NodeName)
		log.Info("DevBox running", "pod", pod.Name, "node", pod.Spec.NodeName)
		return ctrl.Result{}, nil
	case corev1.PodFailed:
		r.setFailed(ctx, box, "PodFailed", fmt.Errorf("%s", podFailureMessage(pod)))
		return ctrl.Result{}, nil
	case corev1.PodSucceeded:
		r.setFailed(ctx, box, "PodExited",
			fmt.Errorf("the devbox command exited successfully; set spec.command to a long running process"))
		return ctrl.Result{}, nil
	case corev1.PodPending, corev1.PodUnknown:
	}

	step, message := pendingStep(pod)
	box.Status.Phase = devpilotv1alpha1.PhaseStarting
	box.Status.StartingStep = step
	box.Status.Message = message
	return ctrl.Result{RequeueAfter: requeueWhileStarting}, nil
}

// resolveImage resolves the base image once and refuses to continue when a tag
// now moves: the root disk was built on the pinned lower layer.
//
// A pending rebase is the sanctioned way past that: the next start runs on the
// committed image, clears upper/ and adopts its digest. The pin is not moved
// here, only once the pod that runs with --rebase is up.
func (r *DevBoxReconciler) resolveImage(ctx context.Context, box *devpilotv1alpha1.DevBox) (ImageInfo, error) {
	if pending := box.Status.PendingRebaseImage; pending != "" {
		return r.Images.Resolve(ctx, pending)
	}
	info, err := r.Images.Resolve(ctx, box.Spec.Image)
	if err != nil {
		return ImageInfo{}, err
	}
	if box.Status.ActiveImageDigest == "" {
		box.Status.ActiveImageDigest = info.Digest
		box.Status.ActiveImage = box.Spec.Image
		return info, nil
	}
	if box.Status.ActiveImageDigest != info.Digest {
		return ImageInfo{}, fmt.Errorf("image %q now resolves to %s but the root disk was created from %s; "+
			"commit it or reset the root disk before switching base images",
			box.Spec.Image, info.Digest, box.Status.ActiveImageDigest)
	}
	return info, nil
}

// consumeRebase adopts the pending image once the pod that ran with --rebase is
// up. Clearing upper/ is destructive, so it happens exactly once: the pending
// field and the spec image move together, and the status patch that follows
// makes the two writes durable in one reconcile.
func (r *DevBoxReconciler) consumeRebase(ctx context.Context, box *devpilotv1alpha1.DevBox, info ImageInfo) error {
	pending := box.Status.PendingRebaseImage
	box.Spec.Image = pending
	// A spec write bumps the generation, so the old object is the right base
	// for the merge patch.
	if err := r.Update(ctx, box); err != nil {
		return fmt.Errorf("adopt the rebased base image %s: %w", pending, err)
	}
	box.Status.ActiveImage = info.ImageRef
	box.Status.ActiveImageDigest = info.Digest
	box.Status.RebaseCount++
	box.Status.PendingRebaseImage = ""
	logf.FromContext(ctx).Info("Rebased the root disk",
		"image", info.ImageRef, "digest", info.Digest, "rebaseCount", box.Status.RebaseCount)
	return nil
}

// currentPod returns the pod owned by the DevBox, or nil.
func (r *DevBoxReconciler) currentPod(ctx context.Context, box *devpilotv1alpha1.DevBox) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	name := types.NamespacedName{Namespace: box.Namespace, Name: box.Name + PodSuffix}
	if err := r.Get(ctx, name, pod); err != nil {
		return nil, client.IgnoreNotFound(fmt.Errorf("get pod %s: %w", name.Name, err))
	}
	return pod, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DevBoxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&devpilotv1alpha1.DevBox{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Named("devbox").
		Complete(r)
}
