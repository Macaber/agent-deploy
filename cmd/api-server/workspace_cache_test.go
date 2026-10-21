package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

// envtestAssetsDir returns the envtest binary directory, preferring the
// KUBEBUILDER_ASSETS env var (set by `make test`) and falling back to the
// directory the Makefile installs into. Returns "" when neither is usable, so
// the calling test can skip instead of failing on machines without the
// control-plane binaries.
func envtestAssetsDir() string {
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}
	candidate := filepath.Join("..", "..", "bin", "k8s", "1.36.0-darwin-arm64")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return ""
}

// TestStartWorkspaceCacheReturns is the regression test for the startup hang:
// cache.Cache.Start blocks until its context is cancelled, so calling it
// synchronously inside startWorkspaceCache made the whole API server hang
// forever before it ever registered a route or bound the port.
func TestStartWorkspaceCacheReturns(t *testing.T) {
	assets := envtestAssetsDir()
	if assets == "" {
		t.Skip("envtest binaries not available; run `make test` or set KUBEBUILDER_ASSETS")
	}

	env := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("failed to start envtest control plane: %v", err)
	}
	defer func() {
		if err := env.Stop(); err != nil {
			t.Logf("failed to stop envtest control plane: %v", err)
		}
	}()

	scheme := runtime.NewScheme()
	if err := aiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to build scheme: %v", err)
	}

	// A pre-existing Workspace proves the returned cache is already populated
	// by the initial LIST, not just that the call returned.
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	existing := &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "pre-existing", Namespace: "default"},
		Spec: aiv1alpha1.WorkspaceSpec{
			Owner:   "alice",
			Runtime: aiv1alpha1.RuntimeSpec{Image: "nginx:alpine"},
			Storage: aiv1alpha1.StorageSpec{Size: "1Gi"},
		},
	}
	if err := k8sClient.Create(context.Background(), existing); err != nil {
		t.Fatalf("failed to create fixture Workspace: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		cache *workspaceCache
		err   error
	}
	done := make(chan result, 1)
	go func() {
		c, err := startWorkspaceCache(ctx, cfg, scheme)
		done <- result{cache: c, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("startWorkspaceCache returned an error: %v", got.err)
		}
		if got.cache.size() != 1 {
			t.Errorf("expected 1 materialized workspace, got %d", got.cache.size())
		}
		if !got.cache.isReady() {
			t.Error("expected the cache to report ready after a successful sync")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("startWorkspaceCache did not return within 15s; cache.Start is blocking the startup path")
	}
}
