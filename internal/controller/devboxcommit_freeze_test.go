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
	"sync"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

// The events fakeFreezer records, in the order a good session has them.
const (
	eventFreeze = "freeze"
	eventCut    = "cut"
	eventThaw   = "thaw"
)

// fakeFreezer stands in for the exec based freezer. It records the order of
// freeze, cut and thaw, and whether the snapshot existed at each step, so the
// tests can prove the snapshot was created inside the freeze.
type fakeFreezer struct {
	mu sync.Mutex

	namespace string
	events    []string
	// freezeErr fails Freeze; thawErr fails the session's Thaw, the way a
	// helper that thawed on its own (exit 3) reports.
	freezeErr error
	thawErr   error

	snapshotAtFreeze bool
	snapshotAtThaw   bool
	standaloneThaws  int
}

func (f *fakeFreezer) record(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *fakeFreezer) Events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *fakeFreezer) Freeze(_ context.Context, _ *corev1.Pod) (FreezeSession, error) {
	f.record(eventFreeze)
	f.mu.Lock()
	f.snapshotAtFreeze = !snapshotGone(f.namespace)
	f.mu.Unlock()
	if f.freezeErr != nil {
		return nil, f.freezeErr
	}
	return &fakeFreezeSession{freezer: f}, nil
}

func (f *fakeFreezer) Thaw(_ context.Context, _ *corev1.Pod) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.standaloneThaws++
	return nil
}

type fakeFreezeSession struct {
	freezer *fakeFreezer
}

func (s *fakeFreezeSession) Thaw(_ context.Context) error {
	s.freezer.record(eventThaw)
	s.freezer.mu.Lock()
	defer s.freezer.mu.Unlock()
	s.freezer.snapshotAtThaw = !snapshotGone(s.freezer.namespace)
	return s.freezer.thawErr
}

// newFreezeReconciler is the snapshot reconciler with a freezer and a fast
// cut poll.
func newFreezeReconciler(flusher RootDiskFlusher, freezer RootDiskFreezer) *DevBoxCommitReconciler {
	r := newSnapshotReconciler(flusher)
	r.Freezer = freezer
	r.cutPollInterval = 20 * time.Millisecond
	return r
}

// fakeCut reports a cut once the snapshot exists, and records the check.
func fakeCut(freezer *fakeFreezer, result bool) func(context.Context, types.NamespacedName) (bool, error) {
	return func(_ context.Context, key types.NamespacedName) (bool, error) {
		if snapshotGone(key.Namespace) {
			return false, fmt.Errorf("snapshot %s does not exist", key.Name)
		}
		freezer.record(eventCut)
		return result, nil
	}
}

// bindRootDiskToVolume binds the running DevBox's root disk claim to a CSI
// volume of the given driver, which is what the consistency promotion reads.
func bindRootDiskToVolume(namespace, driver string) {
	pvName := "pv-" + namespace
	Expect(k8sClient.Create(ctx, &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(testVolumeSize)},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			VolumeMode:                    new(corev1.PersistentVolumeBlock),
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: pvName},
			},
		},
	})).To(Succeed())

	pvc := &corev1.PersistentVolumeClaim{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Namespace: namespace, Name: testDevBoxRunning + RootDiskPVCSuffix,
	}, pvc)).To(Succeed())
	pvc.Spec.VolumeName = pvName
	Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
}

// cleanupVolume removes the volume bindRootDiskToVolume created. envtest runs
// no PV protection controller, so the finalizer has to go by hand.
func cleanupVolume(namespace string) {
	pv := &corev1.PersistentVolume{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "pv-" + namespace}, pv); err != nil {
		return
	}
	pv.Finalizers = nil
	_ = k8sClient.Update(ctx, pv)
	_ = k8sClient.Delete(ctx, pv)
}

// cutSnapshotWhenCreated plays the CSI snapshot controller: as soon as the
// snapshot exists it sets status.creationTime, the moment of the cut. It
// records "created" and eventCut so the order against the freeze is visible.
func cutSnapshotWhenCreated(stop <-chan struct{}, namespace string, freezer *fakeFreezer) {
	go func() {
		defer GinkgoRecover()
		recorded := false
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			snap := &snapshotv1.VolumeSnapshot{}
			if err := k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitSnapshotSuffix,
			}, snap); err != nil {
				continue
			}
			// Recorded before the update: the reconciler may thaw as soon
			// as the update lands.
			if !recorded {
				recorded = true
				freezer.record("created")
				freezer.record(eventCut)
			}
			now := metav1.Now()
			if snap.Status == nil {
				snap.Status = &snapshotv1.VolumeSnapshotStatus{}
			}
			snap.Status.CreationTime = &now
			if err := k8sClient.Status().Update(ctx, snap); err != nil {
				continue
			}
			return
		}
	}()
}

func readyCondition(commit *devpilotv1alpha1.DevBoxCommit) metav1.Condition {
	for _, cond := range commit.Status.Conditions {
		if cond.Type == CommitConditionReady {
			return cond
		}
	}
	return metav1.Condition{}
}

var _ = Describe("DevBoxCommit frozen snapshots", func() {
	var (
		namespace string
		flusher   *fakeFlusher
		freezer   *fakeFreezer
	)

	BeforeEach(func() {
		namespace = fmt.Sprintf("freeze-%d", specCounter)
		specCounter++
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
		flusher = &fakeFlusher{}
		freezer = &fakeFreezer{namespace: namespace}
	})

	AfterEach(func() {
		cleanupOnlineCommit(namespace)
		cleanupVolume(namespace)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		_ = k8sClient.Delete(ctx, ns)
	})

	freezeSpec := func(spec *devpilotv1alpha1.DevBoxCommitSpec) {
		spec.Consistency = devpilotv1alpha1.ConsistencyFreeze
	}

	It("creates the snapshot inside the freeze and thaws after the cut", func() {
		key := newOnlineCommit(namespace, freezeSpec)
		reconciler := newFreezeReconciler(flusher, freezer)

		stop := make(chan struct{})
		defer close(stop)
		cutSnapshotWhenCreated(stop, namespace, freezer)

		commit := reconcileCommit(reconciler, key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseSnapshotting))
		Expect(commit.Status.Mode).To(Equal(devpilotv1alpha1.CommitModeOnlineSnapshot))
		Expect(commit.Status.SnapshotName).To(Equal(testCommitName + CommitSnapshotSuffix))
		Expect(readyCondition(commit).Message).To(ContainSubstring("consistency Freeze"))
		Expect(readyCondition(commit).Message).To(ContainSubstring("requested by spec.consistency"))

		Expect(freezer.Events()).To(Equal([]string{eventFreeze, "created", eventCut, eventThaw}))
		Expect(freezer.snapshotAtFreeze).To(BeFalse(), "the freeze must come before the snapshot")
		Expect(freezer.snapshotAtThaw).To(BeTrue(), "the thaw must come after the snapshot")
		Expect(freezer.standaloneThaws).To(BeZero())
		Expect(flusher.calls).To(BeZero(), "syncfs on a frozen filesystem would block")
		Expect(snapshotGone(namespace)).To(BeFalse())

		// The snapshot is the point in time from here on: no second freeze.
		markSnapshotReady(namespace)
		commit = reconcileCommit(reconciler, key)
		Expect(commit.Status.Message).To(ContainSubstring("Restoring"))
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: testCommitName + CommitCloneSuffix})
		commit = reconcileCommit(reconciler, key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseBuilding))
		Expect(getBuildJob(namespace).Spec.Template.Spec.Volumes).To(ContainElement(
			HaveField("VolumeSource.PersistentVolumeClaim.ClaimName", testCommitName+CommitCloneSuffix)))
		Expect(freezer.Events()).To(HaveLen(4))
		Expect(flusher.calls).To(BeZero())
	})

	It("promotes Sync to Freeze for a root disk on the loop driver", func() {
		key := newOnlineCommit(namespace)
		bindRootDiskToVolume(namespace, LoopCSIDriverName)
		reconciler := newFreezeReconciler(flusher, freezer)
		reconciler.snapshotCut = fakeCut(freezer, true)

		commit := reconcileCommit(reconciler, key)
		Expect(commit.Spec.Consistency).To(Equal(devpilotv1alpha1.ConsistencySync))
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseSnapshotting))
		Expect(freezer.Events()).To(Equal([]string{eventFreeze, eventCut, eventThaw}))
		Expect(flusher.calls).To(BeZero())
		Expect(readyCondition(commit).Message).To(SatisfyAll(
			ContainSubstring("promoted from Sync"),
			ContainSubstring(LoopCSIDriverName),
		))
		Expect(snapshotGone(namespace)).To(BeFalse())
	})

	It("keeps the Sync flush for a root disk on another driver", func() {
		key := newOnlineCommit(namespace)
		bindRootDiskToVolume(namespace, "rbd.csi.ceph.com")
		reconciler := newFreezeReconciler(flusher, freezer)
		reconciler.snapshotCut = fakeCut(freezer, true)

		commit := reconcileCommit(reconciler, key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseSnapshotting))
		Expect(flusher.calls).To(Equal(1))
		Expect(freezer.Events()).To(BeEmpty())
		Expect(snapshotGone(namespace)).To(BeFalse())
	})

	It("discards the snapshot when the helper thawed before the release", func() {
		key := newOnlineCommit(namespace, freezeSpec)
		freezer.thawErr = errors.New("freeze helper thawed on its own (timeout or signal) before the release")
		reconciler := newFreezeReconciler(flusher, freezer)
		reconciler.snapshotCut = fakeCut(freezer, true)

		commit := reconcileCommit(reconciler, key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseFailed))
		Expect(readyCondition(commit).Reason).To(Equal(CommitReasonFreezeFailed))
		Expect(commit.Status.Message).To(ContainSubstring("thawed on its own"))
		Expect(freezer.Events()).To(Equal([]string{eventFreeze, eventCut, eventThaw}))
		Expect(freezer.standaloneThaws).To(Equal(1), "a failed release falls back to --thaw")
		Expect(snapshotGone(namespace)).To(BeTrue(), "a snapshot not proven frozen must not survive")
		Expect(countBuildJobs(namespace)).To(BeZero())
	})

	It("discards the snapshot when the cut misses the freeze window", func() {
		key := newOnlineCommit(namespace, freezeSpec)
		reconciler := newFreezeReconciler(flusher, freezer)
		reconciler.Config.CommitFreezeTimeout = 200 * time.Millisecond
		reconciler.snapshotCut = fakeCut(freezer, false)

		commit := reconcileCommit(reconciler, key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseFailed))
		Expect(readyCondition(commit).Reason).To(Equal(CommitReasonFreezeFailed))
		Expect(commit.Status.Message).To(SatisfyAll(
			ContainSubstring("freeze window"),
			ContainSubstring("--commit-freeze-timeout"),
			ContainSubstring("offline"),
		))
		events := freezer.Events()
		Expect(events[0]).To(Equal(eventFreeze))
		Expect(events[len(events)-1]).To(Equal(eventThaw))
		Expect(events).To(ContainElement(eventCut))
		Expect(snapshotGone(namespace)).To(BeTrue())
		Expect(flusher.calls).To(BeZero())
	})

	It("fails without a snapshot when the freeze is refused", func() {
		key := newOnlineCommit(namespace, freezeSpec)
		freezer.freezeErr = errors.New("FIFREEZE root disk: device or resource busy")
		reconciler := newFreezeReconciler(flusher, freezer)
		reconciler.snapshotCut = fakeCut(freezer, true)

		commit := reconcileCommit(reconciler, key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseFailed))
		Expect(readyCondition(commit).Reason).To(Equal(CommitReasonFreezeFailed))
		Expect(commit.Status.Message).To(ContainSubstring("device or resource busy"))
		Expect(freezer.Events()).To(Equal([]string{eventFreeze}))
		Expect(freezer.standaloneThaws).To(Equal(1))
		Expect(snapshotGone(namespace)).To(BeTrue())
		Expect(flusher.calls).To(BeZero())
	})

	It("fails a Freeze commit when no freezer is configured", func() {
		key := newOnlineCommit(namespace, freezeSpec)
		reconciler := newFreezeReconciler(flusher, nil)

		commit := reconcileCommit(reconciler, key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseFailed))
		Expect(readyCondition(commit).Reason).To(Equal(CommitReasonFreezeFailed))
		Expect(snapshotGone(namespace)).To(BeTrue())
		Expect(flusher.calls).To(BeZero())
	})
})
