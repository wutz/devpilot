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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// AnnotationLastActive is the heartbeat a console or the platform refreshes
	// while somebody works on the devbox. It is an RFC 3339 timestamp; a
	// devbox whose heartbeat is older than spec.lifecycle.idleShutdownMinutes
	// is shut down.
	AnnotationLastActive = "devpilot.wutz.dev/last-active"

	// LabelExpireAt marks a root disk that has left its devbox for the
	// volumetrash recycler, which deletes the volume once the buffer expires.
	// The value is the instant the disk became eligible in the compact RFC 3339
	// layout: a label value cannot carry the colons of the usual one.
	LabelExpireAt = "volumetrash.wutz.dev/expire-at"

	// expireAtLayout is the label-safe form of a timestamp: 20260131T235959Z.
	expireAtLayout = "20060102T150405Z"

	// ConditionRetentionExpiring tracks the countdown to
	// status.scheduledReleaseTime. Its reason is the last threshold that was
	// announced, which is also what keeps each warning Event to one per
	// threshold.
	ConditionRetentionExpiring = "RetentionExpiring"

	// reasonExpiring72h and reasonExpiring24h are the two warning marks before
	// an automatic release.
	reasonExpiring72h = "ExpiringIn72h"
	reasonExpiring24h = "ExpiringIn24h"

	// reasonIdleTimeout explains a shutdown the operator performed itself.
	reasonIdleTimeout = "IdleTimeout"

	// retentionWarn72h and retentionWarn24h are when the platform is warned
	// that a devbox is about to be released.
	retentionWarn72h = 72 * time.Hour
	retentionWarn24h = 24 * time.Hour

	// automationRequeue caps how far ahead the controller plans a wake-up for
	// idle shutdown or retention. Every wake is one read of one devbox, so an
	// hourly cap bounds that cost while still re-deriving a two week window
	// from the clock rather than trusting a timer set weeks ago.
	automationRequeue = time.Hour
)

// shutdownIfIdle stops a running devbox nobody has used for
// spec.lifecycle.idleShutdownMinutes. It reports whether it changed the spec;
// the caller patches status and returns.
func (r *DevBoxReconciler) shutdownIfIdle(ctx context.Context, box *devpilotv1alpha1.DevBox, now time.Time) (bool, error) {
	if box.Status.Phase != devpilotv1alpha1.PhaseRunning {
		return false, nil
	}
	minutes := box.Spec.Lifecycle.IdleShutdownMinutes
	if minutes <= 0 {
		return false, nil
	}
	last, ok := lastActivity(box)
	if !ok || now.Before(last.Add(time.Duration(minutes)*time.Minute)) {
		return false, nil
	}

	// The segment closes here, while the reconcile still knows why the devbox
	// is going down: once the pod is gone the phase is a plain Stopped, and
	// settling the ledger there would bill the run as a user stop.
	closeSegment(box, now, devpilotv1alpha1.EndReasonIdleTimeout)
	// settleBilling is skipped for this reconcile, so the total the closed
	// segment contributes is derived here rather than a reconcile later.
	box.Status.TotalCost = totalCost(box)
	message := fmt.Sprintf("No activity for %d minutes; shutting the devbox down", minutes)
	box.Spec.DesiredState = devpilotv1alpha1.DesiredStateStopped
	box.Status.Phase = devpilotv1alpha1.PhaseStopping
	box.Status.Message = message
	r.setCondition(box, ConditionReady, metav1ConditionFalse, reasonIdleTimeout, message)
	r.eventf(box, corev1.EventTypeNormal, "IdleShutdown", message)

	// The spec change and the status mutations travel separately: the update
	// carries desiredState, and the caller's status patch carries the phase.
	status := box.Status.DeepCopy()
	if err := r.Update(ctx, box); err != nil {
		return false, fmt.Errorf("shut down idle devbox: %w", err)
	}
	box.Status = *status
	logf.FromContext(ctx).Info("DevBox idle shutdown", "devbox", box.Name, "idleMinutes", minutes)
	return true, nil
}

// lastActivity is the latest instant the devbox is known to have been used: the
// heartbeat a console refreshes, or the start of the open segment when no
// heartbeat was ever written. The later of the two wins, so a pod that just
// started is active as of its own segment and a stale heartbeat left over from
// the previous run cannot shut it down straight away.
func lastActivity(box *devpilotv1alpha1.DevBox) (time.Time, bool) {
	var last time.Time
	found := false
	if raw := strings.TrimSpace(box.Annotations[AnnotationLastActive]); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			last, found = parsed.UTC(), true
		}
	}
	if segment := openBillingSegment(box); segment != nil {
		if start := segment.StartTime.Time; !found || start.After(last) {
			last, found = start, true
		}
	}
	return last, found
}

// settleRetention keeps the release countdown in step with the non-running
// streak, and reports whether the devbox has reached its release time. The
// caller persists the status that says so before deleting anything, so a
// release that fails part way is visible as an expired devbox rather than a
// devbox that quietly disappeared.
//
// Only Stopped and Interrupted count: a devbox that is starting, stopping or
// failing is not idle, it is busy doing something the user asked for.
func (r *DevBoxReconciler) settleRetention(box *devpilotv1alpha1.DevBox, now time.Time) bool {
	switch box.Status.Phase {
	case devpilotv1alpha1.PhaseStopped, devpilotv1alpha1.PhaseInterrupted:
	default:
		return false
	}
	if box.Status.NonRunningSince == nil {
		// The pointer lives in the status, not in this stack frame: a local
		// metav1.Time would dangle as soon as settleRetention returns.
		box.Status.NonRunningSince = new(metav1.NewTime(now.UTC()))
	}
	days := box.Spec.RootDisk.RetentionDaysAfterStop
	if days <= 0 {
		box.Status.ScheduledReleaseTime = nil
		r.clearCondition(box, ConditionRetentionExpiring)
		return false
	}
	releaseAt := box.Status.NonRunningSince.Add(time.Duration(days) * 24 * time.Hour)
	box.Status.ScheduledReleaseTime = new(metav1.NewTime(releaseAt.UTC()))
	if !now.Before(releaseAt) {
		return true
	}
	r.warnRetention(box, releaseAt.Sub(now), releaseAt)
	return false
}

// warnRetention publishes the warning at each threshold exactly once. The
// condition records which threshold was announced, so a reconcile that finds
// the same verdict already recorded stays quiet; a devbox that runs again and
// later idles out a second time announces both thresholds again.
func (r *DevBoxReconciler) warnRetention(box *devpilotv1alpha1.DevBox, remaining time.Duration, releaseAt time.Time) {
	reason := ""
	switch {
	case remaining <= retentionWarn24h:
		reason = reasonExpiring24h
	case remaining <= retentionWarn72h:
		reason = reasonExpiring72h
	}
	if reason == "" {
		r.clearCondition(box, ConditionRetentionExpiring)
		return
	}
	announced := false
	if cond := conditionOf(box, ConditionRetentionExpiring); cond != nil && cond.Status == metav1.ConditionTrue && cond.Reason == reason {
		announced = true
	}
	message := fmt.Sprintf("The devbox has not run since %s and will be released at %s unless it is started",
		box.Status.NonRunningSince.UTC().Format(time.RFC3339), releaseAt.UTC().Format(time.RFC3339))
	r.setCondition(box, ConditionRetentionExpiring, metav1ConditionTrue, reason, message)
	if !announced {
		r.eventf(box, corev1.EventTypeWarning, "ShutdownRetentionExpiring", message)
	}
}

// releaseRetention ends a devbox that sat out its retention window. Depending
// on the operator's configuration the root disk moves to the volumetrash
// recycler, which holds it for a buffer and then deletes the volume, or it is
// deleted along with the devbox.
func (r *DevBoxReconciler) releaseRetention(ctx context.Context, box *devpilotv1alpha1.DevBox, now time.Time) error {
	log := logf.FromContext(ctx)

	// A devbox that reached its release time without going through settleBilling
	// still gets a closed ledger line, so an invoice run never sees a run that
	// never ended.
	closeSegment(box, now, devpilotv1alpha1.EndReasonReleased)

	name := box.Status.RootDiskPVC
	if name == "" {
		name = box.Name + RootDiskPVCSuffix
	}
	if r.Config.RetentionReclaimMode() == ReclaimDelete {
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: box.Namespace}}
		if err := r.Delete(ctx, pvc); err != nil && !isNotFound(err) {
			return fmt.Errorf("delete released root disk %s: %w", name, err)
		}
		log.Info("Deleted root disk of an expired devbox", "pvc", name)
	} else if err := r.moveToTrash(ctx, box, name, now); err != nil {
		return err
	}

	message := "The devbox was not started within its retention window and is released"
	r.eventf(box, corev1.EventTypeWarning, "Released", message)
	log.Info("Released expired devbox", "devbox", box.Name, "rootDiskPVC", name, "reclaim", r.Config.RetentionReclaimMode())
	box.Status.Message = message
	if err := r.Delete(ctx, box); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete expired devbox %s: %w", box.Name, err)
	}
	return nil
}

// moveToTrash hands the root disk to the recycler. The claim loses its owner
// reference in the same update: garbage collection would otherwise delete the
// volume the recycler is now responsible for, which is the difference between a
// recoverable disk and a lost one.
func (r *DevBoxReconciler) moveToTrash(ctx context.Context, box *devpilotv1alpha1.DevBox, name string, now time.Time) error {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: name}, pvc); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("get root disk %s: %w", name, err))
	}
	if pvc.Labels == nil {
		pvc.Labels = map[string]string{}
	}
	pvc.Labels[LabelExpireAt] = now.UTC().Format(expireAtLayout)
	kept := make([]metav1.OwnerReference, 0, len(pvc.OwnerReferences))
	for _, ref := range pvc.OwnerReferences {
		if ref.UID != box.UID {
			kept = append(kept, ref)
		}
	}
	pvc.OwnerReferences = kept
	if err := r.Update(ctx, pvc); err != nil {
		return fmt.Errorf("hand root disk %s to volumetrash: %w", name, err)
	}
	logf.FromContext(ctx).Info("Handed root disk to volumetrash", "pvc", name, "expireAt", pvc.Labels[LabelExpireAt])
	return nil
}

// automationWake is when the controller must look at the devbox again for idle
// shutdown or retention, or zero when nothing is scheduled. Waking at the
// threshold rather than polling keeps a cluster of idle devboxes quiet.
func automationWake(box *devpilotv1alpha1.DevBox, now time.Time) time.Duration {
	var wake time.Duration
	switch box.Status.Phase {
	case devpilotv1alpha1.PhaseRunning:
		if box.Spec.Lifecycle.IdleShutdownMinutes <= 0 {
			return 0
		}
		last, ok := lastActivity(box)
		if !ok {
			return 0
		}
		wake = last.Add(time.Duration(box.Spec.Lifecycle.IdleShutdownMinutes) * time.Minute).Sub(now)
	case devpilotv1alpha1.PhaseStopped, devpilotv1alpha1.PhaseInterrupted:
		releaseAt, ok := releaseDeadline(box)
		if !ok {
			return 0
		}
		remaining := releaseAt.Sub(now)
		switch {
		case remaining > retentionWarn72h:
			wake = remaining - retentionWarn72h
		case remaining > retentionWarn24h:
			wake = remaining - retentionWarn24h
		default:
			wake = remaining
		}
	}
	if wake <= 0 {
		wake = time.Second
	}
	return min(wake, automationRequeue)
}

// releaseDeadline is when a non-running devbox is released, or false when the
// spec disables it.
func releaseDeadline(box *devpilotv1alpha1.DevBox) (time.Time, bool) {
	days := box.Spec.RootDisk.RetentionDaysAfterStop
	if days <= 0 || box.Status.NonRunningSince == nil {
		return time.Time{}, false
	}
	return box.Status.NonRunningSince.Add(time.Duration(days) * 24 * time.Hour), true
}
