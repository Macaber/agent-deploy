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
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

// phaseLabelUnknown is used for Workspaces whose Phase has not been set yet.
const phaseLabelUnknown = "Unknown"

// defaultActiveInstancesSyncInterval is how often the workspace_active_instances
// gauge is rebuilt from a cluster-wide Workspace list when the reconciler runs
// as a manager Runnable. Per-reconcile updates keep it fresh in between.
const defaultActiveInstancesSyncInterval = time.Minute

var (
	// workspaceReconcileDuration observes the wall time of each full Workspace
	// reconciliation, including all child resources (PVC, Deployment, Service,
	// Ingress, NetworkPolicy) and the status update.
	workspaceReconcileDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "workspace_reconcile_duration_seconds",
		Help: "Wall time in seconds of a full Workspace reconciliation (all child resources plus status update).",
		Buckets: []float64{
			0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
		},
	})

	// workspaceActiveInstances tracks how many Workspaces the controller manages
	// in each status Phase so dashboards can show the fleet water level.
	workspaceActiveInstances = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "workspace_active_instances",
		Help: "Number of Workspace instances managed by the controller, grouped by status Phase.",
	}, []string{"phase"})

	// The gauges below are unlabelled convenience series derived from the same
	// tracker as workspace_active_instances. They exist so "how many workspaces
	// are there in total / currently Running / at peak, and when" can be read
	// (and alerted on) without aggregation or `phase` selector typos. They are
	// updated from the exact same code path as the labelled gauge, so they can
	// never disagree with it.

	// workspaceTotalInstances is the number of Workspaces the controller tracks
	// right now, across every Phase. It always equals
	// sum(workspace_active_instances).
	workspaceTotalInstances = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "workspace_total_instances",
		Help: "Number of Workspace instances currently managed by the controller (all Phases).",
	})

	// workspaceRunningInstances is the number of Workspaces in Phase Running
	// right now. It always equals workspace_active_instances{phase="Running"}.
	workspaceRunningInstances = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "workspace_running_instances",
		Help: "Number of Workspace instances currently in status Phase Running.",
	})

	// workspaceRunningPeakInstances is the high-water mark of concurrent
	// Running Workspaces observed by THIS manager process. It is monotonic until
	// the process restarts (leader failover included), which is what makes it
	// more accurate than max_over_time(): every phase change is observed, not
	// just the values sampled at scrape time. For a peak limited to a query
	// window use max_over_time(workspace_running_instances[$__range]) instead.
	workspaceRunningPeakInstances = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "workspace_running_peak_instances",
		Help: "High-water mark of concurrently Running Workspaces since this manager process started.",
	})

	// workspaceRunningPeakTimestamp records when the current peak was first
	// reached, so "when did we peak?" does not need a second data source.
	// Prometheus cannot recover the time of a max from max_over_time().
	workspaceRunningPeakTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "workspace_running_peak_timestamp_seconds",
		Help: "Unix timestamp at which workspace_running_peak_instances was first reached.",
	})
)

// allWorkspaceMetricPhases lists every WorkspacePhase that always has a time
// series, even at zero, so Grafana stacked panels never leave gaps. Failed is
// deliberately absent: nothing in this controller assigns it (reconcile errors
// are surfaced by controller_runtime_reconcile_errors_total instead), so a
// series here would render a permanently-zero line.
var allWorkspaceMetricPhases = []aiv1alpha1.WorkspacePhase{
	aiv1alpha1.WorkspacePending,
	aiv1alpha1.WorkspaceStarting,
	aiv1alpha1.WorkspaceRunning,
	aiv1alpha1.WorkspaceSleeping,
	aiv1alpha1.WorkspaceStopped,
}

var registerWorkspaceMetricsOnce sync.Once

// registerWorkspaceMetrics registers the Workspace observability metrics with the
// controller-runtime metrics registry (exposed on the manager metrics endpoint).
// Safe to call multiple times; registration happens exactly once per process.
// Reconcile errors are intentionally not duplicated here: controller-runtime
// already exports controller_runtime_reconcile_errors_total{controller="workspace"}.
func registerWorkspaceMetrics() {
	registerWorkspaceMetricsOnce.Do(func() {
		metrics.Registry.MustRegister(
			workspaceReconcileDuration,
			workspaceActiveInstances,
			workspaceTotalInstances,
			workspaceRunningInstances,
			workspaceRunningPeakInstances,
			workspaceRunningPeakTimestamp,
		)
		// Publish zero-valued series for every phase so dashboards render
		// before the first reconcile produces data.
		zeroWorkspacePhaseSeries()
	})
}

// workspacePhases is the process-wide tracker backing workspace_active_instances.
// Reconciles update it on completion; the periodic sync runnable rebuilds it from
// the cluster to correct drift (missed deletes, error paths, HA failover).
var workspacePhases = newWorkspacePhaseTracker()

// workspacePhaseTracker maintains the per-Workspace phase map and keeps the
// workspace_active_instances gauge in sync incrementally (O(1) per reconcile).
// It also maintains the derived unlabelled gauges (total / running / running
// peak) from the same map so they can never drift from the labelled series.
type workspacePhaseTracker struct {
	mu     sync.Mutex
	phases map[types.NamespacedName]aiv1alpha1.WorkspacePhase
	// running is the number of entries currently in Phase Running. Kept as a
	// counter so per-reconcile updates stay O(1) instead of recounting the map.
	running int
	// peakRunning is the highest `running` value seen by this process, and
	// peakAt is when it was first reached (zero until the first peak).
	peakRunning int
	peakAt      time.Time
	// now is the clock used to stamp peakAt; nil means time.Now (set by the
	// constructor, overridable in tests).
	now func() time.Time
}

func newWorkspacePhaseTracker() *workspacePhaseTracker {
	return &workspacePhaseTracker{
		phases: make(map[types.NamespacedName]aiv1alpha1.WorkspacePhase),
		now:    time.Now,
	}
}

// clock returns the tracker clock, tolerating a zero-value tracker in tests.
func (t *workspacePhaseTracker) clock() time.Time {
	if t.now == nil {
		return time.Now()
	}
	return t.now()
}

// set records the phase for one Workspace and refreshes only the affected gauge
// series. This runs at the tail of every reconcile, so it must stay O(1): a full
// recompute here would turn a fleet-wide sweep into O(N^2).
func (t *workspacePhaseTracker) set(key types.NamespacedName, phase aiv1alpha1.WorkspacePhase) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if previous, ok := t.phases[key]; ok {
		if previous == phase {
			return
		}
		workspaceActiveInstances.WithLabelValues(phaseMetricLabel(previous)).Dec()
		if previous == aiv1alpha1.WorkspaceRunning {
			t.running--
		}
	} else {
		workspaceTotalInstances.Inc()
	}
	t.phases[key] = phase
	workspaceActiveInstances.WithLabelValues(phaseMetricLabel(phase)).Inc()
	if phase == aiv1alpha1.WorkspaceRunning {
		t.running++
	}
	t.publishDerivedLocked()
}

// unset removes one Workspace and decrements only its phase series.
func (t *workspacePhaseTracker) unset(key types.NamespacedName) {
	t.mu.Lock()
	defer t.mu.Unlock()
	previous, ok := t.phases[key]
	if !ok {
		return
	}
	delete(t.phases, key)
	workspaceTotalInstances.Dec()
	workspaceActiveInstances.WithLabelValues(phaseMetricLabel(previous)).Dec()
	if previous == aiv1alpha1.WorkspaceRunning {
		t.running--
	}
	t.publishDerivedLocked()
}

// len returns the number of tracked Workspaces (used by tests).
func (t *workspacePhaseTracker) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.phases)
}

// resetAll replaces the tracked Workspaces with the given snapshot and rebuilds
// every series from it. This is the low-frequency path (periodic resync, tests);
// per-reconcile changes go through set/unset and stay O(1).
func (t *workspacePhaseTracker) resetAll(phases map[types.NamespacedName]aiv1alpha1.WorkspacePhase) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phases = phases
	counts := make(map[string]int, len(phases))
	for _, phase := range phases {
		counts[phaseMetricLabel(phase)]++
	}
	for _, phase := range allWorkspaceMetricPhases {
		workspaceActiveInstances.WithLabelValues(string(phase)).Set(float64(counts[string(phase)]))
	}
	workspaceActiveInstances.WithLabelValues(phaseLabelUnknown).Set(float64(counts[phaseLabelUnknown]))

	workspaceTotalInstances.Set(float64(len(phases)))
	t.running = counts[string(aiv1alpha1.WorkspaceRunning)]
	t.publishDerivedLocked()
}

// publishDerivedLocked publishes the unlabelled total/running gauges and folds
// the current Running count into the high-water mark. Callers must hold t.mu.
func (t *workspacePhaseTracker) publishDerivedLocked() {
	workspaceRunningInstances.Set(float64(t.running))
	// The peak is deliberately NOT reset by resetAll(): a resync (or an HA
	// failover replay) must not erase the water mark already observed.
	if t.running > t.peakRunning {
		t.peakRunning = t.running
		t.peakAt = t.clock()
		workspaceRunningPeakInstances.Set(float64(t.peakRunning))
		workspaceRunningPeakTimestamp.Set(float64(t.peakAt.Unix()))
	}
}

// zeroWorkspacePhaseSeries publishes zero-valued series for every phase so
// dashboards render zeros instead of gaps before the first reconcile.
func zeroWorkspacePhaseSeries() {
	for _, phase := range allWorkspaceMetricPhases {
		workspaceActiveInstances.WithLabelValues(string(phase)).Set(0)
	}
	workspaceActiveInstances.WithLabelValues(phaseLabelUnknown).Set(0)
	workspaceTotalInstances.Set(0)
	workspaceRunningInstances.Set(0)
	workspaceRunningPeakInstances.Set(0)
	workspaceRunningPeakTimestamp.Set(0)
}

// phaseMetricLabel maps a WorkspacePhase to its gauge label value.
func phaseMetricLabel(phase aiv1alpha1.WorkspacePhase) string {
	if phase == "" {
		return phaseLabelUnknown
	}
	return string(phase)
}
