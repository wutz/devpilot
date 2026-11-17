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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

func createTestNode(name string, ready bool, labels map[string]string, taints []corev1.Taint, images []string) {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	nodeImages := make([]corev1.ContainerImage, 0, len(images))
	for _, img := range images {
		nodeImages = append(nodeImages, corev1.ContainerImage{Names: []string{img}, SizeBytes: 1 << 30})
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: corev1.NodeSpec{
			Taints: taints,
		},
	}
	Expect(k8sClient.Create(ctx, node)).To(Succeed())
	if ready {
		// kube-apiserver's TaintNodesByCondition plugin adds not-ready on Create.
		node.Spec.Taints = taints
		Expect(k8sClient.Update(ctx, node)).To(Succeed())
	}
	node.Status = corev1.NodeStatus{
		Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: status},
		},
		Images: nodeImages,
	}
	Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
	DeferCleanup(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}))).To(Succeed())
	})
}

func bindPodToNode(pod *corev1.Pod, nodeName string) {
	Expect(k8sClient.SubResource("binding").Create(ctx, pod, &corev1.Binding{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		Target:     corev1.ObjectReference{Kind: "Node", Name: nodeName},
	})).To(Succeed())
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
}

var _ = Describe("DevBox Sticky Affinity, Image Cache State, and Two-Phase Prepull", func() {
	var (
		namespace string
		resolver  *fakeResolver
		r         *DevBoxReconciler
	)

	BeforeEach(func() {
		specCounter++
		namespace = fmt.Sprintf("devbox-cache-%d-%d", GinkgoRandomSeed(), specCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		resolver = newTestResolver()
		r = newTestReconciler(resolver)
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))).To(Succeed())
	})

	startAndReachNode := func(box *devpilotv1alpha1.DevBox, nodeName string) types.NamespacedName {
		key := client.ObjectKeyFromObject(box)
		Expect(k8sClient.Create(ctx, box)).To(Succeed())

		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}, pod)).To(Succeed())
		bindPodToNode(pod, nodeName)
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		return key
	}

	It("injects sticky soft affinity to status.lastNodeName across stop/restart and clears it on reset-rootdisk", func() {
		nodeName := namespace + "-gpu-1"
		createTestNode(nodeName, true, map[string]string{
			corev1.LabelHostname: nodeName,
			DefaultGPUNodeLabel:  testGPUModelA100,
		}, nil, nil)

		box := newTestDevBox("sticky-box", namespace)
		key := startAndReachNode(box, nodeName)

		updated := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.LastNodeName).To(Equal(nodeName))
		Expect(updated.Status.ImageCacheState).To(Equal(devpilotv1alpha1.ImageCacheStateCacheHit))

		By("stopping the DevBox and verifying lastNodeName is preserved")
		updated.Spec.DesiredState = devpilotv1alpha1.DesiredStateStopped
		Expect(k8sClient.Update(ctx, updated)).To(Succeed())
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		podKey := types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}
		pod := &corev1.Pod{}
		if err := k8sClient.Get(ctx, podKey, pod); err == nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0)))).To(Succeed())
		}
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(devpilotv1alpha1.PhaseStopped))
		Expect(updated.Status.NodeName).To(BeEmpty())
		Expect(updated.Status.LastNodeName).To(Equal(nodeName))

		By("restarting the DevBox and verifying weight 80 hostname affinity")
		updated.Spec.DesiredState = devpilotv1alpha1.DesiredStateRunning
		Expect(k8sClient.Update(ctx, updated)).To(Succeed())
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		restartedPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, podKey, restartedPod)).To(Succeed())
		Expect(restartedPod.Spec.Affinity).NotTo(BeNil())
		prefs := restartedPod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
		Expect(prefs).To(HaveLen(1))
		Expect(prefs[0].Weight).To(Equal(int32(80)))
		Expect(prefs[0].Preference.MatchExpressions[0].Key).To(Equal(corev1.LabelHostname))
		Expect(prefs[0].Preference.MatchExpressions[0].Values).To(Equal([]string{nodeName}))

		By("resetting the root disk and verifying lastNodeName is cleared")
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, restartedPod, client.GracePeriodSeconds(0)))).To(Succeed())
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		updated.Spec.DesiredState = devpilotv1alpha1.DesiredStateStopped
		Expect(k8sClient.Update(ctx, updated)).To(Succeed())
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		updated.Annotations = map[string]string{AnnotationAction: actionResetRootDisk + "=1"}
		Expect(k8sClient.Update(ctx, updated)).To(Succeed())
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.LastNodeName).To(BeEmpty())
	})

	It("classifies CacheHit, P2PPulling, and ColdPulling accurately", func() {
		nodeA := namespace + "-node-a"
		nodeB := namespace + "-node-b"
		createTestNode(nodeA, true, map[string]string{
			corev1.LabelHostname: nodeA,
			DefaultGPUNodeLabel:  testGPUModelA100,
		}, nil, []string{testDevBoxImage})
		createTestNode(nodeB, true, map[string]string{
			corev1.LabelHostname: nodeB,
			DefaultGPUNodeLabel:  "h100",
		}, nil, nil)

		By("observing P2PPulling when another cluster node caches the image but the scheduled node does not")
		box := newTestDevBox("p2p-box", namespace)
		box.Spec.Resources.GPUModel = "h100"
		key := client.ObjectKeyFromObject(box)
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.ImageCacheState).To(Equal(devpilotv1alpha1.ImageCacheStateP2PPulling))

		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}, pod)).To(Succeed())
		bindPodToNode(pod, nodeB)
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.ImageCacheState).To(Equal(devpilotv1alpha1.ImageCacheStateP2PPulling))
	})

	It("runs two-phase GPU-free prepull for large cold images and binds GPU pod to the prepulled node", func() {
		nodeName := namespace + "-gpu-prepull"
		createTestNode(nodeName, true, map[string]string{
			corev1.LabelHostname: nodeName,
			DefaultGPUNodeLabel:  testGPUModelA100,
		}, nil, nil)

		resolver.info.TotalSizeBytes = 25 << 30 // 25 GiB > 20 GiB threshold
		r.Config.PrepullThresholdBytes = 20 << 30

		box := newTestDevBox("prepull-box", namespace)
		key := client.ObjectKeyFromObject(box)
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})

		By("creating the GPU-free prepull pod before the GPU pod")
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.StartingStep).To(Equal(devpilotv1alpha1.StartingStepPrepullingImage))
		Expect(box.Status.ImageCacheState).To(Equal(devpilotv1alpha1.ImageCacheStatePrepulling))
		Expect(box.Status.ImageSizeBytes).To(Equal(int64(25 << 30)))
		Expect(box.Status.PrepullPodName).To(Equal(box.Name + PrepullPodSuffix))

		gpuPod := &corev1.Pod{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}, gpuPod)
		Expect(err).To(HaveOccurred())

		prepullPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PrepullPodSuffix}, prepullPod)).To(Succeed())
		Expect(prepullPod.Spec.Containers[0].Args).To(Equal([]string{"--noop"}))
		_, requestsGPU := prepullPod.Spec.Containers[0].Resources.Limits[GPUResourceName]
		Expect(requestsGPU).To(BeFalse())

		By("completing the prepull pod and verifying GPU pod is created with weight 100 affinity")
		bindPodToNode(prepullPod, nodeName)
		prepullPod.Status.Phase = corev1.PodSucceeded
		Expect(k8sClient.Status().Update(ctx, prepullPod)).To(Succeed())

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.PrepulledNode).To(Equal(nodeName))
		Expect(box.Status.PrepullPodName).To(BeEmpty())
		Expect(box.Status.StartingStep).To(Equal(devpilotv1alpha1.StartingStepPullingImage))
		Expect(box.Status.ImageCacheState).To(Equal(devpilotv1alpha1.ImageCacheStateCacheHit))

		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}, gpuPod)).To(Succeed())
		Expect(gpuPod.Spec.Affinity).NotTo(BeNil())
		prefs := gpuPod.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
		Expect(prefs[0].Weight).To(Equal(int32(100)))
		Expect(prefs[0].Preference.MatchExpressions[0].Values).To(Equal([]string{nodeName}))
	})

	It("skips prepull when PrepullPolicyNever is set", func() {
		resolver.info.TotalSizeBytes = 30 << 30
		r.Config.PrepullThresholdBytes = 20 << 30

		box := newTestDevBox("never-prepull", namespace)
		box.Spec.Scheduling.Prepull = devpilotv1alpha1.PrepullPolicyNever
		key := client.ObjectKeyFromObject(box)
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		gpuPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PodSuffix}, gpuPod)).To(Succeed())
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: box.Name + PrepullPodSuffix}, &corev1.Pod{})
		Expect(err).To(HaveOccurred())
	})
})
