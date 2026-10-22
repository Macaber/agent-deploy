/*
Copyright 2026.

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
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

// metricsTestNamespace is the test namespace used across the metrics unit tests.
const metricsTestNamespace = "default"

// Namespaces and Workspace names reused across the tracker tests. They are
// extracted as constants because goconst treats three or more identical
// literals as a finding.
const (
	nsOne   = "ns1"
	nsTwo   = "ns2"
	wsName1 = "ws-1"
	wsName2 = "ws-2"
	wsName3 = "ws-3"
	wsNameA = "ws-a"
	wsNameB = "ws-b"
)

func resetPhaseTracker(t *testing.T) {
	t.Helper()
	// Swap in a fresh tracker instead of only calling resetAll: resetAll
	// deliberately preserves the Running high-water mark, which would leak
	// between tests.
	workspacePhases = newWorkspacePhaseTracker()
	t.Cleanup(func() {
		workspacePhases = newWorkspacePhaseTracker()
	})
}

func gaugeValueForPhase(t *testing.T, phase string) float64 {
	t.Helper()
	return testutil.ToFloat64(workspaceActiveInstances.WithLabelValues(phase))
}

// resetDerivedGauges zeroes the unlabelled total/running/peak gauges. The peak
// is process-wide and monotonic, so tests that assert on it must reset it
// explicitly (production code never does).
func resetDerivedGauges(t *testing.T) {
	t.Helper()
	workspaceTotalInstances.Set(0)
	workspaceRunningInstances.Set(0)
	workspaceRunningPeakInstances.Set(0)
	workspaceRunningPeakTimestamp.Set(0)
	t.Cleanup(func() {
		workspaceTotalInstances.Set(0)
		workspaceRunningInstances.Set(0)
		workspaceRunningPeakInstances.Set(0)
		workspaceRunningPeakTimestamp.Set(0)
	})
}

func assertNotPanics(t *testing.T, msg string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: unexpected panic: %v", msg, r)
		}
	}()
	f()
}

func TestRegisterWorkspaceMetricsIsIdempotent(t *testing.T) {
	assertNotPanics(t, "registerWorkspaceMetrics must be idempotent", func() {
		registerWorkspaceMetrics()
		registerWorkspaceMetrics()
	})

	// Every phase series must exist from the start so dashboards render zeros
	// instead of gaps before the first reconcile.
	for _, phase := range allWorkspaceMetricPhases {
		if got := gaugeValueForPhase(t, string(phase)); got < 0 {
			t.Errorf("phase %q should have a registered series, got %v", phase, got)
		}
	}
	if got := gaugeValueForPhase(t, phaseLabelUnknown); got != 0 {
		t.Errorf("Unknown phase series should be 0 after registration, got %v", got)
	}
}

func TestWorkspacePhaseTrackerSetUnsetAndUnknown(t *testing.T) {
	resetPhaseTracker(t)

	keyA := types.NamespacedName{Namespace: metricsTestNamespace, Name: wsNameA}
	keyB := types.NamespacedName{Namespace: metricsTestNamespace, Name: wsNameB}
	keyEmpty := types.NamespacedName{Namespace: metricsTestNamespace, Name: "ws-empty"}

	workspacePhases.set(keyA, aiv1alpha1.WorkspaceRunning)
	workspacePhases.set(keyB, aiv1alpha1.WorkspaceRunning)
	// An empty phase (never initialized status) is counted under "Unknown".
	workspacePhases.set(keyEmpty, "")

	if got := gaugeValueForPhase(t, string(aiv1alpha1.WorkspaceRunning)); got != 2 {
		t.Errorf("expected 2 Running instances, got %v", got)
	}
	if got := gaugeValueForPhase(t, phaseLabelUnknown); got != 1 {
		t.Errorf("expected 1 Unknown instance, got %v", got)
	}
	if got := gaugeValueForPhase(t, string(aiv1alpha1.WorkspaceSleeping)); got != 0 {
		t.Errorf("expected 0 Sleeping instances, got %v", got)
	}

	// Reconciling a phase transition moves the instance between buckets.
	workspacePhases.set(keyA, aiv1alpha1.WorkspaceSleeping)
	if got := gaugeValueForPhase(t, string(aiv1alpha1.WorkspaceRunning)); got != 1 {
		t.Errorf("expected 1 Running instance after transition, got %v", got)
	}
	if got := gaugeValueForPhase(t, string(aiv1alpha1.WorkspaceSleeping)); got != 1 {
		t.Errorf("expected 1 Sleeping instance after transition, got %v", got)
	}

	// Deleting a Workspace removes it from the gauge.
	workspacePhases.unset(keyB)
	if got := gaugeValueForPhase(t, string(aiv1alpha1.WorkspaceRunning)); got != 0 {
		t.Errorf("expected 0 Running instances after delete, got %v", got)
	}

	// Unsetting an unknown key is a no-op.
	assertNotPanics(t, "unset of an untracked Workspace must not panic", func() {
		workspacePhases.unset(types.NamespacedName{Namespace: metricsTestNamespace, Name: "ws-never-seen"})
	})
	if got := workspacePhases.len(); got != 2 {
		t.Errorf("expected 2 tracked workspaces, got %d", got)
	}
}

func TestWorkspacePhaseTrackerResetAll(t *testing.T) {
	resetPhaseTracker(t)

	workspacePhases.resetAll(map[types.NamespacedName]aiv1alpha1.WorkspacePhase{
		{Namespace: nsOne, Name: wsName1}: aiv1alpha1.WorkspaceStarting,
		{Namespace: nsOne, Name: wsName2}: aiv1alpha1.WorkspaceStarting,
		{Namespace: nsTwo, Name: wsName3}: aiv1alpha1.WorkspaceStopped,
	})

	if got := workspacePhases.len(); got != 3 {
		t.Errorf("expected 3 tracked workspaces, got %d", got)
	}
	if got := gaugeValueForPhase(t, string(aiv1alpha1.WorkspaceStarting)); got != 2 {
		t.Errorf("expected 2 Starting instances, got %v", got)
	}
	if got := gaugeValueForPhase(t, string(aiv1alpha1.WorkspaceStopped)); got != 1 {
		t.Errorf("expected 1 Stopped instance, got %v", got)
	}

	// The periodic sync runnable replaces the whole snapshot, dropping stale keys.
	workspacePhases.resetAll(map[types.NamespacedName]aiv1alpha1.WorkspacePhase{
		{Namespace: nsTwo, Name: wsName3}: aiv1alpha1.WorkspaceStopped,
	})
	if got := workspacePhases.len(); got != 1 {
		t.Errorf("expected 1 tracked workspace after resync, got %d", got)
	}
	if got := gaugeValueForPhase(t, string(aiv1alpha1.WorkspaceStarting)); got != 0 {
		t.Errorf("expected 0 Starting instances after resync, got %v", got)
	}
	if got := gaugeValueForPhase(t, string(aiv1alpha1.WorkspaceStopped)); got != 1 {
		t.Errorf("expected 1 Stopped instance after resync, got %v", got)
	}
}

// TestWorkspaceTotalsAndRunningGauges covers the unlabelled convenience series:
// total (all Phases), running (Phase=Running now) and the Running high-water
// mark. They must always agree with the labelled gauge they derive from.
func TestWorkspaceTotalsAndRunningGauges(t *testing.T) {
	resetPhaseTracker(t)
	resetDerivedGauges(t)

	if got := testutil.ToFloat64(workspaceTotalInstances); got != 0 {
		t.Errorf("expected 0 total instances on a fresh tracker, got %v", got)
	}
	if got := testutil.ToFloat64(workspaceRunningInstances); got != 0 {
		t.Errorf("expected 0 running instances on a fresh tracker, got %v", got)
	}

	keyA := types.NamespacedName{Namespace: metricsTestNamespace, Name: wsNameA}
	keyB := types.NamespacedName{Namespace: metricsTestNamespace, Name: wsNameB}
	keyC := types.NamespacedName{Namespace: metricsTestNamespace, Name: "ws-c"}

	workspacePhases.set(keyA, aiv1alpha1.WorkspacePending)
	workspacePhases.set(keyB, aiv1alpha1.WorkspaceRunning)
	if got := testutil.ToFloat64(workspaceTotalInstances); got != 2 {
		t.Errorf("expected total 2 after two Workspaces, got %v", got)
	}
	if got := testutil.ToFloat64(workspaceRunningInstances); got != 1 {
		t.Errorf("expected running 1, got %v", got)
	}

	// A phase transition must not change the total, only the running count.
	workspacePhases.set(keyA, aiv1alpha1.WorkspaceRunning)
	if got := testutil.ToFloat64(workspaceTotalInstances); got != 2 {
		t.Errorf("expected total to stay 2 across a phase transition, got %v", got)
	}
	if got := testutil.ToFloat64(workspaceRunningInstances); got != 2 {
		t.Errorf("expected running 2 after transition, got %v", got)
	}
	if got := testutil.ToFloat64(workspaceRunningPeakInstances); got != 2 {
		t.Errorf("expected peak to rise to 2, got %v", got)
	}

	// Peak is a high-water mark: leaving Running does not lower it.
	workspacePhases.set(keyA, aiv1alpha1.WorkspaceSleeping)
	workspacePhases.unset(keyB)
	if got := testutil.ToFloat64(workspaceRunningInstances); got != 0 {
		t.Errorf("expected running 0 after sleep+delete, got %v", got)
	}
	if got := testutil.ToFloat64(workspaceRunningPeakInstances); got != 2 {
		t.Errorf("expected peak to stay 2, got %v", got)
	}
	if got := testutil.ToFloat64(workspaceTotalInstances); got != 1 {
		t.Errorf("expected total 1 after one delete, got %v", got)
	}

	// Reaching a lower Running level again must not move the peak or its stamp.
	peakStamp := testutil.ToFloat64(workspaceRunningPeakTimestamp)
	workspacePhases.set(keyC, aiv1alpha1.WorkspaceRunning)
	if got := testutil.ToFloat64(workspaceRunningPeakInstances); got != 2 {
		t.Errorf("expected peak to stay 2 at a lower level, got %v", got)
	}
	if got := testutil.ToFloat64(workspaceRunningPeakTimestamp); got != peakStamp {
		t.Errorf("expected peak timestamp to stay %v, got %v", peakStamp, got)
	}

	// The two unlabelled gauges must always agree with the labelled series.
	if got, want := testutil.ToFloat64(workspaceRunningInstances), gaugeValueForPhase(t, string(aiv1alpha1.WorkspaceRunning)); got != want {
		t.Errorf("workspace_running_instances (%v) must equal phase=Running (%v)", got, want)
	}
}

// TestWorkspaceRunningPeakSentinelZeroKeepsTimestampZero guards the "no peak
// yet" state: a zero-running fleet must not stamp a peak of 0.
func TestWorkspaceRunningPeakSentinelZeroKeepsTimestampZero(t *testing.T) {
	resetPhaseTracker(t)
	resetDerivedGauges(t)

	workspacePhases.set(types.NamespacedName{Namespace: metricsTestNamespace, Name: "ws-idle"}, aiv1alpha1.WorkspaceSleeping)
	if got := testutil.ToFloat64(workspaceRunningPeakInstances); got != 0 {
		t.Errorf("expected peak 0 with no Running Workspace, got %v", got)
	}
	if got := testutil.ToFloat64(workspaceRunningPeakTimestamp); got != 0 {
		t.Errorf("expected peak timestamp 0 with no Running Workspace, got %v", got)
	}
}

// TestWorkspaceRunningPeakSurvivesResync pins the high-water mark across the
// periodic cluster resync: a lower snapshot must not reset the peak.
func TestWorkspaceRunningPeakSurvivesResync(t *testing.T) {
	resetPhaseTracker(t)
	resetDerivedGauges(t)

	workspacePhases.resetAll(map[types.NamespacedName]aiv1alpha1.WorkspacePhase{
		{Namespace: nsOne, Name: wsName1}: aiv1alpha1.WorkspaceRunning,
		{Namespace: nsOne, Name: wsName2}: aiv1alpha1.WorkspaceRunning,
		{Namespace: nsOne, Name: wsName3}: aiv1alpha1.WorkspaceSleeping,
	})
	if got := testutil.ToFloat64(workspaceRunningPeakInstances); got != 2 {
		t.Fatalf("expected peak 2 after resync, got %v", got)
	}
	peakStamp := testutil.ToFloat64(workspaceRunningPeakTimestamp)
	if peakStamp <= 0 {
		t.Fatalf("expected a positive peak timestamp, got %v", peakStamp)
	}

	// A later resync with fewer Running Workspaces keeps the peak and its stamp.
	workspacePhases.resetAll(map[types.NamespacedName]aiv1alpha1.WorkspacePhase{
		{Namespace: nsOne, Name: wsName1}: aiv1alpha1.WorkspaceSleeping,
	})
	if got := testutil.ToFloat64(workspaceRunningInstances); got != 0 {
		t.Errorf("expected running 0 after resync, got %v", got)
	}
	if got := testutil.ToFloat64(workspaceRunningPeakInstances); got != 2 {
		t.Errorf("expected peak to survive resync as 2, got %v", got)
	}
	if got := testutil.ToFloat64(workspaceRunningPeakTimestamp); got != peakStamp {
		t.Errorf("expected peak timestamp to survive resync as %v, got %v", peakStamp, got)
	}
	if got := testutil.ToFloat64(workspaceTotalInstances); got != 1 {
		t.Errorf("expected total 1 after resync, got %v", got)
	}
}

// TestWorkspaceRunningPeakTimestampMovesOnNewPeak verifies the peak timestamp is
// only rewritten when the high-water mark actually advances.
func TestWorkspaceRunningPeakTimestampMovesOnNewPeak(t *testing.T) {
	resetDerivedGauges(t)

	phases := make(map[types.NamespacedName]aiv1alpha1.WorkspacePhase)
	tr := &workspacePhaseTracker{phases: phases, now: time.Now}
	tr.set(types.NamespacedName{Namespace: metricsTestNamespace, Name: "ws-1"}, aiv1alpha1.WorkspaceRunning)
	first := testutil.ToFloat64(workspaceRunningPeakTimestamp)
	if first <= 0 {
		t.Fatalf("expected a positive peak timestamp, got %v", first)
	}

	// Advance the fake clock and break the record: the stamp must follow.
	tr.now = func() time.Time { return time.Unix(int64(first)+60, 0) }
	tr.set(types.NamespacedName{Namespace: metricsTestNamespace, Name: "ws-2"}, aiv1alpha1.WorkspaceRunning)
	if got := testutil.ToFloat64(workspaceRunningPeakInstances); got != 2 {
		t.Errorf("expected new peak 2, got %v", got)
	}
	if got, want := testutil.ToFloat64(workspaceRunningPeakTimestamp), first+60; got != want {
		t.Errorf("expected peak timestamp %v after a new record, got %v", want, got)
	}

	// A zero-value tracker (no clock injected) must not panic on the fallback.
	zero := &workspacePhaseTracker{phases: make(map[types.NamespacedName]aiv1alpha1.WorkspacePhase)}
	assertNotPanics(t, "a tracker without an injected clock must fall back to time.Now", func() {
		zero.set(types.NamespacedName{Namespace: metricsTestNamespace, Name: "ws-zero"}, aiv1alpha1.WorkspaceRunning)
	})
}

// BenchmarkWorkspacePhaseTrackerSet guards the O(1) property of the per-reconcile// path: with 5000 tracked Workspaces, a full recompute would walk the whole map on
// every call, while the incremental implementation only touches two gauge series.
func BenchmarkWorkspacePhaseTrackerSet(b *testing.B) {
	phases := make(map[types.NamespacedName]aiv1alpha1.WorkspacePhase, 5000)
	for i := range 5000 {
		phases[types.NamespacedName{Namespace: metricsTestNamespace, Name: "ws-" + strconv.Itoa(i)}] = aiv1alpha1.WorkspaceRunning
	}
	t := newWorkspacePhaseTracker()
	t.resetAll(phases)
	key := types.NamespacedName{Namespace: metricsTestNamespace, Name: "ws-0"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		phase := aiv1alpha1.WorkspaceRunning
		if i%2 == 1 {
			phase = aiv1alpha1.WorkspaceSleeping
		}
		t.set(key, phase)
	}
}
