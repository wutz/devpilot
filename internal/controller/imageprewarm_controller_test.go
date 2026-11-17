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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

var _ = Describe("ImagePrewarm Controller", func() {
	var (
		namespace string
		resolver  *fakeResolver
		r         *ImagePrewarmReconciler
	)

	BeforeEach(func() {
		specCounter++
		namespace = fmt.Sprintf("prewarm-ns-%d-%d", GinkgoRandomSeed(), specCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		resolver = newTestResolver()
		r = &ImagePrewarmReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Config: Config{
				InitImage:        testInit,
				PrewarmNamespace: namespace,
			},
			Images: resolver,
		}
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))).To(Succeed())
	})

	createPrewarm := func(ipw *devpilotv1alpha1.ImagePrewarm) types.NamespacedName {
		Expect(k8sClient.Create(ctx, ipw)).To(Succeed())
		DeferCleanup(func() {
			current := &devpilotv1alpha1.ImagePrewarm{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: ipw.Name}, current); err == nil {
				controllerutil.RemoveFinalizer(current, PrewarmFinalizer)
				_ = k8sClient.Update(ctx, current)
				_ = k8sClient.Delete(ctx, current)
			}
		})
		return types.NamespacedName{Name: ipw.Name}
	}

	reconcileIPW := func(key types.NamespacedName) (reconcile.Result, *devpilotv1alpha1.ImagePrewarm) {
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		ipw := &devpilotv1alpha1.ImagePrewarm{}
		Expect(k8sClient.Get(ctx, key, ipw)).To(Succeed())
		return res, ipw
	}

	It("rolls out prewarm pods respecting maxConcurrency, nodeSelector, and tolerations", func() {
		const testBoolTrue = "true"
		selector := map[string]string{namespace + "/gpu-present": testBoolTrue}
		toleration := corev1.Toleration{Key: namespace + "/gpu", Operator: corev1.TolerationOpExists}
		taints := make([]corev1.Taint, 0, 2)
		taints = append(taints, corev1.Taint{Key: namespace + "/gpu", Effect: corev1.TaintEffectNoSchedule})

		for i := 1; i <= 3; i++ {
			createTestNode(fmt.Sprintf("%s-gpu-%d", namespace, i), true, selector, taints, nil)
		}
		// Non-matching CPU node, unready node, and untolerated node must be ignored.
		createTestNode(namespace+"-cpu", true, map[string]string{namespace + "/cpu": testBoolTrue}, nil, nil)
		createTestNode(namespace+"-unready", false, selector, taints, nil)
		createTestNode(namespace+"-untolerated", true, selector, append(taints, corev1.Taint{
			Key: "maintenance", Effect: corev1.TaintEffectNoSchedule,
		}), nil)

		key := createPrewarm(&devpilotv1alpha1.ImagePrewarm{
			ObjectMeta: metav1.ObjectMeta{Name: namespace + "-rollout"},
			Spec: devpilotv1alpha1.ImagePrewarmSpec{
				Image:          testDevBoxImage,
				NodeSelector:   selector,
				Tolerations:    []corev1.Toleration{toleration},
				MaxConcurrency: 2,
			},
		})

		_, ipw := reconcileIPW(key)
		Expect(ipw.Status.Phase).To(Equal(devpilotv1alpha1.ImagePrewarmPhaseRunning))
		Expect(ipw.Status.DesiredNodes).To(Equal(int32(3)))
		Expect(ipw.Status.InProgressNodes).To(Equal(int32(2)))
		Expect(ipw.Status.WarmedNodes).To(Equal(int32(0)))

		pods := &corev1.PodList{}
		Expect(k8sClient.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{LabelImagePrewarm: ipw.Name})).To(Succeed())
		Expect(pods.Items).To(HaveLen(2))
		for _, pod := range pods.Items {
			Expect(pod.Spec.Containers[0].Args).To(Equal([]string{"--noop"}))
			_, hasGPU := pod.Spec.Containers[0].Resources.Limits[GPUResourceName]
			Expect(hasGPU).To(BeFalse())
		}

		By("completing the first two pods and warming the remaining node")
		for i := range pods.Items {
			p := &pods.Items[i]
			p.Status.Phase = corev1.PodSucceeded
			Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		}
		_, ipw = reconcileIPW(key)
		Expect(ipw.Status.WarmedNodes).To(Equal(int32(2)))
		Expect(ipw.Status.InProgressNodes).To(Equal(int32(1)))
		Expect(ipw.Status.Phase).To(Equal(devpilotv1alpha1.ImagePrewarmPhaseRunning))

		By("completing the final node and verifying Completed status")
		Expect(k8sClient.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{LabelImagePrewarm: ipw.Name})).To(Succeed())
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Status.Phase != corev1.PodSucceeded {
				p.Status.Phase = corev1.PodSucceeded
				Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
			}
		}
		_, ipw = reconcileIPW(key)
		Expect(ipw.Status.Phase).To(Equal(devpilotv1alpha1.ImagePrewarmPhaseCompleted))
		Expect(ipw.Status.WarmedNodes).To(Equal(int32(3)))
		Expect(ipw.Status.CompletedAt).NotTo(BeNil())

		By("adding a new matching node and verifying automatic expansion back to Running")
		createTestNode(namespace+"-gpu-4", true, selector, taints, nil)
		_, ipw = reconcileIPW(key)
		Expect(ipw.Status.Phase).To(Equal(devpilotv1alpha1.ImagePrewarmPhaseRunning))
		Expect(ipw.Status.DesiredNodes).To(Equal(int32(4)))
		Expect(ipw.Status.InProgressNodes).To(Equal(int32(1)))
	})

	It("creates --hold sentinel pods when pinImage is true", func() {
		selector := map[string]string{namespace + "/pin": "true"}
		nodeName := namespace + "-pin-node"
		createTestNode(nodeName, true, selector, nil, nil)

		key := createPrewarm(&devpilotv1alpha1.ImagePrewarm{
			ObjectMeta: metav1.ObjectMeta{Name: namespace + "-pin"},
			Spec: devpilotv1alpha1.ImagePrewarmSpec{
				Image:        testDevBoxImage,
				NodeSelector: selector,
				PinImage:     true,
			},
		})

		_, ipw := reconcileIPW(key)
		Expect(ipw.Status.Phase).To(Equal(devpilotv1alpha1.ImagePrewarmPhaseRunning))

		pods := &corev1.PodList{}
		Expect(k8sClient.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{LabelImagePrewarm: ipw.Name})).To(Succeed())
		Expect(pods.Items).To(HaveLen(1))
		sentinel := &pods.Items[0]
		Expect(sentinel.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyAlways))
		Expect(sentinel.Spec.Containers[0].Args).To(Equal([]string{"--hold"}))
		Expect(sentinel.Spec.Containers[0].Resources.Requests.Cpu().String()).To(Equal("5m"))
		Expect(sentinel.Spec.Containers[0].Resources.Requests.Memory().String()).To(Equal("8Mi"))

		sentinel.Status.Phase = corev1.PodRunning
		Expect(k8sClient.Status().Update(ctx, sentinel)).To(Succeed())
		_, ipw = reconcileIPW(key)
		Expect(ipw.Status.Phase).To(Equal(devpilotv1alpha1.ImagePrewarmPhaseCompleted))
		Expect(ipw.Status.WarmedNodes).To(Equal(int32(1)))
	})

	It("marks Degraded on failed pods, cleans up expired TTL resources, and deletes owned pods on deletion", func() {
		selector := map[string]string{namespace + "/ttl": "true"}
		createTestNode(namespace+"-ttl-node", true, selector, nil, nil)

		key := createPrewarm(&devpilotv1alpha1.ImagePrewarm{
			ObjectMeta: metav1.ObjectMeta{Name: namespace + "-ttl"},
			Spec: devpilotv1alpha1.ImagePrewarmSpec{
				Image:                 testDevBoxImage,
				NodeSelector:          selector,
				TTLHoursAfterComplete: 1,
			},
		})

		_, ipw := reconcileIPW(key)
		pods := &corev1.PodList{}
		Expect(k8sClient.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{LabelImagePrewarm: ipw.Name})).To(Succeed())
		Expect(pods.Items).To(HaveLen(1))

		By("marking pod Failed transitions ImagePrewarm to Degraded")
		pods.Items[0].Status.Phase = corev1.PodFailed
		Expect(k8sClient.Status().Update(ctx, &pods.Items[0])).To(Succeed())
		_, ipw = reconcileIPW(key)
		Expect(ipw.Status.Phase).To(Equal(devpilotv1alpha1.ImagePrewarmPhaseDegraded))
		Expect(ipw.Status.FailedNodes).To(Equal(int32(1)))

		By("completing and backdating CompletedAt triggers TTL deletion")
		pods.Items[0].Status.Phase = corev1.PodSucceeded
		Expect(k8sClient.Status().Update(ctx, &pods.Items[0])).To(Succeed())
		res, ipw := reconcileIPW(key)
		Expect(ipw.Status.Phase).To(Equal(devpilotv1alpha1.ImagePrewarmPhaseCompleted))
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))

		expiredAt := metav1.NewTime(time.Now().Add(-2 * time.Hour))
		ipw.Status.CompletedAt = &expiredAt
		Expect(k8sClient.Status().Update(ctx, ipw)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		// Reconcile once more to run finalizer after deletion timestamp is set.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		err = k8sClient.Get(ctx, key, &devpilotv1alpha1.ImagePrewarm{})
		Expect(err).To(HaveOccurred())
		Expect(k8sClient.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{LabelImagePrewarm: ipw.Name})).To(Succeed())
		Expect(pods.Items).To(BeEmpty())
	})
})
