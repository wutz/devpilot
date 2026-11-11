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

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	testSnapshotClass = "snapshot-class"
	testDevBoxRunning = "devbox-running"
)

// fakeFlusher records how often the root disk was flushed and can fail on
// demand. envtest has no pod to exec into, so the real flusher is not usable.
type fakeFlusher struct {
	calls int
	err   error
}

func (f *fakeFlusher) Flush(_ context.Context, _ *corev1.Pod) error {
	f.calls++
	return f.err
}

// newOnlineCommit creates a DevBox whose pod exists and a commit that targets
// it, so the reconciler takes the snapshot path.
func newOnlineCommit(namespace string, mutate ...func(*devpilotv1alpha1.DevBoxCommitSpec)) types.NamespacedName {
	devboxName := testDevBoxRunning
	box := newTestDevBox(devboxName, namespace)
	// The per-storageclass mapping is the default path; the override is covered
	// separately.
	box.Spec.RootDisk.VolumeSnapshotClassName = ""
	Expect(k8sClient.Create(ctx, box)).To(Succeed())

	pvcName := devboxName + RootDiskPVCSuffix
	Expect(k8sClient.Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: new(box.Spec.RootDisk.StorageClassName),
			VolumeMode:       new(corev1.PersistentVolumeBlock),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(testVolumeSize)},
			},
		},
	})).To(Succeed())

	box.Status.Phase = devpilotv1alpha1.PhaseRunning
	box.Status.RootDiskPVC = pvcName
	Expect(k8sClient.Status().Update(ctx, box)).To(Succeed())

	Expect(k8sClient.Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: devboxName + PodSuffix, Namespace: namespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: MainContainerName, Image: testDevBoxImage}},
		},
	})).To(Succeed())

	commit := &devpilotv1alpha1.DevBoxCommit{
		ObjectMeta: metav1.ObjectMeta{Name: testCommitName, Namespace: namespace},
		Spec: devpilotv1alpha1.DevBoxCommitSpec{
			DevBoxName: devboxName,
			Destination: devpilotv1alpha1.CommitDestination{
				Repository: testCommitRepository,
				Tags:       []string{testCommitTag},
			},
			Compression: devpilotv1alpha1.CompressionZstd,
		},
	}
	// The spec is immutable after create, so a variant has to be applied here.
	for _, apply := range mutate {
		apply(&commit.Spec)
	}
	Expect(k8sClient.Create(ctx, commit)).To(Succeed())
	return client.ObjectKeyFromObject(commit)
}

// newSnapshotReconciler wires the per-storageclass snapshot class mapping and a
// flusher, which the offline tests never need.
func newSnapshotReconciler(flusher RootDiskFlusher) *DevBoxCommitReconciler {
	r := newCommitReconciler("")
	r.Config.DefaultSnapshotClasses = map[string]string{testStorageClassNVMe: testSnapshotClass}
	r.Flusher = flusher
	return r
}

func getSnapshot(namespace string) *snapshotv1.VolumeSnapshot {
	snap := &snapshotv1.VolumeSnapshot{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Namespace: namespace, Name: testCommitName + CommitSnapshotSuffix,
	}, snap)).To(Succeed())
	return snap
}

func getClone(namespace string) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Namespace: namespace, Name: testCommitName + CommitCloneSuffix,
	}, pvc)).To(Succeed())
	return pvc
}

// markSnapshotReady stands in for the CSI snapshot controller, which envtest
// does not run.
func markSnapshotReady(namespace string) {
	snap := getSnapshot(namespace)
	ready := true
	// Status is a pointer and is nil until the controller first writes it.
	if snap.Status == nil {
		snap.Status = &snapshotv1.VolumeSnapshotStatus{}
	}
	snap.Status.ReadyToUse = &ready
	Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())
}

func countBuildJobs(namespace string) int {
	jobs := &batchv1.JobList{}
	Expect(k8sClient.List(ctx, jobs, client.InNamespace(namespace))).To(Succeed())
	return len(jobs.Items)
}

func getBuildJob(namespace string) *batchv1.Job {
	job := &batchv1.Job{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Namespace: namespace, Name: testCommitName + CommitJobSuffix,
	}, job)).To(Succeed())
	return job
}

// snapshotGone reports whether the temporary snapshot was deleted.
func snapshotGone(namespace string) bool {
	err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: namespace, Name: testCommitName + CommitSnapshotSuffix,
	}, &snapshotv1.VolumeSnapshot{})
	return err != nil && isNotFound(err)
}

func cleanupOnlineCommit(namespace string) {
	commit := &devpilotv1alpha1.DevBoxCommit{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: testCommitName}, commit); err == nil {
		controllerutil.RemoveFinalizer(commit, CommitFinalizer)
		_ = k8sClient.Update(ctx, commit)
		_ = k8sClient.Delete(ctx, commit)
	}
	_ = k8sClient.DeleteAllOf(ctx, &snapshotv1.VolumeSnapshot{}, client.InNamespace(namespace))
	_ = k8sClient.DeleteAllOf(ctx, &corev1.PersistentVolumeClaim{}, client.InNamespace(namespace))
	_ = k8sClient.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(namespace))
	cleanupOfflineCommit(namespace)
}

var _ = Describe("DevBoxCommit online snapshots", func() {
	var (
		namespace string
		flusher   *fakeFlusher
	)

	BeforeEach(func() {
		namespace = fmt.Sprintf("online-%d", specCounter)
		specCounter++
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
		flusher = &fakeFlusher{}
	})

	AfterEach(func() {
		cleanupOnlineCommit(namespace)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		_ = k8sClient.Delete(ctx, ns)
	})

	It("flushes once and snapshots the running root disk", func() {
		key := newOnlineCommit(namespace)
		reconciler := newSnapshotReconciler(flusher)

		commit := reconcileCommit(reconciler, key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseSnapshotting))
		Expect(commit.Status.Mode).To(Equal(devpilotv1alpha1.CommitModeOnlineSnapshot))
		Expect(commit.Status.SnapshotName).To(Equal(testCommitName + CommitSnapshotSuffix))
		Expect(flusher.calls).To(Equal(1))
		Expect(countBuildJobs(namespace)).To(Equal(0), "the build must wait for the clone")

		snap := getSnapshot(namespace)
		Expect(snap.Spec.VolumeSnapshotClassName).To(HaveValue(Equal(testSnapshotClass)))
		Expect(snap.Spec.Source.PersistentVolumeClaimName).To(HaveValue(Equal(testDevBoxRunning + RootDiskPVCSuffix)))
		Expect(snap.OwnerReferences).To(HaveLen(1))
		Expect(snap.OwnerReferences[0].Name).To(Equal(testCommitName))

		// The snapshot is a point in time: a second reconcile must not flush
		// again, and it must not take another snapshot.
		reconcileCommit(reconciler, key)
		Expect(flusher.calls).To(Equal(1))
	})

	It("restores the ready snapshot into a read-only clone", func() {
		key := newOnlineCommit(namespace)
		reconciler := newSnapshotReconciler(flusher)
		reconcileCommit(reconciler, key)

		markSnapshotReady(namespace)
		commit := reconcileCommit(reconciler, key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseSnapshotting))
		Expect(commit.Status.Message).To(ContainSubstring("Restoring"))
		Expect(countBuildJobs(namespace)).To(Equal(0), "the clone must bind before the build starts")

		clone := getClone(namespace)
		Expect(clone.Spec.DataSourceRef).NotTo(BeNil())
		Expect(clone.Spec.DataSourceRef.Kind).To(Equal("VolumeSnapshot"))
		Expect(clone.Spec.DataSourceRef.Name).To(Equal(testCommitName + CommitSnapshotSuffix))
		Expect(clone.Spec.DataSourceRef.APIGroup).To(HaveValue(Equal(snapshotv1.GroupName)))
		Expect(clone.Spec.VolumeMode).To(HaveValue(Equal(corev1.PersistentVolumeBlock)))
		Expect(clone.Spec.StorageClassName).To(HaveValue(Equal(testStorageClassNVMe)))
	})

	It("builds from the clone once it binds", func() {
		key := newOnlineCommit(namespace)
		reconciler := newSnapshotReconciler(flusher)
		reconcileCommit(reconciler, key)
		markSnapshotReady(namespace)
		reconcileCommit(reconciler, key)

		// A clone without a volume name is not bound, so the commit waits.
		commit := reconcileCommit(reconciler, key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseSnapshotting))
		Expect(commit.Status.Message).To(ContainSubstring("bind"))
		Expect(countBuildJobs(namespace)).To(Equal(0))

		markClaimBound(types.NamespacedName{Namespace: namespace, Name: testCommitName + CommitCloneSuffix})

		commit = reconcileCommit(reconciler, key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseBuilding))
		Expect(commit.Status.Mode).To(Equal(devpilotv1alpha1.CommitModeOnlineSnapshot))

		job := getBuildJob(namespace)
		Expect(job.Spec.Template.Spec.Volumes).To(ContainElement(corev1.Volume{
			Name: RootDiskVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: testCommitName + CommitCloneSuffix,
					ReadOnly:  true,
				},
			},
		}), "the build must read the clone, never the live root disk")
	})

	It("releases the clone and snapshot when the build succeeds", func() {
		key := newOnlineCommit(namespace)
		reconciler := newSnapshotReconciler(flusher)
		reconcileCommit(reconciler, key)
		markSnapshotReady(namespace)
		reconcileCommit(reconciler, key)
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: testCommitName + CommitCloneSuffix})
		reconcileCommit(reconciler, key)

		completeBuildJob(getBuildJob(namespace), fmt.Sprintf(
			`{"manifestDigest":%q,"images":[%q]}`,
			testCommitDigest, testCommitRepository+"@"+testCommitDigest))

		commit := reconcileCommit(reconciler, key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseSucceeded))
		Expect(commit.Status.SnapshotName).To(Equal(testCommitName + CommitSnapshotSuffix))

		// envtest has no kubelet to end PVC protection, so the claim may linger
		// with a deletion timestamp; being marked for deletion is the release.
		Eventually(func() bool {
			pvc := &corev1.PersistentVolumeClaim{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace, Name: testCommitName + CommitCloneSuffix,
			}, pvc)
			return isNotFound(err) || !pvc.DeletionTimestamp.IsZero()
		}).Should(BeTrue(), "the clone claim must be released after a successful build")
		Eventually(func() bool {
			return snapshotGone(namespace)
		}).Should(BeTrue(), "the snapshot must be released after a successful build")
	})

	It("releases the clone and snapshot when the build fails", func() {
		key := newOnlineCommit(namespace)
		reconciler := newSnapshotReconciler(flusher)
		reconcileCommit(reconciler, key)
		markSnapshotReady(namespace)
		reconcileCommit(reconciler, key)
		markClaimBound(types.NamespacedName{Namespace: namespace, Name: testCommitName + CommitCloneSuffix})
		reconcileCommit(reconciler, key)

		completeBuildJob(getBuildJob(namespace), `{"error":"the registry refused the push"}`)

		commit := reconcileCommit(reconciler, key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseFailed))
		Eventually(func() bool {
			return snapshotGone(namespace)
		}).Should(BeTrue(), "a failed build must not leak the snapshot")
	})

	It("releases the temporary storage when a commit is deleted mid-flight", func() {
		key := newOnlineCommit(namespace)
		reconciler := newSnapshotReconciler(flusher)
		reconcileCommit(reconciler, key)
		markSnapshotReady(namespace)
		reconcileCommit(reconciler, key)

		commit := &devpilotv1alpha1.DevBoxCommit{}
		Expect(k8sClient.Get(ctx, key, commit)).To(Succeed())
		Expect(k8sClient.Delete(ctx, commit)).To(Succeed())
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(snapshotGone(namespace)).To(BeTrue())
	})

	It("waits instead of snapshotting when consistency is OfflineOnly", func() {
		key := newOnlineCommit(namespace, func(spec *devpilotv1alpha1.DevBoxCommitSpec) {
			spec.Consistency = devpilotv1alpha1.ConsistencyOfflineOnly
		})

		commit := reconcileCommit(newSnapshotReconciler(flusher), key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhasePending))
		Expect(commit.Status.Message).To(ContainSubstring("OfflineOnly"))
		Expect(flusher.calls).To(BeZero())
		Expect(snapshotGone(namespace)).To(BeTrue())
	})

	It("uses the DevBox snapshot class override", func() {
		key := newOnlineCommit(namespace)
		box := &devpilotv1alpha1.DevBox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: testDevBoxRunning,
		}, box)).To(Succeed())
		box.Spec.RootDisk.VolumeSnapshotClassName = "override-class"
		Expect(k8sClient.Update(ctx, box)).To(Succeed())

		// No mapping is configured: the override alone must be enough.
		reconciler := newCommitReconciler("")
		reconciler.Flusher = flusher
		reconcileCommit(reconciler, key)

		Expect(getSnapshot(namespace).Spec.VolumeSnapshotClassName).To(HaveValue(Equal("override-class")))
	})

	It("rejects rebasing a running DevBox", func() {
		key := newOnlineCommit(namespace, func(spec *devpilotv1alpha1.DevBoxCommitSpec) {
			spec.RebaseDevBox = true
		})

		commit := reconcileCommit(newSnapshotReconciler(flusher), key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseFailed))
		Expect(commit.Status.Conditions).To(ContainElement(HaveField("Reason", "RebaseOnLiveCommit")))
		Expect(flusher.calls).To(BeZero())
		Expect(snapshotGone(namespace)).To(BeTrue(),
			"a refused commit must not leave a snapshot behind")
	})

	It("fails instead of snapshotting an unflushed disk", func() {
		key := newOnlineCommit(namespace)
		flusher.err = errors.New("helper exited 1")

		commit := reconcileCommit(newSnapshotReconciler(flusher), key)
		Expect(commit.Status.Phase).To(Equal(devpilotv1alpha1.CommitPhaseFailed))
		Expect(commit.Status.Conditions).To(ContainElement(HaveField("Reason", "FlushFailed")))
		Expect(snapshotGone(namespace)).To(BeTrue())
	})
})
