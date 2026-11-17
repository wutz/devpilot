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
	"maps"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// PrepullPodSuffix is appended to the DevBox name for the GPU-free pre-pull pod.
	PrepullPodSuffix = "-prepull"

	// LabelImagePrewarm marks Pods created by an ImagePrewarm resource.
	LabelImagePrewarm = "devpilot.wutz.dev/imageprewarm"
	// LabelPodRole distinguishes auxiliary prewarm/prepull Pods from DevBox pods.
	LabelPodRole = "devpilot.wutz.dev/role"
	// PodRolePrepull marks two-phase DevBox pre-pull Pods.
	PodRolePrepull = "prepull"
	// PodRolePrewarm marks ImagePrewarm Pods.
	PodRolePrewarm = "prewarm"

	// AnnotationPrewarmNode records the target node on an ImagePrewarm Pod.
	AnnotationPrewarmNode = "devpilot.wutz.dev/prewarm-node"

	stickyNodeAffinityWeight    int32 = 80
	prepulledNodeAffinityWeight int32 = 100
)

type imageCacheSummary struct {
	AnyEligibleCached bool
	StickyNodeCached  bool
	AnyClusterCached  bool
	TargetNodeCached  bool
}

// stickyNodeAffinityEnabled reports whether soft affinity to status.lastNodeName is enabled.
func stickyNodeAffinityEnabled(box *devpilotv1alpha1.DevBox) bool {
	if box.Spec.Scheduling.StickyNodeAffinity == nil {
		return true
	}
	return *box.Spec.Scheduling.StickyNodeAffinity
}

// nodeAffinityForBox builds soft node affinity preferring the pre-pulled node
// (weight 100) or the previous node (weight 80) so containerd reuses cached layers.
func nodeAffinityForBox(box *devpilotv1alpha1.DevBox) *corev1.Affinity {
	var target string
	var weight int32
	switch {
	case box.Status.PrepulledNode != "":
		target = box.Status.PrepulledNode
		weight = prepulledNodeAffinityWeight
	case stickyNodeAffinityEnabled(box) && box.Status.LastNodeName != "":
		target = box.Status.LastNodeName
		weight = stickyNodeAffinityWeight
	default:
		return nil
	}
	return hostnameAffinity(target, weight)
}

func hostnameAffinity(nodeName string, weight int32) *corev1.Affinity {
	if nodeName == "" {
		return nil
	}
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
				{
					Weight: weight,
					Preference: corev1.NodeSelectorTerm{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      corev1.LabelHostname,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{nodeName},
							},
						},
					},
				},
			},
		},
	}
}

// nodeReady reports whether a node has Ready=True and is schedulable.
func nodeReady(node *corev1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// nodeMatches checks label selectors and taints/tolerations against a node.
func nodeMatches(node *corev1.Node, selector map[string]string, tolerations []corev1.Toleration) bool {
	if !nodeReady(node) {
		return false
	}
	for k, v := range selector {
		if node.Labels[k] != v {
			return false
		}
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if !tolerationsTolerate(tolerations, taint) {
			return false
		}
	}
	return true
}

func tolerationsTolerate(tolerations []corev1.Toleration, taint corev1.Taint) bool {
	for _, tol := range tolerations {
		if tol.Effect != "" && tol.Effect != taint.Effect {
			continue
		}
		if tol.Key == "" && tol.Operator == corev1.TolerationOpExists {
			return true
		}
		if tol.Key != taint.Key {
			continue
		}
		switch tol.Operator {
		case corev1.TolerationOpExists:
			return true
		case "", corev1.TolerationOpEqual:
			if tol.Value == taint.Value {
				return true
			}
		}
	}
	return false
}

// nodeHasImage checks whether node.Status.Images already caches imageRef or imageDigest.
func nodeHasImage(node *corev1.Node, imageRef, imageDigest string) bool {
	digestSuffix := ""
	if imageDigest != "" {
		if _, d, ok := strings.Cut(imageDigest, "@"); ok && d != "" {
			digestSuffix = "@" + d
		} else {
			digestSuffix = "@" + imageDigest
		}
	}
	for _, img := range node.Status.Images {
		for _, n := range img.Names {
			if imageRef != "" && n == imageRef {
				return true
			}
			if imageDigest != "" && n == imageDigest {
				return true
			}
			if digestSuffix != "" && strings.HasSuffix(n, digestSuffix) {
				return true
			}
		}
	}
	return false
}

// pinnedImageRef resolves imageRef to an immutable repo@digest reference when
// digest is available so P2P/Spegel peers and kubelet cache keys align exactly.
func pinnedImageRef(imageRef, digest string) string {
	if digest == "" {
		return imageRef
	}
	if strings.Contains(digest, "@") {
		return digest
	}
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return imageRef
	}
	return ref.Context().Digest(digest).String()
}

// inspectImageCache scans cluster nodes to classify cache locality for a DevBox.
func (r *DevBoxReconciler) inspectImageCache(
	ctx context.Context, box *devpilotv1alpha1.DevBox, info ImageInfo, scheduledNode string,
) (imageCacheSummary, error) {
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return imageCacheSummary{}, fmt.Errorf("list nodes for image cache check: %w", err)
	}
	selector := nodeSelectors(box)
	var summary imageCacheSummary
	for i := range nodes.Items {
		node := &nodes.Items[i]
		cached := nodeHasImage(node, info.ImageRef, info.Digest)
		if !cached {
			continue
		}
		if nodeReady(node) {
			summary.AnyClusterCached = true
		}
		if scheduledNode != "" && node.Name == scheduledNode {
			summary.TargetNodeCached = true
		}
		if nodeMatches(node, selector, box.Spec.Tolerations) {
			summary.AnyEligibleCached = true
			if box.Status.LastNodeName != "" && node.Name == box.Status.LastNodeName {
				summary.StickyNodeCached = true
			}
		}
	}
	return summary, nil
}

func classifyImageCacheState(
	box *devpilotv1alpha1.DevBox, pod *corev1.Pod, summary imageCacheSummary,
) devpilotv1alpha1.ImageCacheState {
	if pod != nil && pod.Spec.NodeName != "" {
		if summary.TargetNodeCached {
			return devpilotv1alpha1.ImageCacheStateCacheHit
		}
		if summary.AnyClusterCached {
			return devpilotv1alpha1.ImageCacheStateP2PPulling
		}
		return devpilotv1alpha1.ImageCacheStateColdPulling
	}
	if box.Status.PrepulledNode != "" || summary.StickyNodeCached {
		return devpilotv1alpha1.ImageCacheStateCacheHit
	}
	if summary.AnyClusterCached {
		return devpilotv1alpha1.ImageCacheStateP2PPulling
	}
	return devpilotv1alpha1.ImageCacheStateColdPulling
}

func (r *DevBoxReconciler) shouldTwoPhasePrepull(
	box *devpilotv1alpha1.DevBox, info ImageInfo, summary imageCacheSummary,
) bool {
	if box.Spec.Resources.GPUs <= 0 {
		return false
	}
	if box.Status.PrepulledNode != "" {
		return false
	}
	if summary.StickyNodeCached {
		return false
	}
	policy := box.Spec.Scheduling.Prepull
	if policy == "" {
		policy = devpilotv1alpha1.PrepullPolicyAuto
	}
	switch policy {
	case devpilotv1alpha1.PrepullPolicyNever:
		return false
	case devpilotv1alpha1.PrepullPolicyAlways:
		return !summary.AnyEligibleCached
	case devpilotv1alpha1.PrepullPolicyAuto:
		return r.Config.PrepullThresholdBytes > 0 &&
			info.TotalSizeBytes >= r.Config.PrepullThresholdBytes &&
			!summary.AnyEligibleCached
	default:
		return false
	}
}

func (r *DevBoxReconciler) currentPrepullPod(ctx context.Context, box *devpilotv1alpha1.DevBox) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Namespace: box.Namespace, Name: box.Name + PrepullPodSuffix}
	if err := r.Get(ctx, podKey, pod); err != nil {
		return nil, clientIgnoreNotFound(fmt.Errorf("get prepull pod %s: %w", podKey.Name, err), err)
	}
	return pod, nil
}

func clientIgnoreNotFound(wrapped, raw error) error {
	if isNotFound(raw) {
		return nil
	}
	return wrapped
}

// deletePrepullPod removes the temporary GPU-free pre-pull pod if present.
func (r *DevBoxReconciler) deletePrepullPod(ctx context.Context, box *devpilotv1alpha1.DevBox) error {
	podName := box.Status.PrepullPodName
	if podName == "" {
		podName = box.Name + PrepullPodSuffix
	}
	if err := r.deletePod(ctx, podName, box.Namespace); err != nil {
		return err
	}
	box.Status.PrepullPodName = ""
	return nil
}

// reconcilePrepull drives the GPU-free pre-pull phase before creating the GPU Pod.
// When done is true, the caller proceeds immediately to build/create the GPU Pod.
func (r *DevBoxReconciler) reconcilePrepull(
	ctx context.Context, box *devpilotv1alpha1.DevBox, info ImageInfo, summary imageCacheSummary,
) (bool, ctrl.Result, error) {
	log := logf.FromContext(ctx)

	prepullPod, err := r.currentPrepullPod(ctx, box)
	if err != nil {
		return false, ctrl.Result{}, err
	}

	if prepullPod == nil {
		if !r.shouldTwoPhasePrepull(box, info, summary) {
			return true, ctrl.Result{}, nil
		}
		desired, err := r.buildPrepullPod(box, info)
		if err != nil {
			r.setFailed(ctx, box, "PrepullSpecInvalid", err)
			return false, ctrl.Result{}, err
		}
		log.Info("Creating GPU-free prepull pod", "pod", desired.Name, "image", info.ImageRef, "sizeBytes", info.TotalSizeBytes)
		if err := r.Create(ctx, desired); err != nil && !isAlreadyExists(err) {
			r.setFailed(ctx, box, "PrepullCreateFailed", err)
			return false, ctrl.Result{}, fmt.Errorf("create prepull pod %s: %w", desired.Name, err)
		}
		box.Status.Phase = devpilotv1alpha1.PhaseStarting
		box.Status.StartingStep = devpilotv1alpha1.StartingStepPrepullingImage
		box.Status.ImageCacheState = devpilotv1alpha1.ImageCacheStatePrepulling
		box.Status.PrepullPodName = desired.Name
		box.Status.Message = "Pre-pulling image without locking GPUs"
		return false, ctrl.Result{RequeueAfter: requeueWhileStarting}, nil
	}

	switch prepullPod.Status.Phase {
	case corev1.PodSucceeded:
		if prepullPod.Spec.NodeName != "" {
			box.Status.PrepulledNode = prepullPod.Spec.NodeName
		}
		if err := r.deletePod(ctx, prepullPod.Name, prepullPod.Namespace); err != nil {
			return false, ctrl.Result{}, err
		}
		box.Status.PrepullPodName = ""
		log.Info("Prepull pod completed", "pod", prepullPod.Name, "node", box.Status.PrepulledNode)
		return true, ctrl.Result{}, nil
	case corev1.PodFailed:
		msg := podFailureMessage(prepullPod)
		_ = r.deletePod(ctx, prepullPod.Name, prepullPod.Namespace)
		box.Status.PrepullPodName = ""
		r.setFailed(ctx, box, "PrepullFailed", fmt.Errorf("%s", msg))
		return false, ctrl.Result{}, nil
	default:
		box.Status.Phase = devpilotv1alpha1.PhaseStarting
		box.Status.StartingStep = devpilotv1alpha1.StartingStepPrepullingImage
		box.Status.ImageCacheState = devpilotv1alpha1.ImageCacheStatePrepulling
		box.Status.PrepullPodName = prepullPod.Name
		box.Status.Message = "Pre-pulling image without locking GPUs"
		return false, ctrl.Result{RequeueAfter: requeueWhileStarting}, nil
	}
}

func (r *DevBoxReconciler) buildPrepullPod(box *devpilotv1alpha1.DevBox, info ImageInfo) (*corev1.Pod, error) {
	var affinity *corev1.Affinity
	if stickyNodeAffinityEnabled(box) && box.Status.LastNodeName != "" {
		affinity = hostnameAffinity(box.Status.LastNodeName, stickyNodeAffinityWeight)
	}
	selectors := maps.Clone(nodeSelectors(box))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      box.Name + PrepullPodSuffix,
			Namespace: box.Namespace,
			Labels: map[string]string{
				LabelManagedBy:  ValueManagedBy,
				LabelDevBoxName: box.Name,
				LabelPodRole:    PodRolePrepull,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: new(false),
			EnableServiceLinks:           new(false),
			SecurityContext: &corev1.PodSecurityContext{
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			InitContainers: []corev1.Container{initContainer(r.Config.InitImage)},
			Containers: []corev1.Container{
				{
					Name:            "prepull",
					Image:           pinnedImageRef(info.ImageRef, info.Digest),
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{InitBinaryPath},
					Args:            []string{"--noop"},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    apiresource.MustParse("50m"),
							corev1.ResourceMemory: apiresource.MustParse("64Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    apiresource.MustParse("50m"),
							corev1.ResourceMemory: apiresource.MustParse("64Mi"),
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
			NodeSelector: selectors,
			Tolerations:  box.Spec.Tolerations,
			Affinity:     affinity,
		},
	}
	if err := ctrlSetOwner(pod, box, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference on prepull pod: %w", err)
	}
	return pod, nil
}
