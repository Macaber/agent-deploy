package controller

import (
	"testing"
	"time"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestComputeStartedAt verifies the reconcile-time decision for status.startedAt:
// it initializes on a transition into Running (preferring the Pod start time) and
// preserves the existing value while the workspace stays Running.
func TestComputeStartedAt(t *testing.T) {
	podStart := metav1.NewTime(time.Date(2026, 11, 5, 8, 30, 0, 0, time.UTC))
	existing := metav1.NewTime(time.Date(2026, 11, 5, 7, 0, 0, 0, time.UTC))

	t.Run("Sleeping to Running uses pod start time", func(t *testing.T) {
		got := computeStartedAt(aiv1alpha1.WorkspaceSleeping, &existing, &podStart)
		if got == nil || !got.Equal(&podStart) {
			t.Errorf("expected pod start time %v, got %v", podStart, got)
		}
	})

	t.Run("Stopped to Running uses pod start time", func(t *testing.T) {
		got := computeStartedAt(aiv1alpha1.WorkspaceStopped, nil, &podStart)
		if got == nil || !got.Equal(&podStart) {
			t.Errorf("expected pod start time %v, got %v", podStart, got)
		}
	})

	t.Run("Running stays Running preserves existing value", func(t *testing.T) {
		got := computeStartedAt(aiv1alpha1.WorkspaceRunning, &existing, &podStart)
		if got == nil || !got.Equal(&existing) {
			t.Errorf("expected existing start time %v preserved, got %v", existing, got)
		}
	})

	t.Run("Running with nil StartedAt is initialized", func(t *testing.T) {
		got := computeStartedAt(aiv1alpha1.WorkspaceRunning, nil, &podStart)
		if got == nil || !got.Equal(&podStart) {
			t.Errorf("expected pod start time %v, got %v", podStart, got)
		}
	})

	t.Run("falls back to now when pod start time missing", func(t *testing.T) {
		before := time.Now().Add(-2 * time.Second)
		got := computeStartedAt(aiv1alpha1.WorkspaceSleeping, nil, nil)
		if got == nil {
			t.Fatal("expected non-nil start time fallback")
		}
		if got.Time.Before(before) {
			t.Errorf("expected fallback close to now, got %v", got.Time)
		}
	})
}

// TestMergeStartedAt verifies the monotonic persistence of status.startedAt so
// out-of-order concurrent reconcile writes never move it backwards.
func TestMergeStartedAt(t *testing.T) {
	older := metav1.NewTime(time.Date(2026, 11, 5, 7, 0, 0, 0, time.UTC))
	newer := metav1.NewTime(time.Date(2026, 11, 5, 8, 30, 0, 0, time.UTC))

	t.Run("nil desired keeps latest without change", func(t *testing.T) {
		got, changed := mergeStartedAt(&older, nil)
		if changed {
			t.Error("expected no change when desired is nil")
		}
		if got == nil || !got.Equal(&older) {
			t.Errorf("expected latest %v preserved, got %v", older, got)
		}
	})

	t.Run("nil latest accepts desired", func(t *testing.T) {
		got, changed := mergeStartedAt(nil, &newer)
		if !changed {
			t.Error("expected change when latest is nil")
		}
		if got == nil || !got.Equal(&newer) {
			t.Errorf("expected desired %v, got %v", newer, got)
		}
	})

	t.Run("newer desired advances value", func(t *testing.T) {
		got, changed := mergeStartedAt(&older, &newer)
		if !changed {
			t.Error("expected change when desired is newer")
		}
		if got == nil || !got.Equal(&newer) {
			t.Errorf("expected newer %v, got %v", newer, got)
		}
	})

	t.Run("older desired does not move backwards", func(t *testing.T) {
		got, changed := mergeStartedAt(&newer, &older)
		if changed {
			t.Error("expected no change when desired is older")
		}
		if got == nil || !got.Equal(&newer) {
			t.Errorf("expected latest %v preserved, got %v", newer, got)
		}
	})

	t.Run("equal desired is no-op", func(t *testing.T) {
		same := metav1.NewTime(newer.Time)
		_, changed := mergeStartedAt(&newer, &same)
		if changed {
			t.Error("expected no change when desired equals latest")
		}
	})
}
