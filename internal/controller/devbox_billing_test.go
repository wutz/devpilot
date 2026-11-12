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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// testSpotPrice is what the spot specs are sold at.
	testSpotPrice = "2.80"
	// testSpotNode is a node the specs drain to simulate a reclaim.
	testSpotNode = "spot-node-1"
)

// cachedClient stands in for the informer cache: it keeps serving the DevBox
// status it was handed, the way a cache that has not yet seen a status patch
// does. Everything else goes to the real client.
type cachedClient struct {
	client.Client
	box *devpilotv1alpha1.DevBox
}

func (c cachedClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if box, ok := obj.(*devpilotv1alpha1.DevBox); ok {
		c.box.DeepCopyInto(box)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// metaCondition returns one condition of a DevBox.
func metaCondition(box *devpilotv1alpha1.DevBox, condType string) *metav1.Condition {
	for i := range box.Status.Conditions {
		if box.Status.Conditions[i].Type == condType {
			return &box.Status.Conditions[i]
		}
	}
	return nil
}

var _ = Describe("Billing arithmetic", func() {
	DescribeTable("parsing an hourly price into micros",
		func(price string, want int64, ok bool) {
			got, parsed := parsePriceMicros(price)
			Expect(parsed).To(Equal(ok))
			Expect(got).To(Equal(want))
		},
		Entry("whole units", "3", int64(3_000_000), true),
		Entry("two decimals", "2.80", int64(2_800_000), true),
		Entry("one decimal", "2.8", int64(2_800_000), true),
		Entry("six decimals", "0.000001", int64(1), true),
		Entry("zero", "0", int64(0), true),
		Entry("empty", "", int64(0), false),
		Entry("too many decimals", "1.1234567", int64(0), false),
		Entry("no whole part", ".5", int64(0), false),
		Entry("not a number", "cheap", int64(0), false),
	)

	DescribeTable("formatting micros back into an amount",
		func(micros int64, want string) {
			Expect(formatAmount(micros)).To(Equal(want))
		},
		Entry("zero", int64(0), "0.00"),
		Entry("negative", int64(-5), "0.00"),
		Entry("a whole amount", int64(3_000_000), "3.00"),
		Entry("two decimals", int64(2_800_000), "2.80"),
		Entry("full precision", int64(1_234_567), "1.234567"),
		Entry("a fraction of a unit", int64(1), "0.000001"),
	)

	DescribeTable("billing a duration",
		func(price string, seconds int64, want string) {
			Expect(segmentCost(price, seconds)).To(Equal(want))
		},
		Entry("a full hour", testSpotPrice, int64(3600), "2.80"),
		Entry("half an hour", testSpotPrice, int64(1800), "1.40"),
		Entry("a minute", "60", int64(60), "1.00"),
		// metav1.Time keeps whole seconds, but the cost keeps the full scale so an
		// invoice run never has to re-derive a short segment.
		Entry("one second", "60", int64(1), "0.016666"),
		Entry("no time", testSpotPrice, int64(0), "0.00"),
		Entry("an unparseable price bills nothing", "", int64(3600), ""),
	)

	It("drops the oldest closed segments and keeps the open one", func() {
		box := &devpilotv1alpha1.DevBox{}
		for i := 1; i <= maxBillingSegments; i++ {
			end := metav1.NewTime(time.Unix(int64(i), 0))
			box.Status.Segments = append(box.Status.Segments, devpilotv1alpha1.BillingSegment{
				ID:      int64(i),
				EndTime: &end,
			})
		}

		openSegment(box, time.Unix(10_000, 0))

		Expect(box.Status.Segments).To(HaveLen(maxBillingSegments))
		Expect(box.Status.Segments[0].ID).To(Equal(int64(2)), "the oldest closed segment goes first")
		Expect(box.Status.Segments[len(box.Status.Segments)-1].EndTime).To(BeNil())
	})

	It("adopts an open segment instead of opening a second one", func() {
		box := &devpilotv1alpha1.DevBox{}
		openSegment(box, time.Unix(100, 0))
		openSegment(box, time.Unix(200, 0))

		Expect(box.Status.Segments).To(HaveLen(1))
		Expect(box.Status.Segments[0].StartTime.Time.Unix()).To(Equal(int64(100)))
	})
})

var _ = Describe("DevBox billing ledger", func() {
	var (
		namespace string
		recorder  *events.FakeRecorder
		r         *DevBoxReconciler
	)

	BeforeEach(func() {
		specCounter++
		namespace = fmt.Sprintf("billing-%d-%d", GinkgoRandomSeed(), specCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
		r = newTestReconciler(newTestResolver())
		recorder = events.NewFakeRecorder(16)
		r.Recorder = recorder
	})

	AfterEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
	})

	// createBound creates a devbox with a bound claim and a pod the kubelet has
	// not picked up yet.
	createBound := func(box *devpilotv1alpha1.DevBox) types.NamespacedName {
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		key := client.ObjectKeyFromObject(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: box.Namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		return key
	}

	podKey := func(name string) types.NamespacedName {
		return types.NamespacedName{Namespace: namespace, Name: name + PodSuffix}
	}

	// markPodStatus writes a pod phase the way a kubelet would report it.
	markPodStatus := func(name string, phase corev1.PodPhase, reason, message string) {
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, podKey(name), pod)).To(Succeed())
		pod.Status.Phase = phase
		pod.Status.Reason = reason
		pod.Status.Message = message
		if phase == corev1.PodRunning {
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	markRunning := func(name string) {
		markPodStatus(name, corev1.PodRunning, "", "")
	}

	// runUntilRunning drives a fresh devbox to Running.
	runUntilRunning := func(box *devpilotv1alpha1.DevBox) types.NamespacedName {
		key := createBound(box)
		markRunning(box.Name)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		return key
	}

	spotBox := func(name string) *devpilotv1alpha1.DevBox {
		box := newTestDevBox(name, namespace)
		box.Spec.BillingMode = devpilotv1alpha1.BillingModeSpot
		box.Spec.PricePerHour = testSpotPrice
		return box
	}

	It("opens a segment while the devbox runs", func() {
		box := spotBox("running")
		key := runUntilRunning(box)

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseRunning))
		Expect(box.Status.Segments).To(HaveLen(1))
		segment := box.Status.Segments[0]
		Expect(segment.ID).To(Equal(int64(1)))
		Expect(segment.PricePerHour).To(Equal(testSpotPrice))
		Expect(segment.StartTime.IsZero()).To(BeFalse())
		Expect(segment.EndTime).To(BeNil())
		Expect(segment.BilledSeconds).To(BeZero())
		Expect(segment.Cost).To(BeEmpty())

		By("not rewriting the ledger on an unchanged run")
		first := box.Status.Segments[0].StartTime
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Segments).To(HaveLen(1))
		Expect(box.Status.Segments[0].StartTime.Equal(&first)).To(BeTrue())
	})

	It("closes the segment when the devbox stops and opens a new one on restart", func() {
		box := spotBox("stopped")
		key := runUntilRunning(box)

		By("stopping")
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		box.Spec.DesiredState = devpilotv1alpha1.DesiredStateStopped
		Expect(k8sClient.Update(ctx, box)).To(Succeed())
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		By("deleting the pod the way the controller did")
		pod := &corev1.Pod{}
		err = k8sClient.Get(ctx, podKey(box.Name), pod)
		if err == nil {
			Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
		}
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, podKey(box.Name), &corev1.Pod{}))
		}, "10s").Should(BeTrue())

		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseStopped))
		Expect(box.Status.Segments).To(HaveLen(1))
		segment := box.Status.Segments[0]
		Expect(segment.EndTime).NotTo(BeNil())
		Expect(segment.EndReason).To(Equal(devpilotv1alpha1.EndReasonUserStopped))
		Expect(segment.BilledSeconds).To(BeNumerically(">=", 0))
		Expect(segment.Cost).To(MatchRegexp(`^[0-9]+\.[0-9]{2,6}$`))
		Expect(box.Status.TotalCost).To(Equal(segment.Cost))

		By("starting again")
		box.Spec.DesiredState = devpilotv1alpha1.DesiredStateRunning
		Expect(k8sClient.Update(ctx, box)).To(Succeed())
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markRunning(box.Name)
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Segments).To(HaveLen(2))
		Expect(box.Status.Segments[1].ID).To(Equal(int64(2)))
		Expect(box.Status.Segments[1].StartTime.Before(box.Status.Segments[0].EndTime)).To(BeFalse(),
			"the new segment starts at or after the one it follows")
		Expect(box.Status.Segments[1].EndTime).To(BeNil())
	})

	It("interrupts a spot devbox the platform evicted and bills the segment as preempted", func() {
		box := spotBox("preempted")
		key := runUntilRunning(box)

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		box.Status.NodeName = testSpotNode
		Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())

		markPodStatus(box.Name, corev1.PodFailed, "Evicted", "The node was low on resource: ephemeral-storage.")
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseInterrupted))
		Expect(box.Status.InterruptedAt).NotTo(BeNil())
		Expect(box.Status.NonRunningSince).NotTo(BeNil())
		Expect(box.Status.Message).To(ContainSubstring("spot node was reclaimed"))
		Expect(box.Status.NodeName).To(BeEmpty(), "an interrupted devbox is on no node")

		By("reporting the cause without a running verdict")
		ready := metaCondition(box, ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("Preempted"))
		preempt := metaCondition(box, ConditionPreempted)
		Expect(preempt).NotTo(BeNil())
		Expect(preempt.Status).To(Equal(metav1.ConditionTrue))

		By("closing the ledger segment")
		Expect(box.Status.Segments).To(HaveLen(1))
		Expect(box.Status.Segments[0].EndReason).To(Equal(devpilotv1alpha1.EndReasonPreempted))
		Expect(box.Status.Segments[0].EndTime).NotTo(BeNil())

		By("publishing an Event")
		Eventually(recorder.Events).Should(Receive(ContainSubstring("Warning Interrupted")))

		By("leaving the pod deleted")
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, podKey(box.Name), &corev1.Pod{}))
		}, "10s").Should(BeTrue())
	})

	It("interrupts a spot devbox whose node was taken out of service", func() {
		box := spotBox("drained")
		key := createBound(box)

		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: testSpotNode}}
		node.Spec.Taints = []corev1.Taint{{
			Key:    outOfServiceTaint,
			Effect: corev1.TaintEffectNoExecute,
		}}
		Expect(k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed())
		})

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		box.Status.Phase = devpilotv1alpha1.PhaseStarting
		box.Status.NodeName = testSpotNode
		Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())

		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseInterrupted))
		Expect(box.Status.Message).To(ContainSubstring("out of service"))

		By("not interrupting again once the node is cleared")
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.NodeName).To(BeEmpty())
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseInterrupted))
	})

	It("restarts an interrupted devbox only when the user asks", func() {
		box := spotBox("resume")
		key := runUntilRunning(box)

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		box.Status.NodeName = testSpotNode
		Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())
		markPodStatus(box.Name, corev1.PodFailed, "Preempted", "pod was preempted")
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, podKey(box.Name), &corev1.Pod{}))
		}, "10s").Should(BeTrue())

		By("staying put while nothing asks it to start")
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseInterrupted))
		Expect(k8sClient.Get(ctx, podKey(box.Name), &corev1.Pod{})).NotTo(Succeed())

		By("starting on request")
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		if box.Annotations == nil {
			box.Annotations = map[string]string{}
		}
		box.Annotations[AnnotationAction] = actionStart + "=" + time.Now().UTC().Format(time.RFC3339)
		Expect(k8sClient.Update(ctx, box)).To(Succeed())
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseStarting))
		Expect(metaCondition(box, ConditionPreempted)).To(BeNil(), "a restart clears the verdict")
		Expect(k8sClient.Get(ctx, podKey(box.Name), &corev1.Pod{})).To(Succeed())

		By("running again and opening a second segment")
		markRunning(box.Name)
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseRunning))
		Expect(box.Status.InterruptedAt).To(BeNil())
		Expect(box.Status.Segments).To(HaveLen(2))
		Expect(box.Status.Segments[0].EndReason).To(Equal(devpilotv1alpha1.EndReasonPreempted))
	})

	It("does not relaunch an interrupted devbox from a stale cached status", func() {
		box := spotBox("stale")
		key := runUntilRunning(box)

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		box.Status.NodeName = testSpotNode
		Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())

		// This is the running devbox as the informer cache still reports it.
		stale := box.DeepCopy()
		Expect(stale.Status.Phase).To(Equal(devpilotv1alpha1.PhaseRunning))

		markPodStatus(box.Name, corev1.PodFailed, "Evicted", "The node was low on resource: ephemeral-storage.")
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, podKey(box.Name), &corev1.Pod{}))
		}, "10s").Should(BeTrue())

		By("reconciling against a cache that has not seen the interruption")
		r.APIReader = k8sClient
		r.Client = cachedClient{Client: k8sClient, box: stale}
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseInterrupted))
		Expect(box.Status.Segments).To(HaveLen(1))
		Expect(k8sClient.Get(ctx, podKey(box.Name), &corev1.Pod{})).NotTo(Succeed(),
			"an interrupted devbox is not relaunched from a stale phase")
	})

	It("never interrupts an on-demand devbox", func() {
		box := newTestDevBox("ondemand", namespace)
		box.Spec.PricePerHour = testSpotPrice
		key := runUntilRunning(box)

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		box.Status.NodeName = testSpotNode
		Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())
		markPodStatus(box.Name, corev1.PodFailed, "Evicted", "The node was low on resource: memory.")
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseFailed))
		Expect(box.Status.InterruptedAt).To(BeNil())
		Expect(box.Status.Segments).To(HaveLen(1))
		Expect(box.Status.Segments[0].EndReason).To(Equal(devpilotv1alpha1.EndReasonNodeFailure))
	})
})
