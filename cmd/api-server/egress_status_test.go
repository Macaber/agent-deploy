package main

import (
	"context"
	ai "github.com/example/workspace-operator/api/v1alpha1"
	"github.com/example/workspace-operator/internal/egress"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestMigrationObservationIncludesTerminatingOldPods(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = ai.AddToScheme(scheme)
	_ = apps.AddToScheme(scheme)
	_ = core.AddToScheme(scheme)
	now := meta.Now()
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "old", Namespace: "work", Labels: map[string]string{"app": "workspace", egress.MigrationLabel: "unrestricted-v1"}, DeletionTimestamp: &now, Finalizers: []string{"test/hold"}}}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	state, pending, err := observeEgressMigration(context.Background(), k, egress.ModeAllowlist)
	if err != nil || state != "pending" || len(pending) != 1 {
		t.Fatalf("%s %v %v", state, pending, err)
	}
	// Even an old terminating Pod with the target mode is not complete.
	state, _, err = observeEgressMigration(context.Background(), k, egress.ModeUnrestricted)
	if err != nil || state != "pending" {
		t.Fatalf("terminating Pod marked complete: %s %v", state, err)
	}
}
