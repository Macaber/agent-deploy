package egress

import (
	"context"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestConfigurationDeletionIsNotFirstInstallation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = core.AddToScheme(scheme)
	k := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()
	if _, _, err := Read(ctx, k, "system"); !apierrors.IsNotFound(err) {
		t.Fatalf("first installation: %v", err)
	}
	if err := Save(ctx, k, "system", "", Config{Mode: ModeLegacy}); err != nil {
		t.Fatal(err)
	}
	cm := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: ConfigMapName, Namespace: "system"}}
	if err := k.Delete(ctx, cm); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(ctx, k, "system"); err == nil || apierrors.IsNotFound(err) {
		t.Fatalf("lost config silently became first installation: %v", err)
	}
	if err := k.Get(ctx, client.ObjectKey{Namespace: "system", Name: HistoryConfigMapName}, &core.ConfigMap{}); err != nil {
		t.Fatal(err)
	}
	// Explicit backup restore remains possible; deleting the marker is unnecessary.
	if err := Save(ctx, k, "system", "", Config{Mode: ModeLegacy}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(ctx, k, "system"); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedInitializationFailsClosed(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = core.AddToScheme(scheme)
	k := fake.NewClientBuilder().WithScheme(scheme).Build()
	if err := RememberConfiguration(context.Background(), k, "system"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(context.Background(), k, "system"); err == nil || apierrors.IsNotFound(err) {
		t.Fatalf("interrupted initialization treated as default: %v", err)
	}
}
