package controller

import (
	"context"
	"testing"
	"time"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestBackfillMissingStartedAt(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = aiv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	podStart := metav1.NewTime(time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC))
	lastActive := metav1.NewTime(time.Date(2026, 11, 4, 18, 0, 0, 0, time.UTC))
	alreadyStarted := metav1.NewTime(time.Date(2026, 11, 5, 7, 0, 0, 0, time.UTC))

	// ws1: Running, StartedAt == nil, has running pod
	ws1 := &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-running", Namespace: "default"},
		Spec:       aiv1alpha1.WorkspaceSpec{Owner: "user1"},
		Status: aiv1alpha1.WorkspaceStatus{
			Phase: aiv1alpha1.WorkspaceRunning,
		},
	}
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-running-pod",
			Namespace: "default",
			Labels:    map[string]string{"workspace": "ws-running"},
		},
		Status: corev1.PodStatus{
			Phase:     corev1.PodRunning,
			StartTime: &podStart,
		},
	}

	// ws2: Sleeping, StartedAt == nil, has LastActiveTime
	ws2 := &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-sleeping", Namespace: "default"},
		Spec:       aiv1alpha1.WorkspaceSpec{Owner: "user2"},
		Status: aiv1alpha1.WorkspaceStatus{
			Phase:          aiv1alpha1.WorkspaceSleeping,
			LastActiveTime: &lastActive,
		},
	}

	// ws3: Running, already has StartedAt
	ws3 := &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-with-started", Namespace: "default"},
		Spec:       aiv1alpha1.WorkspaceSpec{Owner: "user3"},
		Status: aiv1alpha1.WorkspaceStatus{
			Phase:     aiv1alpha1.WorkspaceRunning,
			StartedAt: &alreadyStarted,
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ws1, pod1, ws2, ws3).
		WithStatusSubresource(&aiv1alpha1.Workspace{}).
		Build()

	r := &WorkspaceReconciler{
		Client: k8sClient,
		Scheme: scheme,
	}

	ctx := context.Background()
	wsList := &aiv1alpha1.WorkspaceList{
		Items: []aiv1alpha1.Workspace{*ws1, *ws2, *ws3},
	}

	r.backfillMissingStartedAt(ctx, wsList)

	// Verify ws1 (running) was backfilled with pod's StartTime
	got1 := &aiv1alpha1.Workspace{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ws-running"}, got1); err != nil {
		t.Fatalf("failed to fetch ws1: %v", err)
	}
	if got1.Status.StartedAt == nil || !got1.Status.StartedAt.Equal(&podStart) {
		t.Errorf("expected ws1 StartedAt %v, got %v", podStart, got1.Status.StartedAt)
	}

	// Verify ws2 (sleeping) was backfilled with LastActiveTime
	got2 := &aiv1alpha1.Workspace{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ws-sleeping"}, got2); err != nil {
		t.Fatalf("failed to fetch ws2: %v", err)
	}
	if got2.Status.StartedAt == nil || !got2.Status.StartedAt.Equal(&lastActive) {
		t.Errorf("expected ws2 StartedAt %v, got %v", lastActive, got2.Status.StartedAt)
	}

	// Verify ws3 kept its original StartedAt
	got3 := &aiv1alpha1.Workspace{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ws-with-started"}, got3); err != nil {
		t.Fatalf("failed to fetch ws3: %v", err)
	}
	if got3.Status.StartedAt == nil || !got3.Status.StartedAt.Equal(&alreadyStarted) {
		t.Errorf("expected ws3 StartedAt %v, got %v", alreadyStarted, got3.Status.StartedAt)
	}
}
