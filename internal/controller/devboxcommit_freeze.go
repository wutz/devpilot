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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// LoopCSIDriverName is the CSI driver that keeps each root disk as an
	// ext4 disk.img on shared storage. The commit build reads its snapshots
	// with a userspace ext4 reader that cannot replay a journal, so a snapshot
	// of a mounted loop volume must be taken while the filesystem is frozen.
	LoopCSIDriverName = "loop.devpilot.wutz.dev"

	// CommitReasonFreezeFailed is the Ready condition reason when the freeze
	// around an online snapshot did not hold.
	CommitReasonFreezeFailed = "FreezeFailed"

	// defaultCutPollInterval spaces the snapshot status checks while frozen.
	defaultCutPollInterval = 500 * time.Millisecond
	// freezeCleanupTimeout bounds the thaw and the snapshot delete that run
	// after a freeze, even when the reconcile context is already cancelled.
	freezeCleanupTimeout = 30 * time.Second
)

// effectiveConsistency resolves how the snapshot of a running root disk is
// made consistent. Sync is promoted to Freeze for loop-backed root disks: a
// merely synced ext4 image still has needs_recovery set and an unreplayed
// journal, which the commit build cannot read. The second result explains a
// Freeze, for the status message.
func (r *DevBoxCommitReconciler) effectiveConsistency(
	ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit, box *devpilotv1alpha1.DevBox,
) (devpilotv1alpha1.CommitConsistency, string, error) {
	switch commit.Spec.Consistency {
	case devpilotv1alpha1.ConsistencyFreeze:
		return devpilotv1alpha1.ConsistencyFreeze, "requested by spec.consistency", nil
	case "", devpilotv1alpha1.ConsistencySync:
		loop, err := r.rootDiskOnLoopDriver(ctx, box)
		if err != nil {
			return "", "", err
		}
		if loop {
			return devpilotv1alpha1.ConsistencyFreeze, fmt.Sprintf(
				"promoted from Sync because the root disk is served by %s, whose snapshots must be clean ext4 images",
				LoopCSIDriverName), nil
		}
		return devpilotv1alpha1.ConsistencySync, "", nil
	default:
		return commit.Spec.Consistency, "", nil
	}
}

// rootDiskOnLoopDriver reports whether the root disk claim is bound to a
// volume of the loop CSI driver. A claim or volume that cannot be found is not
// a loop volume; any other read error is returned so the reconcile retries
// instead of guessing the consistency.
func (r *DevBoxCommitReconciler) rootDiskOnLoopDriver(ctx context.Context, box *devpilotv1alpha1.DevBox) (bool, error) {
	name := rootDiskClaimName(box)
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: box.Namespace, Name: name}, pvc); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get root disk claim %s: %w", name, err)
	}
	if pvc.Spec.VolumeName == "" {
		return false, nil
	}
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get root disk volume %s: %w", pvc.Spec.VolumeName, err)
	}
	return pv.Spec.CSI != nil && pv.Spec.CSI.Driver == LoopCSIDriverName, nil
}

// snapshotFrozen takes the snapshot of a running root disk inside a freeze:
// freeze, create the VolumeSnapshot, wait until the CSI driver has cut it,
// thaw. A snapshot is kept only if the cut was confirmed while the helper
// still held the freeze; on every other outcome it is deleted and the commit
// fails, because its content may be any mix of before and after the thaw.
//
// The freeze blocks this reconcile for at most the freeze window plus the thaw
// timeout, which is the price of a consistent cut.
func (r *DevBoxCommitReconciler) snapshotFrozen(
	ctx context.Context,
	commit *devpilotv1alpha1.DevBoxCommit,
	box *devpilotv1alpha1.DevBox,
	pod *corev1.Pod,
	class string,
	why string,
) (string, bool, error) {
	log := logf.FromContext(ctx)
	snapKey := types.NamespacedName{Namespace: commit.Namespace, Name: commitSnapshotName(commit)}
	consistencyNote := fmt.Sprintf("consistency Freeze, %s", why)

	// The cache may lag behind a snapshot an earlier reconcile created; ask
	// the API server before freezing a DevBox for nothing.
	if err := r.apiReader().Get(ctx, snapKey, &snapshotv1.VolumeSnapshot{}); err == nil {
		return "", true, nil
	} else if !isNotFound(err) {
		return "", false, fmt.Errorf("get volume snapshot %s: %w", snapKey.Name, err)
	}

	// Without a pod nothing has the filesystem mounted, so it is already as
	// clean as a freeze would make it.
	if pod == nil {
		created, err := r.createSnapshot(ctx, commit, box, class)
		if err != nil {
			if isAlreadyExists(err) {
				return "", true, nil
			}
			return "", false, fmt.Errorf("create volume snapshot %s: %w", snapKey.Name, err)
		}
		r.recordSnapshotCreated(commit, created, fmt.Sprintf(
			"Snapshotting the root disk of %s (%s; no pod, nothing to freeze)", box.Name, consistencyNote))
		log.Info("Created root disk snapshot", "snapshot", created.Name, "class", class, "consistency", "Freeze")
		return "", true, nil
	}

	if r.Freezer == nil {
		return r.failFreeze(ctx, commit, box, fmt.Errorf(
			"%s, but the operator has no root disk freezer configured", consistencyNote))
	}

	window := r.Config.CommitFreezeWindow()
	frozenAt := time.Now()
	session, err := r.Freezer.Freeze(ctx, pod)
	if err != nil {
		r.thawBestEffort(ctx, pod)
		return r.failFreeze(ctx, commit, box, fmt.Errorf("freeze the root disk of %s (%s): %w", box.Name, consistencyNote, err))
	}
	log.Info("Froze the root disk", "devbox", box.Name, "pod", pod.Name, "window", window.String())

	var (
		created *snapshotv1.VolumeSnapshot
		kept    bool
		thawed  bool
		thawErr error
	)
	// release thaws exactly once. It runs on a context detached from the
	// reconcile, so a cancelled reconcile still releases the DevBox.
	release := func() error {
		if thawed {
			return thawErr
		}
		thawed = true
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), freezeCleanupTimeout)
		defer cancel()
		if thawErr = session.Thaw(cleanupCtx); thawErr != nil {
			r.thawBestEffort(cleanupCtx, pod)
		}
		log.Info("Thawed the root disk", "devbox", box.Name, "frozenFor", time.Since(frozenAt).String(),
			"released", thawErr == nil)
		return thawErr
	}
	// The deferred cleanup covers every exit, panics included: the DevBox
	// never stays frozen, and an unconfirmed snapshot never survives.
	defer func() {
		_ = release()
		if created != nil && !kept {
			r.deleteSnapshotBestEffort(ctx, created)
		}
	}()
	// discard thaws first, so the DevBox is released before any further API
	// call, then drops the snapshot and fails the commit.
	discard := func(cause error) (string, bool, error) {
		_ = release()
		if created != nil {
			r.deleteSnapshotBestEffort(ctx, created)
			created = nil
		}
		return r.failFreeze(ctx, commit, box, cause)
	}

	created, err = r.createSnapshot(ctx, commit, box, class)
	if err != nil {
		created = nil
		if isAlreadyExists(err) {
			// Created by an earlier reconcile the API read above raced with.
			// That one is not ours to judge or delete.
			return "", true, nil
		}
		return "", false, fmt.Errorf("create volume snapshot %s: %w", snapKey.Name, err)
	}

	cut, err := r.waitSnapshotCut(ctx, snapKey, frozenAt.Add(window))
	switch {
	case err != nil:
		return discard(fmt.Errorf("wait for volume snapshot %s while the root disk of %s was frozen: %w",
			snapKey.Name, box.Name, err))
	case !cut:
		return discard(fmt.Errorf("the CSI driver did not cut volume snapshot %s within the %s freeze window",
			snapKey.Name, window))
	}
	if err := release(); err != nil {
		return discard(fmt.Errorf("the root disk of %s was not frozen for the whole snapshot: %w", box.Name, err))
	}
	kept = true

	r.recordSnapshotCreated(commit, created, fmt.Sprintf("Snapshotting the root disk of %s (%s)", box.Name, consistencyNote))
	log.Info("Created root disk snapshot", "snapshot", created.Name, "class", class, "consistency", "Freeze",
		"frozenFor", time.Since(frozenAt).String())
	return "", true, nil
}

// recordSnapshotCreated moves the commit to Snapshotting on a new snapshot.
// The condition keeps the message, including the effective consistency, until
// the build starts.
func (r *DevBoxCommitReconciler) recordSnapshotCreated(
	commit *devpilotv1alpha1.DevBoxCommit, snap *snapshotv1.VolumeSnapshot, message string,
) {
	commit.Status.Mode = devpilotv1alpha1.CommitModeOnlineSnapshot
	commit.Status.Phase = devpilotv1alpha1.CommitPhaseSnapshotting
	commit.Status.SnapshotName = snap.Name
	commit.Status.Message = message
	r.setCommitCondition(commit, metav1.ConditionFalse, "Snapshotting", message)
}

// failFreeze fails the commit with an actionable FreezeFailed. The error is
// returned so the caller does not take the empty source for a ready one.
func (r *DevBoxCommitReconciler) failFreeze(
	ctx context.Context, commit *devpilotv1alpha1.DevBoxCommit, box *devpilotv1alpha1.DevBox, cause error,
) (string, bool, error) {
	_ = r.failCommit(ctx, commit, CommitReasonFreezeFailed, fmt.Errorf(
		"%w; no snapshot was kept. If the snapshot class is slow while frozen (for example a loop snapshot "+
			"strategy that copies disk.img), stop %s and commit it offline, or raise --commit-freeze-timeout (now %s)",
		cause, box.Name, r.Config.CommitFreezeWindow()))
	return "", true, errors.New(commit.Status.Message)
}

// waitSnapshotCut polls until the CSI driver has cut the snapshot or the
// deadline passes. It always checks at least once.
func (r *DevBoxCommitReconciler) waitSnapshotCut(
	ctx context.Context, key types.NamespacedName, deadline time.Time,
) (bool, error) {
	check := r.snapshotCut
	if check == nil {
		check = r.snapshotCutFromAPI
	}
	interval := r.cutPollInterval
	if interval <= 0 {
		interval = defaultCutPollInterval
	}
	for {
		cut, err := check(ctx, key)
		if err != nil || cut {
			return cut, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}
		timer := time.NewTimer(min(interval, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

// snapshotCutFromAPI reads the snapshot from the API server, not the cache:
// the freeze window is short and a stale read only prolongs it. creationTime
// is set once the driver's CreateSnapshot returned, which is the point in time
// the snapshot captures; readyToUse implies it.
func (r *DevBoxCommitReconciler) snapshotCutFromAPI(ctx context.Context, key types.NamespacedName) (bool, error) {
	snap := &snapshotv1.VolumeSnapshot{}
	if err := r.apiReader().Get(ctx, key, snap); err != nil {
		if isNotFound(err) {
			return false, fmt.Errorf("volume snapshot %s disappeared", key.Name)
		}
		return false, fmt.Errorf("get volume snapshot %s: %w", key.Name, err)
	}
	status := snap.Status
	if status == nil {
		return false, nil
	}
	if status.Error != nil && status.Error.Message != nil {
		return false, fmt.Errorf("volume snapshot %s failed: %s", key.Name, *status.Error.Message)
	}
	return status.CreationTime != nil || (status.ReadyToUse != nil && *status.ReadyToUse), nil
}

func (r *DevBoxCommitReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// thawBestEffort runs the standalone thaw. Its error is only logged: it is a
// safety net behind the session's own release.
func (r *DevBoxCommitReconciler) thawBestEffort(ctx context.Context, pod *corev1.Pod) {
	if r.Freezer == nil || pod == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), freezeCleanupTimeout)
	defer cancel()
	if err := r.Freezer.Thaw(cleanupCtx, pod); err != nil {
		logf.FromContext(ctx).Error(err, "Could not run the standalone root disk thaw", "pod", pod.Name)
	}
}

// deleteSnapshotBestEffort drops a snapshot whose cut was not confirmed while
// frozen. A failure is logged: the commit owns the snapshot, so the garbage
// collector and the finalizer still remove it.
func (r *DevBoxCommitReconciler) deleteSnapshotBestEffort(ctx context.Context, snap *snapshotv1.VolumeSnapshot) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), freezeCleanupTimeout)
	defer cancel()
	if err := r.Delete(cleanupCtx, snap); err != nil && !isNotFound(err) {
		logf.FromContext(ctx).Error(err, "Could not delete the unconfirmed root disk snapshot", "snapshot", snap.Name)
	}
}
