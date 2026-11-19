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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// maxUsageSegments caps status.segments. A devbox that starts and stops
	// for months would otherwise grow the object past the etcd request limit;
	// the oldest closed segment is dropped first, keeping the most recent
	// usage history.
	maxUsageSegments = 256
)

// settleMetering keeps status.segments in step with the phase: a running devbox
// has exactly one open segment, and a devbox that stops, is interrupted or
// fails closes it. Pending, Starting and Stopping leave the segment alone: a
// pod replaced to apply a new spec is a restart of the same allocation, not the
// end of it, and closing it would split one continuous run.
// It runs on every reconcile, the failing ones included, so an eviction is
// still metered.
func (r *DevBoxReconciler) settleMetering(box *devpilotv1alpha1.DevBox, now time.Time) {
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
	box.Status.TotalUsageSeconds = totalUsageSeconds(box)
}

// totalUsageSeconds sums the closed segments. It is derived from the ledger
// rather than accumulated, so a trimmed or replayed segment can never make the
// total drift.
func totalUsageSeconds(box *devpilotv1alpha1.DevBox) int64 {
	var total int64
	for _, segment := range box.Status.Segments {
		if segment.EndTime == nil || segment.DurationSeconds <= 0 {
			continue
		}
		total += segment.DurationSeconds
	}
	return total
}

// openSegment starts a metering interval unless one is already open. A segment
// left open by a controller that died mid-reconcile is adopted rather than
// duplicated, so a restart cannot double count an interval.
func openSegment(box *devpilotv1alpha1.DevBox, now time.Time) {
	if current := openUsageSegment(box); current != nil {
		return
	}
	segment := devpilotv1alpha1.UsageSegment{
		ID:        nextSegmentID(box),
		NodeName:  box.Status.NodeName,
		StartTime: metav1.NewTime(now.UTC()),
	}
	box.Status.Segments = append(box.Status.Segments, segment)
	trimSegments(box)
}

// closeSegment stamps the open segment with its end time, duration and reason.
// A devbox that never ran has nothing to close, and closing twice is a no-op.
func closeSegment(box *devpilotv1alpha1.DevBox, now time.Time, reason devpilotv1alpha1.SegmentEndReason) {
	segment := openUsageSegment(box)
	if segment == nil {
		return
	}
	end := now.UTC()
	// The clock is reported, not trusted: a clock that stepped backwards must
	// not produce a negative usage duration.
	seconds := end.Sub(segment.StartTime.Time).Seconds()
	if seconds < 0 {
		seconds = 0
	}
	segment.EndTime = new(metav1.NewTime(end))
	segment.DurationSeconds = int64(seconds)
	segment.EndReason = reason
}

// openUsageSegment returns the segment still running, or nil.
func openUsageSegment(box *devpilotv1alpha1.DevBox) *devpilotv1alpha1.UsageSegment {
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
	over := len(box.Status.Segments) - maxUsageSegments
	for ; over > 0; over-- {
		if box.Status.Segments[0].EndTime == nil {
			return
		}
		box.Status.Segments = box.Status.Segments[1:]
	}
}
