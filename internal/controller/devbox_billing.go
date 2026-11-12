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
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// ConditionPreempted records that the platform took the devbox back. It is
	// separate from Ready so a console can show the cause without parsing the
	// message, and it stays True until the devbox runs again.
	ConditionPreempted = "Preempted"

	// maxBillingSegments caps status.segments. A devbox that starts and stops
	// for months would otherwise grow the object past the etcd request limit;
	// the oldest closed segment is dropped first, keeping the most recent
	// history a console or an invoice run needs.
	maxBillingSegments = 256

	// microsPerUnit is the fixed point scale of a cost: amounts carry six
	// decimals, which is the precision the price pattern allows.
	microsPerUnit int64 = 1_000_000
	// secondsPerHour is the billing granularity.
	secondsPerHour int64 = 3600
)

// outOfServiceTaint is what a cluster administrator or a cloud controller puts
// on a node before evicting everything from it, which is how a Spot reclaim
// announces itself before the pods are actually deleted.
const outOfServiceTaint = "node.kubernetes.io/out-of-service"

// eventf records an Event when the manager gave the reconciler a recorder. The
// envtest suite builds reconcilers by hand, so the recorder can be nil.
func (r *DevBoxReconciler) eventf(obj runtime.Object, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(obj, nil, eventType, reason, reason, message)
}

// settleBilling keeps status.segments in step with the phase: a running devbox
// has exactly one open segment, and a devbox that stops, is interrupted or
// fails closes it. Pending, Starting and Stopping leave the segment alone: a
// pod replaced to apply a new spec is a restart of the same allocation, not the
// end of it, and billing it as two runs would double count a start-up.
// It runs on every reconcile, the failing ones included, so an eviction is
// still invoiced.
func (r *DevBoxReconciler) settleBilling(box *devpilotv1alpha1.DevBox, now time.Time) {
	switch box.Status.Phase {
	case devpilotv1alpha1.PhaseRunning:
		openSegment(box, now)
	case devpilotv1alpha1.PhaseStopped:
		closeSegment(box, now, devpilotv1alpha1.EndReasonUserStopped)
	case devpilotv1alpha1.PhaseInterrupted:
		closeSegment(box, now, devpilotv1alpha1.EndReasonPreempted)
	case devpilotv1alpha1.PhaseFailed:
		closeSegment(box, now, devpilotv1alpha1.EndReasonNodeFailure)
	case devpilotv1alpha1.PhasePending, devpilotv1alpha1.PhaseStarting, devpilotv1alpha1.PhaseStopping:
	}
	box.Status.TotalCost = totalCost(box)
}

// totalCost sums the closed segments. It is derived from the ledger rather than
// accumulated, so a trimmed or replayed segment can never make the total drift.
func totalCost(box *devpilotv1alpha1.DevBox) string {
	var micros int64
	for _, segment := range box.Status.Segments {
		amount, ok := parsePriceMicros(segment.Cost)
		if !ok {
			continue
		}
		micros += amount
	}
	return formatAmount(micros)
}

// openSegment starts a billing interval unless one is already open. A segment
// left open by a controller that died mid-reconcile is adopted rather than
// duplicated, so a restart cannot double count a minute.
func openSegment(box *devpilotv1alpha1.DevBox, now time.Time) {
	if current := openBillingSegment(box); current != nil {
		return
	}
	segment := devpilotv1alpha1.BillingSegment{
		ID:           nextSegmentID(box),
		NodeName:     box.Status.NodeName,
		PricePerHour: box.Spec.PricePerHour,
		StartTime:    metav1.NewTime(now.UTC()),
	}
	box.Status.Segments = append(box.Status.Segments, segment)
	trimSegments(box)
}

// closeSegment stamps the open segment with its duration and cost. A devbox
// that never ran has nothing to close, and closing twice is a no-op.
func closeSegment(box *devpilotv1alpha1.DevBox, now time.Time, reason devpilotv1alpha1.SegmentEndReason) {
	segment := openBillingSegment(box)
	if segment == nil {
		return
	}
	end := now.UTC()
	// The clock is reported, not trusted: a node that stepped backwards must
	// not produce a negative invoice line.
	seconds := end.Sub(segment.StartTime.Time).Seconds()
	if seconds < 0 {
		seconds = 0
	}
	billed := int64(seconds)
	segment.EndTime = new(metav1.NewTime(end))
	segment.BilledSeconds = billed
	segment.EndReason = reason
	segment.Cost = segmentCost(segment.PricePerHour, billed)
}

// openBillingSegment returns the segment still running, or nil.
func openBillingSegment(box *devpilotv1alpha1.DevBox) *devpilotv1alpha1.BillingSegment {
	for i := len(box.Status.Segments) - 1; i >= 0; i-- {
		if box.Status.Segments[i].EndTime == nil {
			return &box.Status.Segments[i]
		}
	}
	return nil
}

// nextSegmentID continues the sequence, which is what makes an out-of-order
// status patch detectable downstream.
func nextSegmentID(box *devpilotv1alpha1.DevBox) int64 {
	var highest int64
	for _, segment := range box.Status.Segments {
		if segment.ID > highest {
			highest = segment.ID
		}
	}
	return highest + 1
}

// trimSegments drops the oldest closed segments once the ledger is over its
// cap. An open segment is never dropped: it is the one the next close needs.
func trimSegments(box *devpilotv1alpha1.DevBox) {
	over := len(box.Status.Segments) - maxBillingSegments
	for ; over > 0; over-- {
		if box.Status.Segments[0].EndTime == nil {
			return
		}
		box.Status.Segments = box.Status.Segments[1:]
	}
}

// segmentCost multiplies a fixed point hourly price by a duration and renders
// the result with six decimals, trailing zeros trimmed to at least two. A
// price that does not parse bills zero rather than failing the reconcile: the
// schema already rejects a malformed one, and a ledger line is better than a
// devbox that cannot start.
func segmentCost(pricePerHour string, seconds int64) string {
	micros, ok := parsePriceMicros(pricePerHour)
	if !ok {
		return ""
	}
	return formatAmount(micros * seconds / secondsPerHour)
}

// parsePriceMicros reads "2.80" into 2800000. The value must match the
// schema's pattern; anything else reports false.
func parsePriceMicros(price string) (int64, bool) {
	price = strings.TrimSpace(price)
	if price == "" {
		return 0, false
	}
	whole, fraction, _ := strings.Cut(price, ".")
	if whole == "" || len(fraction) > 6 {
		return 0, false
	}
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || units < 0 {
		return 0, false
	}
	// The pattern allows a missing fraction only; the fractional part is padded
	// to the fixed point scale so "2.8" and "2.80" bill identically.
	padding := fraction + strings.Repeat("0", 6-len(fraction))
	micros, err := strconv.ParseInt(padding, 10, 64)
	if err != nil || micros < 0 {
		return 0, false
	}
	return units*microsPerUnit + micros, true
}

// formatAmount renders micros as a decimal with six places, trimmed to a
// minimum of two.
func formatAmount(micros int64) string {
	if micros <= 0 {
		return "0.00"
	}
	whole := micros / microsPerUnit
	fraction := fmt.Sprintf("%06d", micros%microsPerUnit)
	fraction = strings.TrimRight(fraction, "0")
	for len(fraction) < 2 {
		fraction += "0"
	}
	return strconv.FormatInt(whole, 10) + "." + fraction
}

// preempted reports whether the pod itself says the platform took its node
// back. A node drained for maintenance shows up as an out-of-service condition
// on the pod, and a reclaim usually shows up as an eviction.
func preempted(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	if pod.Status.Reason == "Evicted" || pod.Status.Reason == "Preempted" {
		return true
	}
	return pod.Status.Phase == corev1.PodFailed &&
		strings.Contains(strings.ToLower(pod.Status.Message), "out of service")
}

// nodeOutOfService reports whether an administrator or a cloud controller has
// tainted the node for eviction, which is how a reclaim announces itself before
// the pods are actually deleted.
func nodeOutOfService(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == outOfServiceTaint && taint.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

// reclaim reports whether the platform took the node back from this spot
// devbox, and the message to record when it did. Only a devbox that believes
// it is on a node is a candidate: once it is interrupted its node is cleared,
// which is what keeps a later start from being interrupted by the node it
// already left. An on-demand devbox is never interrupted; it keeps the price it
// was sold at, so a lost pod is restarted or fails instead.
func (r *DevBoxReconciler) reclaim(ctx context.Context, box *devpilotv1alpha1.DevBox, pod *corev1.Pod) (string, bool) {
	if box.Spec.BillingMode != devpilotv1alpha1.BillingModeSpot {
		return "", false
	}
	switch box.Status.Phase {
	case devpilotv1alpha1.PhaseRunning, devpilotv1alpha1.PhaseStarting:
	default:
		return "", false
	}
	if preempted(pod) {
		return "The spot node was reclaimed; the root disk is retained", true
	}
	// The pod can vanish before the controller sees why, so the node in status
	// is the remaining evidence. A missing node reads as nothing to do: the
	// controller was not granted enough access to tell, and guessing would
	// interrupt devboxes on every run.
	if box.Status.NodeName == "" {
		return "", false
	}
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: box.Status.NodeName}, node); err != nil {
		return "", false
	}
	if nodeOutOfService(node) {
		return "Node " + box.Status.NodeName + " was taken out of service; the root disk is retained", true
	}
	return "", false
}

// markInterrupted moves the devbox to Interrupted and records why. The root
// disk is untouched: an interrupted devbox is one the platform took, not one
// the user released, so it restarts on the same overlay.
func (r *DevBoxReconciler) markInterrupted(box *devpilotv1alpha1.DevBox, reason, message string) {
	now := nowUTC()
	box.Status.Phase = devpilotv1alpha1.PhaseInterrupted
	box.Status.StartingStep = devpilotv1alpha1.StartingStepNone
	box.Status.InterruptedAt = &now
	if box.Status.NonRunningSince == nil {
		box.Status.NonRunningSince = &now
	}
	// The devbox occupies no node while interrupted; where the last segment ran
	// is already in the ledger.
	box.Status.NodeName = ""
	box.Status.PodName = ""
	box.Status.Message = message
	r.setCondition(box, ConditionReady, metav1ConditionFalse, reason, message)
	r.setCondition(box, ConditionPreempted, metav1ConditionTrue, reason, message)
	r.eventf(box, corev1.EventTypeWarning, "Interrupted", message)
}

// resumeInterrupted lets the start and restart actions take a devbox out of the
// interrupted resting state. The phase becomes Pending and the next reconcile
// schedules a pod on whatever node the scheduler finds; if the spot pool has no
// capacity the devbox stays in Starting and reports why, rather than silently
// falling back to a node that was taken away.
func (r *DevBoxReconciler) resumeInterrupted(box *devpilotv1alpha1.DevBox) {
	if box.Status.Phase != devpilotv1alpha1.PhaseInterrupted {
		return
	}
	box.Status.Phase = devpilotv1alpha1.PhasePending
	box.Status.Message = "Restarting after an interruption"
	r.clearCondition(box, ConditionPreempted)
}
