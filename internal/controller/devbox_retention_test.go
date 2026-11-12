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

var _ = Describe("DevBox automation", func() {
	var (
		namespace string
		recorder  *events.FakeRecorder
		r         *DevBoxReconciler
	)

	BeforeEach(func() {
		specCounter++
		namespace = fmt.Sprintf("automation-%d-%d", GinkgoRandomSeed(), specCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
		r = newTestReconciler(newTestResolver())
		recorder = events.NewFakeRecorder(32)
		r.Recorder = recorder
	})

	AfterEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
	})

	podKey := func(name string) types.NamespacedName {
		return types.NamespacedName{Namespace: namespace, Name: name + PodSuffix}
	}

	// runBox drives a fresh devbox to Running.
	runBox := func(box *devpilotv1alpha1.DevBox) types.NamespacedName {
		Expect(k8sClient.Create(ctx, box)).To(Succeed())
		key := client.ObjectKeyFromObject(box)
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: box.Name + RootDiskPVCSuffix})
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, podKey(box.Name), pod)).To(Succeed())
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		return key
	}

	// backdateStart makes the open segment look old, the way a session that has
	// been going for a while does.
	backdateStart := func(key types.NamespacedName, d time.Duration) *devpilotv1alpha1.DevBox {
		box := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Segments).NotTo(BeEmpty())
		start := metav1.NewTime(timeNow().Add(-d))
		box.Status.Segments[len(box.Status.Segments)-1].StartTime = start
		Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())
		return box
	}

	// setHeartbeat writes the platform's activity annotation.
	setHeartbeat := func(key types.NamespacedName, d time.Duration) *devpilotv1alpha1.DevBox {
		box := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		if box.Annotations == nil {
			box.Annotations = map[string]string{}
		}
		box.Annotations[AnnotationLastActive] = timeNow().Add(-d).Format(time.RFC3339)
		Expect(k8sClient.Update(ctx, box)).To(Succeed())
		return box
	}

	// stopBox shuts a devbox down through the user-facing path and returns once
	// the pod is gone and the phase has landed.
	stopBox := func(key types.NamespacedName) *devpilotv1alpha1.DevBox {
		box := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		box.Spec.DesiredState = devpilotv1alpha1.DesiredStateStopped
		Expect(k8sClient.Update(ctx, box)).To(Succeed())
		_, err := reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, podKey(box.Name), &corev1.Pod{}))
		}, "10s").Should(BeTrue())
		_, err = reconcileOnce(r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseStopped))
		return box
	}

	// rewindNonRunning moves the start of the non-running streak into the past,
	// which is how a devbox that has sat idle for days looks.
	rewindNonRunning := func(key types.NamespacedName, d time.Duration) *devpilotv1alpha1.DevBox {
		box := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
		since := metav1.NewTime(timeNow().Add(-d))
		box.Status.NonRunningSince = &since
		Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())
		return box
	}

	Context("idle shutdown", func() {
		It("stops a devbox whose open segment has been idle past the limit", func() {
			box := newTestDevBox("idle", namespace)
			box.Spec.Lifecycle.IdleShutdownMinutes = 10
			key := runBox(box)

			backdateStart(key, 11*time.Minute)
			_, err := reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
			Expect(box.Spec.DesiredState).To(Equal(devpilotv1alpha1.DesiredStateStopped))
			Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseStopping))
			Expect(box.Status.Message).To(ContainSubstring("No activity for 10 minutes"))

			By("closing the segment as an idle timeout, not a user stop")
			Expect(box.Status.Segments).To(HaveLen(1))
			Expect(box.Status.Segments[0].EndReason).To(Equal(devpilotv1alpha1.EndReasonIdleTimeout))
			Expect(box.Status.Segments[0].EndTime).NotTo(BeNil())
			Expect(box.Status.Segments[0].BilledSeconds).To(BeNumerically(">=", 600))

			By("reporting what happened")
			ready := metaCondition(box, ConditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Reason).To(Equal(reasonIdleTimeout))
			Eventually(recorder.Events).Should(Receive(ContainSubstring("IdleShutdown")))

			By("settling into Stopped")
			// The idle reconcile wrote desiredState; the next one is the one that
			// acts on it and deletes the pod.
			_, err = reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, podKey(box.Name), &corev1.Pod{}))
			}, "10s").Should(BeTrue())
			_, err = reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
			Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseStopped))
			Expect(box.Status.Segments).To(HaveLen(1), "the stop does not close the segment twice")
		})

		It("takes the heartbeat as the later activity and leaves a refreshed devbox alone", func() {
			box := newTestDevBox("heartbeat", namespace)
			box.Spec.Lifecycle.IdleShutdownMinutes = 10
			key := runBox(box)

			By("being older than the heartbeat")
			backdateStart(key, 60*time.Minute)
			setHeartbeat(key, 5*time.Minute)
			_, err := reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
			Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseRunning))
			Expect(box.Spec.DesiredState).To(Equal(devpilotv1alpha1.DesiredStateRunning))

			By("being idle once the heartbeat goes stale too")
			setHeartbeat(key, 15*time.Minute)
			_, err = reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
			Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseStopping))
		})

		It("bills the run it shuts down even when the heartbeat is older than the pod", func() {
			// The ledger must open before the idle verdict is taken: a stale
			// heartbeat left over from the previous run would otherwise shut a
			// freshly started devbox down in the same reconcile that first sees it
			// Running, closing no segment and billing nothing.
			box := newTestDevBox("fresh", namespace)
			box.Spec.Lifecycle.IdleShutdownMinutes = 10
			box.Spec.PricePerHour = "2.80"
			box.Annotations = map[string]string{AnnotationLastActive: timeNow().Add(-time.Hour).Format(time.RFC3339)}
			key := runBox(box)

			Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
			Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseRunning), "the segment start outranks a stale heartbeat")
			Expect(box.Status.Segments).To(HaveLen(1))
			Expect(box.Status.Segments[0].EndTime).To(BeNil())

			backdateStart(key, 11*time.Minute)
			_, err := reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
			Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseStopping))
			Expect(box.Status.Segments).To(HaveLen(1))
			Expect(box.Status.Segments[0].EndReason).To(Equal(devpilotv1alpha1.EndReasonIdleTimeout))
			Expect(box.Status.Segments[0].BilledSeconds).To(BeNumerically(">=", 600))
			Expect(box.Status.TotalCost).NotTo(Equal("0.00"), "an idle timeout is still a run to bill")
		})

		It("leaves a devbox alone when idle shutdown is disabled", func() {
			box := newTestDevBox("nodle", namespace)
			key := runBox(box)

			backdateStart(key, 48*time.Hour)
			_, err := reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
			Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseRunning))
			Expect(box.Spec.DesiredState).To(Equal(devpilotv1alpha1.DesiredStateRunning))
		})
	})

	Context("retention", func() {
		It("counts down from the first non-running instant and resets on a run", func() {
			box := newTestDevBox("countdown", namespace)
			key := runBox(box)

			By("starting the countdown when it stops")
			box = stopBox(key)
			Expect(box.Status.NonRunningSince).NotTo(BeNil())
			Expect(box.Status.ScheduledReleaseTime).NotTo(BeNil())
			Expect(box.Status.ScheduledReleaseTime.Time).To(
				BeTemporally("==", box.Status.NonRunningSince.Add(15*24*time.Hour)))

			By("keeping the first instant across an interruption")
			// metav1.Time goes through the API as whole seconds, so the value the
			// status is compared against is truncated the same way.
			since := metav1.NewTime(timeNow().Add(-48 * time.Hour).Truncate(time.Second))
			Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
			// The start action below needs a running spec to leave behind.
			box.Spec.DesiredState = devpilotv1alpha1.DesiredStateRunning
			Expect(k8sClient.Update(ctx, box)).To(Succeed())
			Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
			box.Status.NonRunningSince = &since
			box.Status.Phase = devpilotv1alpha1.PhaseInterrupted
			interrupted := metav1.NewTime(timeNow())
			box.Status.InterruptedAt = &interrupted
			Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())

			_, err := reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
			Expect(box.Status.NonRunningSince.Equal(&since)).To(BeTrue(), "switching to interrupted keeps the streak")
			Expect(box.Status.ScheduledReleaseTime.Time).To(BeTemporally("==", since.Add(15*24*time.Hour)))

			By("clearing the countdown once it runs again")
			if box.Annotations == nil {
				box.Annotations = map[string]string{}
			}
			box.Annotations[AnnotationAction] = actionStart + "=" + epochNow()
			Expect(k8sClient.Update(ctx, box)).To(Succeed())
			_, err = reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())

			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, podKey(box.Name), pod)).To(Succeed())
			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
			_, err = reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
			Expect(box.Status.Phase).To(Equal(devpilotv1alpha1.PhaseRunning))
			Expect(box.Status.NonRunningSince).To(BeNil())
			Expect(box.Status.ScheduledReleaseTime).To(BeNil())
			Expect(metaCondition(box, ConditionRetentionExpiring)).To(BeNil())
		})

		It("warns at 72 hours and at 24 hours, once each", func() {
			box := newTestDevBox("warn", namespace)
			key := runBox(box)
			stopBox(key)

			By("warning 60 hours before the release")
			rewindNonRunning(key, 15*24*time.Hour-60*time.Hour)
			_, err := reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
			cond := metaCondition(box, ConditionRetentionExpiring)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(reasonExpiring72h))
			Eventually(recorder.Events).Should(Receive(ContainSubstring("ShutdownRetentionExpiring")))

			By("staying quiet while the same threshold still holds")
			_, err = reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())
			Consistently(recorder.Events, "500ms").ShouldNot(Receive())

			By("warning again inside the last day")
			rewindNonRunning(key, 15*24*time.Hour-10*time.Hour)
			_, err = reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
			Expect(metaCondition(box, ConditionRetentionExpiring).Reason).To(Equal(reasonExpiring24h))
			Eventually(recorder.Events).Should(Receive(ContainSubstring("ShutdownRetentionExpiring")))

			By("dropping the warning while the release is still far off")
			rewindNonRunning(key, 24*time.Hour)
			_, err = reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, box)).To(Succeed())
			Expect(metaCondition(box, ConditionRetentionExpiring)).To(BeNil())
		})

		It("hands an expired root disk to volumetrash and deletes the devbox", func() {
			box := newTestDevBox("expired", namespace)
			key := runBox(box)
			stopBox(key)

			rewindNonRunning(key, 16*24*time.Hour)
			_, err := reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, key, &devpilotv1alpha1.DevBox{}))
			}, "10s").Should(BeTrue())

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: box.Name + RootDiskPVCSuffix,
			}, pvc)).To(Succeed(), "the recycler owns the volume, not the garbage collector")
			Expect(pvc.Labels).To(HaveKey(LabelExpireAt))
			_, parseErr := time.Parse(expireAtLayout, pvc.Labels[LabelExpireAt])
			Expect(parseErr).NotTo(HaveOccurred())
			Expect(pvc.OwnerReferences).To(BeEmpty(), "a kept owner reference would let GC delete the disk")

			Eventually(recorder.Events).Should(Receive(ContainSubstring("Released")))
		})

		It("deletes an expired root disk when the operator reclaims by deletion", func() {
			r.Config.RetentionReclaim = ReclaimDelete
			box := newTestDevBox("expired-delete", namespace)
			key := runBox(box)
			stopBox(key)

			rewindNonRunning(key, 16*24*time.Hour)
			_, err := reconcileOnce(r, key)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, key, &devpilotv1alpha1.DevBox{}))
			}, "10s").Should(BeTrue())
			// envtest runs no volume controller, so pvc-protection keeps the claim
			// around after the delete; the deletion timestamp is the evidence the
			// operator asked for it.
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: box.Name + RootDiskPVCSuffix,
			}, pvc)).To(Succeed())
			Expect(pvc.DeletionTimestamp).NotTo(BeNil(), "the root disk of an expired devbox is deleted")
			Expect(pvc.Labels).NotTo(HaveKey(LabelExpireAt))
		})
	})
})

var _ = Describe("Retention arithmetic", func() {
	It("keeps a devbox forever when retention is disabled", func() {
		box := &devpilotv1alpha1.DevBox{ObjectMeta: metav1.ObjectMeta{Name: "forever"}}
		box.Spec.RootDisk.RetentionDaysAfterStop = 0
		since := metav1.NewTime(timeNow().Add(-400 * 24 * time.Hour))
		box.Status.Phase = devpilotv1alpha1.PhaseStopped
		box.Status.NonRunningSince = &since

		r := &DevBoxReconciler{}
		Expect(r.settleRetention(box, timeNow())).To(BeFalse())
		Expect(box.Status.ScheduledReleaseTime).To(BeNil())
		Expect(automationWake(box, timeNow())).To(BeZero())
	})

	It("wakes at the next threshold rather than polling", func() {
		box := &devpilotv1alpha1.DevBox{ObjectMeta: metav1.ObjectMeta{Name: "wake"}}
		box.Spec.RootDisk.RetentionDaysAfterStop = 15
		now := timeNow()
		// Ten days in, the next thing to do is warn at 72 hours before release,
		// which is three days away: the hourly cap wins.
		since := metav1.NewTime(now.Add(-10 * 24 * time.Hour))
		box.Status.Phase = devpilotv1alpha1.PhaseStopped
		box.Status.NonRunningSince = &since
		Expect(automationWake(box, now)).To(Equal(automationRequeue))

		// Forty minutes before release the next wake is the release itself, which
		// is under the hourly cap.
		since = metav1.NewTime(now.Add(-15*24*time.Hour + 40*time.Minute))
		box.Status.NonRunningSince = &since
		Expect(automationWake(box, now)).To(Equal(40 * time.Minute))
	})

	It("wakes for idle shutdown only while the devbox runs", func() {
		box := &devpilotv1alpha1.DevBox{ObjectMeta: metav1.ObjectMeta{Name: "idle-wake"}}
		box.Spec.Lifecycle.IdleShutdownMinutes = 30
		now := timeNow()
		start := metav1.NewTime(now.Add(-10 * time.Minute))
		box.Status.Phase = devpilotv1alpha1.PhaseRunning
		box.Status.Segments = []devpilotv1alpha1.BillingSegment{{ID: 1, StartTime: start}}
		Expect(automationWake(box, now)).To(Equal(20 * time.Minute))

		box.Status.Phase = devpilotv1alpha1.PhaseStopped
		box.Status.NonRunningSince = nil
		Expect(automationWake(box, now)).To(BeZero())
	})
})
