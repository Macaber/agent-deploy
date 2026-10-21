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

	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

// metricsTestNamespace is the test namespace used across the metrics unit tests.
const metricsTestNamespace = "default"

func resetPhaseTracker(t *testing.T) {
	t.Helper()
	workspacePhases.resetAll(map[types.NamespacedName]aiv1alpha1.WorkspacePhase{})
	t.Cleanup(func() {
		workspacePhases.resetAll(map[types.NamespacedName]aiv1alpha1.WorkspacePhase{})
	})
}

func gaugeValueForPhase(t *testing.T, phase string) float64 {
	t.Helper()
	return testutil.ToFloat64(workspaceActiveInstances.WithLabelValues(phase))
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

	keyA := types.NamespacedName{Namespace: metricsTestNamespace, Name: "ws-a"}
	keyB := types.NamespacedName{Namespace: metricsTestNamespace, Name: "ws-b"}
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
		{Namespace: "ns1", Name: "ws-1"}: aiv1alpha1.WorkspaceStarting,
		{Namespace: "ns1", Name: "ws-2"}: aiv1alpha1.WorkspaceStarting,
		{Namespace: "ns2", Name: "ws-3"}: aiv1alpha1.WorkspaceStopped,
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
		{Namespace: "ns2", Name: "ws-3"}: aiv1alpha1.WorkspaceStopped,
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

// BenchmarkWorkspacePhaseTrackerSet guards the O(1) property of the per-reconcile
// path: with 5000 tracked Workspaces, a full recompute would walk the whole map on
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
