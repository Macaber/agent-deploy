package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

// stubProbeTransport makes probeWorkspaceViaIngress answer with status without
// touching the network.
func stubProbeTransport(t *testing.T, status int) {
	t.Helper()
	orig := probeHTTPClient.Transport
	t.Cleanup(func() { probeHTTPClient.Transport = orig })
	probeHTTPClient.Transport = roundTripFunc(func(*http.Request) *http.Response {
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     make(http.Header),
		}
	})
}

func runningWorkspace(namespace, name, owner, endpoint, healthPath string) *aiv1alpha1.Workspace {
	return &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: aiv1alpha1.WorkspaceSpec{
			Owner:   owner,
			Runtime: aiv1alpha1.RuntimeSpec{HealthPath: healthPath},
		},
		Status: aiv1alpha1.WorkspaceStatus{
			Phase:    aiv1alpha1.WorkspaceRunning,
			Endpoint: endpoint,
		},
	}
}

func newPollTestClient(t *testing.T, objs ...*aiv1alpha1.Workspace) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := aiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to build scheme: %v", err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&aiv1alpha1.Workspace{})
	for _, o := range objs {
		builder = builder.WithObjects(o)
	}
	return builder
}

// The poll must be served from the informer view: the fake client holds no
// Workspaces at all, so any direct read would return NotFound and produce 500.
func TestPollWorkspaceRunning_UsesInformerView(t *testing.T) {
	t.Setenv("WORKSPACE_BASE_URL", "http://mock-probe/bocomwork/ws")
	stubProbeTransport(t, http.StatusOK)

	ws := runningWorkspace("bocomwork", "ws-u1", "u1", "ws-u1.example.com", "/health")
	wc := newWorkspaceCache()
	wc.OnAdd(ws, false)

	k8sClient := newPollTestClient(t).Build()

	rr := httptest.NewRecorder()
	pollWorkspaceRunning(context.Background(), k8sClient, wc, rr, "u1", "ws-u1", "bocomwork", "Workspace creation timed out")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 served from the cache, got %d: %s", rr.Code, rr.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if payload["phase"] != string(aiv1alpha1.WorkspaceRunning) {
		t.Errorf("expected phase Running, got %q", payload["phase"])
	}
	if payload["url"] != "http://mock-probe/bocomwork/ws/u1/" {
		t.Errorf("unexpected url %q", payload["url"])
	}
}

// A workspace created moments ago may not have reached the informer yet; the
// poll must fall back to a direct read instead of failing with 500.
func TestPollWorkspaceRunning_FallsBackOnCacheMiss(t *testing.T) {
	t.Setenv("WORKSPACE_BASE_URL", "http://mock-probe/bocomwork/ws")
	stubProbeTransport(t, http.StatusOK)

	ws := runningWorkspace("bocomwork", "ws-u1", "u1", "ws-u1.example.com", "/health")
	k8sClient := newPollTestClient(t, ws).Build()
	wc := newWorkspaceCache() // empty view

	rr := httptest.NewRecorder()
	pollWorkspaceRunning(context.Background(), k8sClient, wc, rr, "u1", "ws-u1", "bocomwork", "Workspace creation timed out")

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 via the direct-read fallback, got %d: %s", rr.Code, rr.Body.String())
	}
}

// Unchanged behaviour: an unresolvable workspace still surfaces as a 500.
func TestPollWorkspaceRunning_UnknownWorkspaceStillReturns500(t *testing.T) {
	t.Setenv("WORKSPACE_BASE_URL", "http://mock-probe/bocomwork/ws")
	stubProbeTransport(t, http.StatusOK)

	k8sClient := newPollTestClient(t).Build()
	wc := newWorkspaceCache()

	rr := httptest.NewRecorder()
	pollWorkspaceRunning(context.Background(), k8sClient, wc, rr, "u1", "ws-missing", "bocomwork", "Workspace creation timed out")

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for a workspace that cannot be resolved, got %d: %s", rr.Code, rr.Body.String())
	}
}
