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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// PrewarmFinalizer ensures owned zero-GPU pre-warm Pods are cleaned up
	// cleanly even in environments without a garbage collector controller.
	PrewarmFinalizer = "devpilot.wutz.dev/imageprewarm"

	defaultPrewarmMaxConcurrency int32 = 5
	prewarmSentinelCPU                 = "5m"
	prewarmSentinelMemory              = "8Mi"
	prewarmNoopCPU                     = "50m"
	prewarmNoopMemory                  = "64Mi"
)

// ImagePrewarmReconciler reconciles an ImagePrewarm object.
type ImagePrewarmReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Config   Config
	Images   ImageResolver
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=devpilot.wutz.dev,resources=imageprewarms,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=devpilot.wutz.dev,resources=imageprewarms/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=devpilot.wutz.dev,resources=imageprewarms/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile drives an ImagePrewarm rollout across all matching cluster nodes.
func (r *ImagePrewarmReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	ipw := &devpilotv1alpha1.ImagePrewarm{}
	if err := r.Get(ctx, req.NamespacedName, ipw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ipw.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, ipw)
	}
	if !controllerutil.ContainsFinalizer(ipw, PrewarmFinalizer) {
		if err := r.patchFinalizer(ctx, ipw, true); err != nil {
			return ctrl.Result{}, fmt.Errorf("add imageprewarm finalizer: %w", err)
		}
	}
	if r.Images == nil {
		r.Images = RegistryResolver{}
	}

	original := ipw.DeepCopy()
	result, expired, err := r.reconcilePrewarm(ctx, ipw)
	if expired {
		log.Info("Deleting expired ImagePrewarm", "imageprewarm", ipw.Name)
		if delErr := r.Delete(ctx, ipw); delErr != nil && !isNotFound(delErr) {
			return ctrl.Result{}, fmt.Errorf("delete expired imageprewarm %s: %w", ipw.Name, delErr)
		}
		return ctrl.Result{}, nil
	}
	if statusErr := r.patchStatus(ctx, original, ipw); statusErr != nil {
		return result, statusErr
	}
	return result, err
}

func (r *ImagePrewarmReconciler) reconcilePrewarm(
	ctx context.Context, ipw *devpilotv1alpha1.ImagePrewarm,
) (ctrl.Result, bool, error) {
	if r.Config.InitImage == "" {
		err := errors.New("devpilot init image must be set")
		r.markDegraded(ipw, "InitImageMissing", err.Error())
		return ctrl.Result{}, false, err
	}

	if err := r.ensureResolvedDigest(ctx, ipw); err != nil {
		r.markDegraded(ipw, "ImageResolutionFailed", err.Error())
		r.eventf(ipw, corev1.EventTypeWarning, "ImageResolutionFailed", err.Error())
		return ctrl.Result{}, false, err
	}

	prewarmNS := r.Config.PrewarmNamespaceOrDefault()
	eligible, podsByNode, err := r.collectNodesAndPods(ctx, ipw, prewarmNS)
	if err != nil {
		return ctrl.Result{}, false, err
	}

	targetImage := pinnedImageRef(ipw.Spec.Image, ipw.Status.ResolvedDigest)
	counts, pendingNodes, err := r.evaluateNodes(ctx, ipw, eligible, podsByNode, targetImage)
	if err != nil {
		return ctrl.Result{}, false, err
	}

	counts.inProgress, err = r.launchPrewarmPods(ctx, ipw, prewarmNS, pendingNodes, targetImage, counts.inProgress)
	if err != nil {
		return ctrl.Result{}, false, err
	}

	return r.updateRolloutStatus(ipw, int32(len(eligible)), counts)
}

type prewarmCounts struct {
	warmed     int32
	inProgress int32
	failed     int32
}

func (r *ImagePrewarmReconciler) ensureResolvedDigest(ctx context.Context, ipw *devpilotv1alpha1.ImagePrewarm) error {
	if ipw.Status.ResolvedDigest == "" || ipw.Status.ObservedGeneration != ipw.Generation {
		info, err := r.Images.Resolve(ctx, ipw.Spec.Image)
		if err != nil {
			return err
		}
		ipw.Status.ResolvedDigest = info.Digest
	}
	ipw.Status.ObservedGeneration = ipw.Generation
	return nil
}

func (r *ImagePrewarmReconciler) collectNodesAndPods(
	ctx context.Context, ipw *devpilotv1alpha1.ImagePrewarm, prewarmNS string,
) ([]*corev1.Node, map[string]*corev1.Pod, error) {
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return nil, nil, fmt.Errorf("list nodes: %w", err)
	}
	eligible := make([]*corev1.Node, 0, len(nodes.Items))
	eligibleSet := make(map[string]struct{}, len(nodes.Items))
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if nodeMatches(node, ipw.Spec.NodeSelector, ipw.Spec.Tolerations) {
			eligible = append(eligible, node)
			eligibleSet[node.Name] = struct{}{}
		}
	}
	slices.SortFunc(eligible, func(a, b *corev1.Node) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(prewarmNS),
		client.MatchingLabels{LabelImagePrewarm: ipw.Name},
	); err != nil {
		return nil, nil, fmt.Errorf("list prewarm pods: %w", err)
	}

	podsByNode := make(map[string]*corev1.Pod, len(podList.Items))
	for i := range podList.Items {
		pod := &podList.Items[i]
		nodeName := prewarmPodNode(pod)
		if _, ok := eligibleSet[nodeName]; !ok || nodeName == "" {
			if err := r.deletePodIfActive(ctx, pod, "delete orphan prewarm pod"); err != nil {
				return nil, nil, err
			}
			continue
		}
		podsByNode[nodeName] = pod
	}
	return eligible, podsByNode, nil
}

func (r *ImagePrewarmReconciler) evaluateNodes(
	ctx context.Context,
	ipw *devpilotv1alpha1.ImagePrewarm,
	eligible []*corev1.Node,
	podsByNode map[string]*corev1.Pod,
	targetImage string,
) (prewarmCounts, []*corev1.Node, error) {
	var counts prewarmCounts
	var pendingNodes []*corev1.Node

	for _, node := range eligible {
		pod := podsByNode[node.Name]
		if pod != nil && !podMatchesImage(pod, targetImage) {
			if err := r.deletePodIfActive(ctx, pod, "replace outdated prewarm pod"); err != nil {
				return counts, nil, err
			}
			counts.inProgress++
			continue
		}

		if ipw.Spec.PinImage {
			evaluatePinnedNode(node, pod, &counts, &pendingNodes)
			continue
		}

		cached := nodeHasImage(node, ipw.Spec.Image, ipw.Status.ResolvedDigest)
		if err := r.evaluateNoopNode(ctx, node, pod, cached, &counts, &pendingNodes); err != nil {
			return counts, nil, err
		}
	}
	return counts, pendingNodes, nil
}

func evaluatePinnedNode(node *corev1.Node, pod *corev1.Pod, counts *prewarmCounts, pendingNodes *[]*corev1.Node) {
	switch {
	case pod == nil:
		*pendingNodes = append(*pendingNodes, node)
	case pod.Status.Phase == corev1.PodFailed:
		counts.failed++
	case pod.Status.Phase == corev1.PodRunning:
		counts.warmed++
	default:
		counts.inProgress++
	}
}

func (r *ImagePrewarmReconciler) evaluateNoopNode(
	ctx context.Context,
	node *corev1.Node,
	pod *corev1.Pod,
	cached bool,
	counts *prewarmCounts,
	pendingNodes *[]*corev1.Node,
) error {
	switch {
	case cached:
		counts.warmed++
		if pod != nil && pod.Status.Phase == corev1.PodSucceeded {
			return r.deletePodIfActive(ctx, pod, "clean up completed prewarm pod")
		}
	case pod != nil && pod.Status.Phase == corev1.PodSucceeded:
		counts.warmed++
	case pod != nil && pod.Status.Phase == corev1.PodFailed:
		counts.failed++
	case pod != nil:
		counts.inProgress++
	default:
		*pendingNodes = append(*pendingNodes, node)
	}
	return nil
}

func (r *ImagePrewarmReconciler) deletePodIfActive(ctx context.Context, pod *corev1.Pod, action string) error {
	if !pod.DeletionTimestamp.IsZero() {
		return nil
	}
	if err := r.Delete(ctx, pod, client.PropagationPolicy(metav1DeleteBackground)); err != nil && !isNotFound(err) {
		return fmt.Errorf("%s %s: %w", action, pod.Name, err)
	}
	return nil
}

func (r *ImagePrewarmReconciler) launchPrewarmPods(
	ctx context.Context,
	ipw *devpilotv1alpha1.ImagePrewarm,
	prewarmNS string,
	pendingNodes []*corev1.Node,
	targetImage string,
	inProgress int32,
) (int32, error) {
	log := logf.FromContext(ctx)
	maxConc := ipw.Spec.MaxConcurrency
	if maxConc <= 0 {
		maxConc = defaultPrewarmMaxConcurrency
	}
	for _, node := range pendingNodes {
		if inProgress >= maxConc {
			break
		}
		pod, err := r.buildPrewarmPod(ipw, prewarmNS, node.Name, targetImage)
		if err != nil {
			r.markDegraded(ipw, "PodSpecInvalid", err.Error())
			return inProgress, err
		}
		log.Info("Creating ImagePrewarm pod", "pod", pod.Name, "node", node.Name, "image", targetImage, "pin", ipw.Spec.PinImage)
		if err := r.Create(ctx, pod); err != nil {
			if isAlreadyExists(err) {
				inProgress++
				continue
			}
			r.markDegraded(ipw, "PodCreateFailed", err.Error())
			return inProgress, fmt.Errorf("create prewarm pod %s: %w", pod.Name, err)
		}
		inProgress++
	}
	return inProgress, nil
}

func (r *ImagePrewarmReconciler) updateRolloutStatus(
	ipw *devpilotv1alpha1.ImagePrewarm, desired int32, counts prewarmCounts,
) (ctrl.Result, bool, error) {
	ipw.Status.DesiredNodes = desired
	ipw.Status.WarmedNodes = counts.warmed
	ipw.Status.InProgressNodes = counts.inProgress
	ipw.Status.FailedNodes = counts.failed

	switch {
	case counts.failed > 0:
		msg := fmt.Sprintf("%d/%d nodes failed to prewarm; %d/%d warmed", counts.failed, desired, counts.warmed, desired)
		r.markDegraded(ipw, "PrewarmFailed", msg)
		return ctrl.Result{}, false, nil
	case desired == 0:
		ipw.Status.Phase = devpilotv1alpha1.ImagePrewarmPhasePending
		ipw.Status.CompletedAt = nil
		ipw.Status.Message = "Waiting for eligible nodes matching selector and tolerations"
		r.setCondition(ipw, metav1.ConditionFalse, "NoEligibleNodes", ipw.Status.Message)
		return ctrl.Result{}, false, nil
	case counts.warmed == desired && counts.inProgress == 0:
		ipw.Status.Phase = devpilotv1alpha1.ImagePrewarmPhaseCompleted
		ipw.Status.Message = fmt.Sprintf("Warmed %d/%d nodes", counts.warmed, desired)
		if ipw.Status.CompletedAt == nil {
			now := nowUTC()
			ipw.Status.CompletedAt = &now
			r.eventf(ipw, corev1.EventTypeNormal, "Completed", ipw.Status.Message)
		}
		r.setCondition(ipw, metav1.ConditionTrue, "Completed", ipw.Status.Message)
		if ipw.Spec.TTLHoursAfterComplete > 0 && ipw.Status.CompletedAt != nil {
			expiresAt := ipw.Status.CompletedAt.Add(time.Duration(ipw.Spec.TTLHoursAfterComplete) * time.Hour)
			remaining := expiresAt.Sub(timeNow())
			if remaining <= 0 {
				return ctrl.Result{}, true, nil
			}
			return ctrl.Result{RequeueAfter: remaining}, false, nil
		}
		return ctrl.Result{}, false, nil
	default:
		ipw.Status.Phase = devpilotv1alpha1.ImagePrewarmPhaseRunning
		ipw.Status.CompletedAt = nil
		ipw.Status.Message = fmt.Sprintf("Warming %d/%d nodes (%d in progress)", counts.warmed, desired, counts.inProgress)
		r.setCondition(ipw, metav1.ConditionFalse, "Warming", ipw.Status.Message)
		return ctrl.Result{}, false, nil
	}
}

func (r *ImagePrewarmReconciler) buildPrewarmPod(
	ipw *devpilotv1alpha1.ImagePrewarm, namespace, nodeName, imageRef string,
) (*corev1.Pod, error) {
	modeFlag := "--noop"
	restartPolicy := corev1.RestartPolicyNever
	cpuReq := apiresource.MustParse(prewarmNoopCPU)
	memReq := apiresource.MustParse(prewarmNoopMemory)
	if ipw.Spec.PinImage {
		modeFlag = "--hold"
		restartPolicy = corev1.RestartPolicyAlways
		cpuReq = apiresource.MustParse(prewarmSentinelCPU)
		memReq = apiresource.MustParse(prewarmSentinelMemory)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      prewarmPodName(ipw.Name, nodeName),
			Namespace: namespace,
			Labels: map[string]string{
				LabelManagedBy:    ValueManagedBy,
				LabelImagePrewarm: ipw.Name,
				LabelPodRole:      PodRolePrewarm,
			},
			Annotations: map[string]string{
				AnnotationPrewarmNode: nodeName,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:                     nodeName,
			RestartPolicy:                restartPolicy,
			AutomountServiceAccountToken: new(false),
			EnableServiceLinks:           new(false),
			Tolerations:                  ipw.Spec.Tolerations,
			ImagePullSecrets:             ipw.Spec.ImagePullSecrets,
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			InitContainers: []corev1.Container{initContainer(r.Config.InitImage)},
			Containers: []corev1.Container{
				{
					Name:            "prewarm",
					Image:           imageRef,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{InitBinaryPath},
					Args:            []string{modeFlag},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    cpuReq,
							corev1.ResourceMemory: memReq,
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    cpuReq,
							corev1.ResourceMemory: memReq,
						},
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: new(false),
						ReadOnlyRootFilesystem:   new(true),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{capabilityAll},
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: InitVolumeName, MountPath: InitMountPath},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name:         InitVolumeName,
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(ipw, pod, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference on prewarm pod: %w", err)
	}
	return pod, nil
}

func prewarmPodName(prewarmName, nodeName string) string {
	raw := prewarmName + "-" + nodeName
	if len(raw) <= 63 {
		return raw
	}
	sum := sha256.Sum256([]byte(raw))
	suffix := "-" + hex.EncodeToString(sum[:4])
	prefixLen := 63 - len(suffix)
	return raw[:prefixLen] + suffix
}

func prewarmPodNode(pod *corev1.Pod) string {
	if pod.Spec.NodeName != "" {
		return pod.Spec.NodeName
	}
	if pod.Annotations != nil {
		return pod.Annotations[AnnotationPrewarmNode]
	}
	return ""
}

func podMatchesImage(pod *corev1.Pod, targetImage string) bool {
	if len(pod.Spec.Containers) == 0 {
		return false
	}
	return pod.Spec.Containers[0].Image == targetImage
}

func (r *ImagePrewarmReconciler) markDegraded(ipw *devpilotv1alpha1.ImagePrewarm, reason, message string) {
	ipw.Status.Phase = devpilotv1alpha1.ImagePrewarmPhaseDegraded
	ipw.Status.CompletedAt = nil
	ipw.Status.Message = message
	r.setCondition(ipw, metav1.ConditionFalse, reason, message)
}

func (r *ImagePrewarmReconciler) setCondition(
	ipw *devpilotv1alpha1.ImagePrewarm, status metav1.ConditionStatus, reason, message string,
) {
	now := metav1.NewTime(timeNow())
	for i := range ipw.Status.Conditions {
		cond := &ipw.Status.Conditions[i]
		if cond.Type != ConditionReady {
			continue
		}
		if cond.Status != status {
			cond.LastTransitionTime = now
		}
		cond.Status = status
		cond.Reason = reason
		cond.Message = message
		cond.ObservedGeneration = ipw.Generation
		return
	}
	ipw.Status.Conditions = append(ipw.Status.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: ipw.Generation,
		LastTransitionTime: now,
	})
}

func (r *ImagePrewarmReconciler) eventf(obj runtime.Object, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(obj, nil, eventType, reason, reason, message)
}

func (r *ImagePrewarmReconciler) patchStatus(
	ctx context.Context, before, after *devpilotv1alpha1.ImagePrewarm,
) error {
	base := after.DeepCopy()
	base.Status = before.Status
	if err := r.Status().Patch(ctx, after, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch ImagePrewarm status: %w", err)
	}
	return nil
}

func (r *ImagePrewarmReconciler) patchFinalizer(
	ctx context.Context, ipw *devpilotv1alpha1.ImagePrewarm, add bool,
) error {
	patched := ipw.DeepCopy()
	if add {
		controllerutil.AddFinalizer(patched, PrewarmFinalizer)
	} else {
		controllerutil.RemoveFinalizer(patched, PrewarmFinalizer)
	}
	if err := r.Patch(ctx, patched, client.MergeFrom(ipw)); err != nil {
		return err
	}
	*ipw = *patched
	return nil
}

func (r *ImagePrewarmReconciler) finalize(ctx context.Context, ipw *devpilotv1alpha1.ImagePrewarm) error {
	if !controllerutil.ContainsFinalizer(ipw, PrewarmFinalizer) {
		return nil
	}
	prewarmNS := r.Config.PrewarmNamespaceOrDefault()
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(prewarmNS),
		client.MatchingLabels{LabelImagePrewarm: ipw.Name},
	); err != nil {
		return fmt.Errorf("list prewarm pods for cleanup: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, pod, client.PropagationPolicy(metav1DeleteBackground)); err != nil && !isNotFound(err) {
				return fmt.Errorf("delete prewarm pod %s: %w", pod.Name, err)
			}
		}
	}
	if err := r.patchFinalizer(ctx, ipw, false); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove imageprewarm finalizer: %w", err)
	}
	return nil
}

func (r *ImagePrewarmReconciler) requestsForNode(ctx context.Context, obj client.Object) []reconcile.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}
	list := &devpilotv1alpha1.ImagePrewarmList{}
	if err := r.List(ctx, list); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		ipw := &list.Items[i]
		if ipw.Status.DesiredNodes > 0 || nodeMatches(node, ipw.Spec.NodeSelector, ipw.Spec.Tolerations) {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: ipw.Name}})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImagePrewarmReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("imageprewarm")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&devpilotv1alpha1.ImagePrewarm{}).
		Owns(&corev1.Pod{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.requestsForNode)).
		Named("imageprewarm").
		Complete(r)
}
