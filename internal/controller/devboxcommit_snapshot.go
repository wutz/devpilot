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
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// CommitSnapshotSuffix is appended to the commit name for the temporary
	// VolumeSnapshot of a running root disk.
	CommitSnapshotSuffix = "-snapshot"
	// CommitCloneSuffix is appended to the commit name for the read-only claim
	// restored from that snapshot.
	CommitCloneSuffix = "-ro"

	// requeueWhileSnapshotting bounds how long a snapshot and its restore wait
	// for the CSI driver to catch up.
	requeueWhileSnapshotting = 5 * time.Second
)

// RootDiskFlusher pushes the dirty pages of a running DevBox to its root disk
// so a volume snapshot sees every write the process already returned from. The
// default implementation execs into the pod; tests inject a fake.
type RootDiskFlusher interface {
	Flush(ctx context.Context, pod *corev1.Pod) error
}

// snapshotClass resolves the volume snapshot class for the root disk: the
// DevBox override wins, otherwise the operator-wide per-storageclass mapping.
func (r *DevBoxCommitReconciler) snapshotClass(box *devpilotv1alpha1.DevBox) (string, error) {
	if class := box.Spec.RootDisk.VolumeSnapshotClassName; class != "" {
		return class, nil
	}
	return r.Config.SnapshotClass(box.Spec.RootDisk.StorageClassName)
}

// resolveSource decides where the build Job reads the root disk from. The
// second result is set when the source is not available yet and the caller
// only waits: a root disk must never be read while a running pod still writes
// to it, so an online commit snapshots a clone instead.
func (r *DevBoxCommitReconciler) resolveSource(
	ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit, box *devpilotv1alpha1.DevBox,
) (commitSource, bool, error) {
	log := logf.FromContext(ctx)

	pod := &corev1.Pod{}
	podKey := types.NamespacedName{Namespace: box.Namespace, Name: box.Name + PodSuffix}
	switch err := r.Get(ctx, podKey, pod); {
	case isNotFound(err):
		pod = nil
	case err != nil:
		log.Error(err, "Could not read the DevBox pod", "devbox", box.Name)
		return commitSource{}, true, nil
	}

	// Once a snapshot is the source, the commit stays on it even if the pod
	// disappears meanwhile: the build reads one point in time, and switching to
	// the live claim would mix that point with later writes.
	online := commit.Status.Mode == devpilotv1alpha1.CommitModeOnlineSnapshot
	if pod == nil && !online {
		// Nothing holds the claim, so it can be mounted read-only directly.
		commit.Status.Mode = devpilotv1alpha1.CommitModeOfflinePVC
		return commitSource{PVCName: rootDiskClaimName(box), Mode: devpilotv1alpha1.CommitModeOfflinePVC}, false, nil
	}

	// Rebasing clears upper/ on the next start, which silently drops every write
	// made after the snapshot. Only an offline commit, where nothing writes,
	// may rebase.
	if commit.Spec.RebaseDevBox {
		_ = r.failCommit(ctx, commit, "RebaseOnLiveCommit",
			fmt.Errorf("rebaseDevBox is only allowed while no pod holds the root disk of %s: "+
				"stop the DevBox or set consistency to OfflineOnly", box.Name))

		// The phase is already Failed; reporting it as an error stops the caller
		// from treating the empty source as a resolved one.
		return commitSource{}, true, errors.New(commit.Status.Message)
	}

	if pod != nil && commit.Spec.Consistency == devpilotv1alpha1.ConsistencyOfflineOnly {
		commit.Status.Phase = devpilotv1alpha1.CommitPhasePending
		commit.Status.Message = fmt.Sprintf(
			"Waiting for the pod of %s to terminate; consistency is OfflineOnly", box.Name)
		return commitSource{}, true, nil
	}

	class, err := r.snapshotClass(box)
	if err != nil {
		// Stopping the DevBox is a valid answer too, so this waits rather than
		// failing: the offline path picks up as soon as the pod is gone.
		commit.Status.Phase = devpilotv1alpha1.CommitPhasePending
		commit.Status.Message = fmt.Sprintf(
			"Waiting for the pod of %s to terminate: %v; set rootDisk.volumeSnapshotClassName to commit it while it runs",
			box.Name, err)
		return commitSource{}, true, nil
	}

	pvcName, waiting, err := r.ensureSnapshotSource(ctx, commit, box, pod, class)
	if err != nil || waiting {
		return commitSource{}, true, err
	}
	return commitSource{PVCName: pvcName, Mode: devpilotv1alpha1.CommitModeOnlineSnapshot}, false, nil
}

// commitSource is the volume one build Job reads.
type commitSource struct {
	// PVCName is the claim mounted read-only at the root disk path.
	PVCName string
	// Mode records how the source was obtained.
	Mode devpilotv1alpha1.CommitMode
}

// rootDiskClaimName is the claim holding the DevBox root disk.
func rootDiskClaimName(box *devpilotv1alpha1.DevBox) string {
	if box.Status.RootDiskPVC != "" {
		return box.Status.RootDiskPVC
	}
	return box.Name + RootDiskPVCSuffix
}

// commitSnapshotName and commitClonePVCName derive the temporary objects from
// the commit name, which is unique per namespace.
func commitSnapshotName(commit *devpilotv1alpha1.DevBoxCommit) string {
	return commit.Name + CommitSnapshotSuffix
}

func commitClonePVCName(commit *devpilotv1alpha1.DevBoxCommit) string {
	return commit.Name + CommitCloneSuffix
}

// ensureSnapshotSource takes a snapshot of the running root disk and restores
// it into a read-only claim. Both steps are asynchronous in the CSI driver, so
// the function reports waiting=true until the clone is bound. The flush happens
// exactly once, right before the snapshot object is created: a snapshot that
// already exists is a point in time we must not disturb.
func (r *DevBoxCommitReconciler) ensureSnapshotSource(
	ctx context.Context,
	commit *devpilotv1alpha1.DevBoxCommit,
	box *devpilotv1alpha1.DevBox,
	pod *corev1.Pod,
	class string,
) (string, bool, error) {
	log := logf.FromContext(ctx)

	snap := &snapshotv1.VolumeSnapshot{}
	snapKey := types.NamespacedName{Namespace: commit.Namespace, Name: commitSnapshotName(commit)}
	switch err := r.Get(ctx, snapKey, snap); {
	case isNotFound(err):
		if err := r.flushRootDisk(ctx, pod); err != nil {
			_ = r.failCommit(ctx, commit, "FlushFailed",
				fmt.Errorf("flush the root disk of %s before snapshotting: %w", box.Name, err))
			// Returning the error keeps the caller from mistaking the empty
			// source for a ready one while still recording the terminal phase.
			return "", true, errors.New(commit.Status.Message)
		}
		created, err := r.createSnapshot(ctx, commit, box, class)
		if err != nil {
			if isAlreadyExists(err) {
				return "", true, nil
			}
			return "", false, fmt.Errorf("create volume snapshot %s: %w", snapKey.Name, err)
		}
		commit.Status.Mode = devpilotv1alpha1.CommitModeOnlineSnapshot
		commit.Status.Phase = devpilotv1alpha1.CommitPhaseSnapshotting
		commit.Status.SnapshotName = created.Name
		commit.Status.Message = fmt.Sprintf("Snapshotting the root disk of %s", box.Name)
		r.setCommitCondition(commit, metav1.ConditionFalse, "Snapshotting", commit.Status.Message)
		log.Info("Created root disk snapshot", "snapshot", created.Name, "class", class)
		return "", true, nil
	case err != nil:
		return "", false, fmt.Errorf("get volume snapshot %s: %w", snapKey.Name, err)
	}

	commit.Status.Mode = devpilotv1alpha1.CommitModeOnlineSnapshot
	commit.Status.SnapshotName = snap.Name
	// Status is a pointer and stays nil until the CSI snapshot controller first
	// touches the object, which is exactly the window right after Create.
	if snap.Status == nil || snap.Status.ReadyToUse == nil || !*snap.Status.ReadyToUse {
		commit.Status.Phase = devpilotv1alpha1.CommitPhaseSnapshotting
		commit.Status.Message = fmt.Sprintf("Waiting for volume snapshot %s to become ready", snap.Name)
		return "", true, nil
	}

	// A ready snapshot is the point in time the build must read, so a failed
	// restore is fatal instead of falling back to the live claim.
	pvcName, waiting, err := r.ensureClonePVC(ctx, commit, box)
	if err != nil {
		_ = r.failCommit(ctx, commit, "CloneFailed",
			fmt.Errorf("restore the root disk snapshot of %s: %w", box.Name, err))
		return "", true, errors.New(commit.Status.Message)
	}
	if waiting {
		return "", true, nil
	}
	return pvcName, false, nil
}

// flushRootDisk asks the running pod to push its page cache to the root disk.
// A missing flusher is only possible in a unit test, so it is not an error:
// production always wires the exec based one.
func (r *DevBoxCommitReconciler) flushRootDisk(ctx context.Context, pod *corev1.Pod) error {
	if r.Flusher == nil || pod == nil {
		// A snapshot taken after the pod is gone still captures a quiescent
		// disk: nothing flushed is lost because nothing is writing.
		return nil
	}
	return r.Flusher.Flush(ctx, pod)
}

// createSnapshot records the point in time copied into the clone claim.
func (r *DevBoxCommitReconciler) createSnapshot(
	ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit, box *devpilotv1alpha1.DevBox, class string,
) (*snapshotv1.VolumeSnapshot, error) {
	claim := rootDiskClaimName(box)
	snap := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      commitSnapshotName(commit),
			Namespace: commit.Namespace,
			Labels: map[string]string{
				LabelManagedBy:  ValueManagedBy,
				LabelDevBoxName: commit.Spec.DevBoxName,
				CommitJobLabel:  commit.Name,
			},
		},
		Spec: snapshotv1.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &class,
			Source: snapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: &claim,
			},
		},
	}
	if err := controllerutil.SetControllerReference(commit, snap, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// ensureClonePVC restores the snapshot into a claim the build Job can mount
// read-only. The CSI driver provisions it asynchronously, so this waits for the
// bound phase rather than assuming the restore finished.
func (r *DevBoxCommitReconciler) ensureClonePVC(
	ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit, box *devpilotv1alpha1.DevBox,
) (string, bool, error) {
	pvcName := commitClonePVCName(commit)
	pvc := &corev1.PersistentVolumeClaim{}
	switch err := r.Get(ctx, types.NamespacedName{Namespace: commit.Namespace, Name: pvcName}, pvc); {
	case isNotFound(err):
		source, err := r.rootDiskClaim(ctx, box)
		if err != nil {
			return "", false, err
		}
		created, err := r.clonePVC(commit, box, source)
		if err != nil {
			return "", false, r.failCommit(ctx, commit, "CloneFailed",
				fmt.Errorf("prepare the read-only clone of the root disk: %w", err))
		}
		if err := r.Create(ctx, created); err != nil {
			if !isAlreadyExists(err) {
				return "", false, fmt.Errorf("create clone claim %s: %w", pvcName, err)
			}
		}
		commit.Status.Phase = devpilotv1alpha1.CommitPhaseSnapshotting
		commit.Status.Message = fmt.Sprintf("Restoring the snapshot into %s", pvcName)
		return pvcName, true, nil
	case err != nil:
		return "", false, fmt.Errorf("get clone claim %s: %w", pvcName, err)
	}

	if pvc.Status.Phase != corev1.ClaimBound {
		commit.Status.Phase = devpilotv1alpha1.CommitPhaseSnapshotting
		commit.Status.Message = fmt.Sprintf("Waiting for the restore into %s to bind", pvcName)
		return pvcName, true, nil
	}
	// The commit reads this claim now, so record the mode that the rest of the
	// reconcile relies on to stay on the snapshot.
	commit.Status.Mode = devpilotv1alpha1.CommitModeOnlineSnapshot
	commit.Status.Phase = devpilotv1alpha1.CommitPhaseSnapshotting
	commit.Status.Message = fmt.Sprintf("Reading the restored root disk from %s", pvcName)
	return pvcName, false, nil
}

// rootDiskClaim reads the source claim, whose storage class and volume mode the
// clone must match.
func (r *DevBoxCommitReconciler) rootDiskClaim(
	ctx context.Context, box *devpilotv1alpha1.DevBox,
) (*corev1.PersistentVolumeClaim, error) {
	name := rootDiskClaimName(box)
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: box.Namespace, Name: name}, pvc); err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("root disk claim %s of devbox %s does not exist", name, box.Name)
		}
		return nil, fmt.Errorf("get root disk claim %s: %w", name, err)
	}
	return pvc, nil
}

// clonePVC describes the read-only restore target. A block volume is cloned as
// a block volume: a filesystem-mode clone of a block source is refused by the
// provisioner, and the commit binary needs the raw device either way.
func (r *DevBoxCommitReconciler) clonePVC(
	commit *devpilotv1alpha1.DevBoxCommit, box *devpilotv1alpha1.DevBox, source *corev1.PersistentVolumeClaim,
) (*corev1.PersistentVolumeClaim, error) {
	apiGroup := snapshotv1.GroupName
	storageClass := source.Spec.StorageClassName
	if storageClass == nil {
		storageClass = &box.Spec.RootDisk.StorageClassName
	}
	clone := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      commitClonePVCName(commit),
			Namespace: commit.Namespace,
			Labels: map[string]string{
				LabelManagedBy:  ValueManagedBy,
				LabelDevBoxName: commit.Spec.DevBoxName,
				CommitJobLabel:  commit.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      source.Spec.AccessModes,
			StorageClassName: storageClass,
			VolumeMode:       source.Spec.VolumeMode,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: source.Spec.Resources.Requests[corev1.ResourceStorage]},
			},
			DataSourceRef: &corev1.TypedObjectReference{
				APIGroup: &apiGroup,
				Kind:     "VolumeSnapshot",
				Name:     commitSnapshotName(commit),
			},
		},
	}
	if err := controllerutil.SetControllerReference(commit, clone, r.Scheme); err != nil {
		return nil, err
	}
	return clone, nil
}

// releaseEphemeralBestEffort releases the build Job and the temporary storage
// after a terminal phase, where a failure must not hide the result the commit
// already reached: the deletion is retried by the garbage collector through the
// owner references and by the finalizer on delete.
func (r *DevBoxCommitReconciler) releaseEphemeralBestEffort(
	ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit,
) {
	// The Job is released before the storage check below: an offline commit
	// has no temporary storage, yet its failed pod still holds a claim, and a
	// Job that outlives its commit keeps that claim in Terminating forever.
	if err := r.releaseBuildJob(ctx, commit); err != nil {
		logf.FromContext(ctx).Error(err, "Could not release the commit build Job", "commit", commit.Name)
	}
	if commit.Status.SnapshotName == "" && commit.Status.Mode != devpilotv1alpha1.CommitModeOnlineSnapshot {
		return
	}
	if err := r.releaseEphemeral(ctx, commit); err != nil {
		logf.FromContext(ctx).Error(err, "Could not release the temporary commit storage", "commit", commit.Name)
	}
}

// releaseBuildJob deletes the build Job with a background propagation policy,
// so its pod goes with it. A completed pod is what keeps the source claim from
// terminating, and the Job is not collected on its own: its owner is the
// commit, which stays around as a record.
func (r *DevBoxCommitReconciler) releaseBuildJob(
	ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit,
) error {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: buildJobName(commit), Namespace: commit.Namespace}}
	if err := r.Delete(ctx, job, client.PropagationPolicy(metav1DeleteBackground)); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete build job %s: %w", job.Name, err)
	}
	return nil
}

// releaseEphemeral deletes the temporary clone and snapshot. The claim goes
// first: a snapshot that is still the source of a claim cannot be deleted until
// that claim is gone.
func (r *DevBoxCommitReconciler) releaseEphemeral(ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit) error {
	var errs []error
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: commitClonePVCName(commit), Namespace: commit.Namespace},
	}
	if err := r.Delete(ctx, pvc); err != nil && !isNotFound(err) {
		errs = append(errs, fmt.Errorf("delete clone claim %s: %w", pvc.Name, err))
	}
	snap := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: commitSnapshotName(commit), Namespace: commit.Namespace},
	}
	if err := r.Delete(ctx, snap); err != nil && !isNotFound(err) && !apierrors.IsForbidden(err) {
		errs = append(errs, fmt.Errorf("delete volume snapshot %s: %w", snap.Name, err))
	}
	return errors.Join(errs...)
}
