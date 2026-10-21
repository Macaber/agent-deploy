package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	gomemstats "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

func TestIsSliceDifferent(t *testing.T) {
	t.Run("both empty or nil", func(t *testing.T) {
		var s1 []aiv1alpha1.ConfigMapVolumeMount
		s2 := []aiv1alpha1.ConfigMapVolumeMount{}
		if isSliceDifferent(s1, s2) {
			t.Errorf("expected false for nil vs empty slice")
		}
	})

	t.Run("non-empty to empty", func(t *testing.T) {
		s1 := []aiv1alpha1.ConfigMapVolumeMount{{ConfigMapName: "cm-1", MountPath: "/etc/config"}}
		s2 := []aiv1alpha1.ConfigMapVolumeMount{}
		if !isSliceDifferent(s1, s2) {
			t.Errorf("expected true when clearing slice")
		}
	})

	t.Run("empty to non-empty", func(t *testing.T) {
		s1 := []aiv1alpha1.ConfigMapVolumeMount{}
		s2 := []aiv1alpha1.ConfigMapVolumeMount{{ConfigMapName: "cm-1", MountPath: "/etc/config"}}
		if !isSliceDifferent(s1, s2) {
			t.Errorf("expected true when adding to slice")
		}
	})

	t.Run("same items", func(t *testing.T) {
		s1 := []aiv1alpha1.ConfigMapVolumeMount{{ConfigMapName: "cm-1", MountPath: "/etc/config"}}
		s2 := []aiv1alpha1.ConfigMapVolumeMount{{ConfigMapName: "cm-1", MountPath: "/etc/config"}}
		if isSliceDifferent(s1, s2) {
			t.Errorf("expected false for identical slices")
		}
	})
}

func TestUpdateExistingWorkspace_ClearsConfigMapVolumeMounts(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = aiv1alpha1.AddToScheme(scheme)

	existingWs := &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-user1",
			Namespace: "default",
		},
		Spec: aiv1alpha1.WorkspaceSpec{
			Owner: "user1",
			Runtime: aiv1alpha1.RuntimeSpec{
				Image: "nginx:latest",
				Port:  8080,
			},
			ConfigMapVolumeMounts: []aiv1alpha1.ConfigMapVolumeMount{
				{ConfigMapName: "my-cm", MountPath: "/etc/config"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&aiv1alpha1.Workspace{}).WithObjects(existingWs).Build()

	req := &workspaceRequest{
		UserID:                "user1",
		Image:                 "nginx:latest",
		ConfigMapVolumeMounts: []aiv1alpha1.ConfigMapVolumeMount{}, // cleared in UI
	}

	err := updateExistingWorkspace(context.Background(), fakeClient, existingWs, req, "ws-user1", "default")
	if err != nil {
		t.Fatalf("updateExistingWorkspace failed: %v", err)
	}

	updated := &aiv1alpha1.Workspace{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "ws-user1"}, updated)
	if err != nil {
		t.Fatalf("failed to fetch updated workspace: %v", err)
	}

	if len(updated.Spec.ConfigMapVolumeMounts) != 0 {
		t.Errorf("expected ConfigMapVolumeMounts to be empty, got: %v", updated.Spec.ConfigMapVolumeMounts)
	}
}

func TestCreateWorkspace_PreservesUserID(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = aiv1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&aiv1alpha1.Workspace{}).Build()

	req := &workspaceRequest{
		UserID:    "user_123",
		Namespace: "bocomwork",
		Image:     "bocomwork:v1",
		Env: []aiv1alpha1.EnvVar{
			{Name: "USER_CODE", Value: "user_code_999"},
		},
	}

	wsName := sanitizeK8sName("ws-" + req.UserID)
	if wsName != "ws-user-123" {
		t.Fatalf("expected wsName 'ws-user-123', got '%s'", wsName)
	}

	err := createNewWorkspace(context.Background(), fakeClient, req, wsName, req.Namespace, req.UserID)
	if err != nil {
		t.Fatalf("createNewWorkspace failed: %v", err)
	}

	created := &aiv1alpha1.Workspace{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "bocomwork", Name: "ws-user-123"}, created)
	if err != nil {
		t.Fatalf("failed to fetch created workspace: %v", err)
	}

	if created.Spec.Owner != "user_123" {
		t.Errorf("expected Spec.Owner to be 'user_123', got '%s'", created.Spec.Owner)
	}
}

func TestCreateWorkspace_OALabel(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = aiv1alpha1.AddToScheme(scheme)

	t.Run("bocomwork namespace sets oa label from Env", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&aiv1alpha1.Workspace{}).Build()
		req := &workspaceRequest{
			UserID:    "user_123",
			Namespace: "bocomwork",
			Image:     "bocomwork:v1",
			Env: []aiv1alpha1.EnvVar{
				{Name: "OA", Value: "zhangsan"},
			},
		}

		wsName := sanitizeK8sName("ws-" + req.UserID)
		err := createNewWorkspace(context.Background(), fakeClient, req, wsName, req.Namespace, req.UserID)
		if err != nil {
			t.Fatalf("createNewWorkspace failed: %v", err)
		}

		created := &aiv1alpha1.Workspace{}
		err = fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "bocomwork", Name: wsName}, created)
		if err != nil {
			t.Fatalf("failed to fetch workspace: %v", err)
		}

		if created.Labels == nil || created.Labels["oa"] != "zhangsan" {
			t.Errorf("expected label oa='zhangsan', got: %v", created.Labels)
		}
	})

	t.Run("non-bocomwork namespace does not set oa label", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&aiv1alpha1.Workspace{}).Build()
		req := &workspaceRequest{
			UserID:    "user_123",
			Namespace: "default",
			Image:     "nginx:latest",
			Env: []aiv1alpha1.EnvVar{
				{Name: "OA", Value: "zhangsan"},
			},
		}

		wsName := sanitizeK8sName("ws-" + req.UserID)
		err := createNewWorkspace(context.Background(), fakeClient, req, wsName, req.Namespace, req.UserID)
		if err != nil {
			t.Fatalf("createNewWorkspace failed: %v", err)
		}

		created := &aiv1alpha1.Workspace{}
		err = fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: wsName}, created)
		if err != nil {
			t.Fatalf("failed to fetch workspace: %v", err)
		}

		if created.Labels != nil && created.Labels["oa"] != "" {
			t.Errorf("expected no oa label in non-bocomwork namespace, got: %v", created.Labels)
		}
	})

	t.Run("updateExistingWorkspace in bocomwork updates oa label", func(t *testing.T) {
		existingWs := &aiv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ws-user-oa",
				Namespace: "bocomwork",
				Labels: map[string]string{
					"oa": "old_oa",
				},
			},
			Spec: aiv1alpha1.WorkspaceSpec{
				Owner: "user_oa",
				Runtime: aiv1alpha1.RuntimeSpec{
					Image: "bocomwork:v1",
				},
			},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&aiv1alpha1.Workspace{}).WithObjects(existingWs).Build()
		req := &workspaceRequest{
			UserID:    "user_oa",
			Namespace: "bocomwork",
			Image:     "bocomwork:v2",
			Env: []aiv1alpha1.EnvVar{
				{Name: "OA", Value: "new_oa"},
			},
		}

		err := updateExistingWorkspace(context.Background(), fakeClient, existingWs, req, "ws-user-oa", "bocomwork")
		if err != nil {
			t.Fatalf("updateExistingWorkspace failed: %v", err)
		}

		updated := &aiv1alpha1.Workspace{}
		err = fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "bocomwork", Name: "ws-user-oa"}, updated)
		if err != nil {
			t.Fatalf("failed to fetch updated workspace: %v", err)
		}

		if updated.Labels == nil || updated.Labels["oa"] != "new_oa" {
			t.Errorf("expected label oa='new_oa', got: %v", updated.Labels)
		}
	})
}

func TestGetOAFromWorkspace(t *testing.T) {
	t.Run("returns OA from label first", func(t *testing.T) {
		ws := &aiv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"oa": "label_oa"},
			},
			Spec: aiv1alpha1.WorkspaceSpec{
				Runtime: aiv1alpha1.RuntimeSpec{
					Env: []aiv1alpha1.EnvVar{{Name: "OA", Value: "env_oa"}},
				},
			},
		}
		if got := getOAFromWorkspace(ws); got != "label_oa" {
			t.Errorf("expected 'label_oa', got '%s'", got)
		}
	})

	t.Run("returns OA from env when label is absent", func(t *testing.T) {
		ws := &aiv1alpha1.Workspace{
			Spec: aiv1alpha1.WorkspaceSpec{
				Runtime: aiv1alpha1.RuntimeSpec{
					Env: []aiv1alpha1.EnvVar{{Name: "oa", Value: "env_oa"}},
				},
			},
		}
		if got := getOAFromWorkspace(ws); got != "env_oa" {
			t.Errorf("expected 'env_oa', got '%s'", got)
		}
	})
}

// newTestWorkspaceCache seeds a workspaceCache via informer events and forces
// an immediate snapshot rebuild. The debounce timer is parked (1h) so tests are
// deterministic; call rebuildNow to publish changes.
func newTestWorkspaceCache(tb testing.TB, workspaces ...*aiv1alpha1.Workspace) *workspaceCache {
	tb.Helper()
	wc := newWorkspaceCache()
	wc.rebuildDelay = time.Hour
	for _, ws := range workspaces {
		wc.OnAdd(ws, false)
	}
	wc.rebuildNow()
	return wc
}

// newListFixture builds a handler over a two-workspace view: ws-u1
// (bocomwork, owner u1, oa label "oa-1", Running) and ws-u2 (default,
// owner u2, Stopped).
func newListFixture(tb testing.TB) (wc *workspaceCache, handler http.HandlerFunc) {
	tb.Helper()
	ws1 := &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-u1",
			Namespace: "bocomwork",
			Labels:    map[string]string{"oa": "oa-1"},
		},
		Spec: aiv1alpha1.WorkspaceSpec{
			Owner: "u1",
		},
		Status: aiv1alpha1.WorkspaceStatus{
			Phase: aiv1alpha1.WorkspaceRunning,
		},
	}
	ws2 := &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-u2",
			Namespace: "default",
		},
		Spec: aiv1alpha1.WorkspaceSpec{
			Owner: "u2",
		},
		Status: aiv1alpha1.WorkspaceStatus{
			Phase: aiv1alpha1.WorkspaceStopped,
		},
	}

	wc = newTestWorkspaceCache(tb, ws1, ws2)
	return wc, listWorkspacesHandler(wc)
}

// decodeItems unmarshals a legacy bare-array list response.
func decodeItems(tb testing.TB, rr *httptest.ResponseRecorder) []workspaceItem {
	tb.Helper()
	var items []workspaceItem
	if err := json.Unmarshal(rr.Body.Bytes(), &items); err != nil {
		tb.Fatalf("failed to unmarshal JSON: %v", err)
	}
	return items
}

// decodeEnvelope unmarshals a paginated list response.
func decodeEnvelope(tb testing.TB, rr *httptest.ResponseRecorder) workspaceListResponse {
	tb.Helper()
	var resp workspaceListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		tb.Fatalf("failed to unmarshal JSON: %v", err)
	}
	return resp
}

func TestListWorkspacesHandler(t *testing.T) {
	_, handler := newListFixture(t)

	t.Run("filter by namespace bocomwork (legacy bare array)", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/workspaces?namespace=bocomwork", nil)
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rr.Code)
		}
		items := decodeItems(t, rr)
		if len(items) != 1 {
			t.Fatalf("expected 1 item, got %d", len(items))
		}
		if items[0].Phase != "Running" {
			t.Errorf("expected phase Running, got %s", items[0].Phase)
		}
		if items[0].Namespace != "bocomwork" {
			t.Errorf("expected namespace bocomwork, got %s", items[0].Namespace)
		}
	})

	t.Run("query all namespaces", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/workspaces?namespace=all", nil)
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rr.Code)
		}
		items := decodeItems(t, rr)
		if len(items) != 2 {
			t.Fatalf("expected 2 items, got %d", len(items))
		}
	})

}

func TestListWorkspacesPagination(t *testing.T) {
	_, handler := newListFixture(t)

	t.Run("paginated envelope with page and pageSize", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/workspaces?page=2&pageSize=1", nil)
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rr.Code)
		}
		resp := decodeEnvelope(t, rr)
		if resp.Total != 2 {
			t.Errorf("expected total 2, got %d", resp.Total)
		}
		if resp.Page != 2 || resp.PageSize != 1 {
			t.Errorf("expected page=2 pageSize=1, got page=%d pageSize=%d", resp.Page, resp.PageSize)
		}
		if resp.TotalPages != 2 {
			t.Errorf("expected totalPages 2, got %d", resp.TotalPages)
		}
		if len(resp.Items) != 1 {
			t.Fatalf("expected 1 item on page 2, got %d", len(resp.Items))
		}
		// Sorted by namespace then name: "bocomwork/ws-u1" before "default/ws-u2"
		if resp.Items[0].Name != "ws-u2" {
			t.Errorf("expected second page item ws-u2, got %s", resp.Items[0].Name)
		}
	})

	t.Run("default pageSize applied when only page is set", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/workspaces?page=1", nil)
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rr.Code)
		}
		resp := decodeEnvelope(t, rr)
		if resp.PageSize != defaultListPageSize {
			t.Errorf("expected default pageSize %d, got %d", defaultListPageSize, resp.PageSize)
		}
		if len(resp.Items) != 2 {
			t.Errorf("expected all items (2) on single page, got %d", len(resp.Items))
		}
	})

	t.Run("page beyond range returns empty items with total", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/workspaces?page=9&pageSize=10", nil)
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rr.Code)
		}
		resp := decodeEnvelope(t, rr)
		if resp.Total != 2 || len(resp.Items) != 0 {
			t.Errorf("expected total=2 with 0 items, got total=%d items=%d", resp.Total, len(resp.Items))
		}
	})

}

func TestListWorkspacesFilters(t *testing.T) {
	_, handler := newListFixture(t)

	t.Run("filters match expected workspaces", func(t *testing.T) {
		cases := []struct {
			name    string
			query   string
			wantLen int
			match   func(workspaceItem) bool
		}{
			{"filter by owner", "owner=u2", 1, func(it workspaceItem) bool { return it.UserID == "u2" }},
			{"filter by oa label combined with namespace", "namespace=bocomwork&oa=oa-1", 1, func(it workspaceItem) bool { return it.OA == "oa-1" }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequest("GET", "/api/workspaces?"+tc.query, nil)
				rr := httptest.NewRecorder()
				handler(rr, req)

				if rr.Code != http.StatusOK {
					t.Fatalf("expected status 200, got %d", rr.Code)
				}
				items := decodeItems(t, rr)
				if len(items) != tc.wantLen {
					t.Fatalf("query %q: expected %d items, got %d", tc.query, tc.wantLen, len(items))
				}
				if !tc.match(items[0]) {
					t.Errorf("query %q: unexpected first item %+v", tc.query, items[0])
				}
			})
		}
	})

	t.Run("unknown namespace returns empty array", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/workspaces?namespace=ghost", nil)
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rr.Code)
		}
		items := decodeItems(t, rr)
		if len(items) != 0 {
			t.Errorf("expected 0 items, got %d", len(items))
		}
	})

}

func TestListWorkspacesInvalidParams(t *testing.T) {
	_, handler := newListFixture(t)

	t.Run("invalid page param returns 400", func(t *testing.T) {
		for _, raw := range []string{"0", "-1", "abc"} {
			req := httptest.NewRequest("GET", "/api/workspaces?page="+raw, nil)
			rr := httptest.NewRecorder()
			handler(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("page=%q: expected 400, got %d", raw, rr.Code)
			}
		}
		req := httptest.NewRequest("GET", "/api/workspaces?pageSize=abc", nil)
		rr := httptest.NewRecorder()
		handler(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("pageSize=abc: expected 400, got %d", rr.Code)
		}
	})

	// Regression: (page-1)*pageSize used to overflow to a negative int, after
	// which offsets[start:end] panicked and reset the connection. One
	// unauthenticated GET was enough to trigger it.
	t.Run("huge page param returns 400 without panicking", func(t *testing.T) {
		for _, raw := range []string{
			"9223372036854775807", // MaxInt64
			"9223372036854775808", // MaxInt64+1, Atoi fails
			"9999999999999999999", // far beyond int64, Atoi fails
			"2147483648",          // over the bound, but no overflow
			"1099511627776",       // 1 << 40
		} {
			req := httptest.NewRequest("GET", "/api/workspaces?page="+raw, nil)
			rr := httptest.NewRecorder()
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						t.Fatalf("page=%q: handler panicked: %v", raw, rec)
					}
				}()
				handler(rr, req)
			}()
			if rr.Code != http.StatusBadRequest {
				t.Errorf("page=%q: expected 400, got %d", raw, rr.Code)
			}
		}
	})

	t.Run("page at the accepted bound does not panic", func(t *testing.T) {
		req := httptest.NewRequest("GET",
			fmt.Sprintf("/api/workspaces?page=%d&pageSize=%d", maxListPage, fallbackMaxListPageSize), nil)
		rr := httptest.NewRecorder()
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("page=%d: handler panicked: %v", maxListPage, rec)
				}
			}()
			handler(rr, req)
		}()
		if rr.Code != http.StatusOK {
			t.Fatalf("page=%d: expected 200, got %d", maxListPage, rr.Code)
		}
		resp := decodeEnvelope(t, rr)
		if len(resp.Items) != 0 {
			t.Errorf("page=%d: expected an empty page, got %d items", maxListPage, len(resp.Items))
		}
	})

	t.Run("pageSize is capped by listMaxPageSize", func(t *testing.T) {
		orig := listMaxPageSize
		listMaxPageSize = 2
		defer func() { listMaxPageSize = orig }()

		req := httptest.NewRequest("GET", "/api/workspaces?page=1&pageSize=999", nil)
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rr.Code)
		}
		resp := decodeEnvelope(t, rr)
		if resp.PageSize != 2 {
			t.Errorf("expected pageSize capped to 2, got %d", resp.PageSize)
		}
	})

}

func TestListWorkspacesEventUpdates(t *testing.T) {
	wc, handler := newListFixture(t)

	t.Run("event updates are reflected after rebuild", func(t *testing.T) {
		ws3 := &aiv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: "ws-u3", Namespace: "bocomwork"},
			Spec:       aiv1alpha1.WorkspaceSpec{Owner: "u3"},
			Status:     aiv1alpha1.WorkspaceStatus{Phase: aiv1alpha1.WorkspaceStarting},
		}
		wc.OnAdd(ws3, false)
		wc.rebuildNow()

		req := httptest.NewRequest("GET", "/api/workspaces?namespace=bocomwork", nil)
		rr := httptest.NewRecorder()
		handler(rr, req)

		items := decodeItems(t, rr)
		if len(items) != 2 {
			t.Fatalf("expected 2 items after add, got %d", len(items))
		}

		wc.OnDelete(ws3)
		wc.rebuildNow()
		req = httptest.NewRequest("GET", "/api/workspaces?namespace=bocomwork", nil)
		rr = httptest.NewRecorder()
		handler(rr, req)
		if err := json.Unmarshal(rr.Body.Bytes(), &items); err != nil {
			t.Fatalf("failed to unmarshal JSON: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("expected 1 item after delete, got %d", len(items))
		}
	})
}

type roundTripFunc func(req *http.Request) *http.Response

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

func TestProbeWorkspaceViaIngress_CanaryCookie(t *testing.T) {
	origTransport := probeHTTPClient.Transport
	defer func() { probeHTTPClient.Transport = origTransport }()

	t.Run("attaches X-Canary to Cookie when CANARY is set in config", func(t *testing.T) {
		t.Setenv("CANARY", "new")
		t.Setenv("WORKSPACE_BASE_URL", "http://mock-probe/bocomwork/ws")

		var receivedCookie string
		var receivedUA string

		probeHTTPClient.Transport = roundTripFunc(func(req *http.Request) *http.Response {
			receivedCookie = req.Header.Get("Cookie")
			receivedUA = req.Header.Get("User-Agent")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Header:     make(http.Header),
			}
		})

		ws := &aiv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Namespace: "bocomwork"},
			Spec: aiv1alpha1.WorkspaceSpec{
				Runtime: aiv1alpha1.RuntimeSpec{HealthPath: "/health"},
			},
		}

		ctx := context.Background()
		ok := probeWorkspaceViaIngress(ctx, "u1", "ws-u1", ws)
		if !ok {
			t.Fatalf("expected probe to succeed")
		}
		if receivedCookie != "oa=u1; WorkspaceUser=u1; X-Canary=new" {
			t.Errorf("expected Cookie 'oa=u1; WorkspaceUser=u1; X-Canary=new', got %q", receivedCookie)
		}
		if receivedUA != "Workspace-Ingress-Probe/1.0" {
			t.Errorf("expected User-Agent header, got %q", receivedUA)
		}
	})

	t.Run("does not attach X-Canary to Cookie when CANARY is not set", func(t *testing.T) {
		t.Setenv("CANARY", "")
		t.Setenv("X_CANARY", "")
		t.Setenv("CANARY_HEADER", "")
		t.Setenv("WORKSPACE_CANARY_HEADER", "")
		t.Setenv("WORKSPACE_BASE_URL", "http://mock-probe/bocomwork/ws")

		var receivedCookie string
		probeHTTPClient.Transport = roundTripFunc(func(req *http.Request) *http.Response {
			receivedCookie = req.Header.Get("Cookie")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Header:     make(http.Header),
			}
		})

		ws := &aiv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Namespace: "bocomwork"},
			Spec: aiv1alpha1.WorkspaceSpec{
				Runtime: aiv1alpha1.RuntimeSpec{HealthPath: "/health"},
			},
		}

		ctx := context.Background()
		ok := probeWorkspaceViaIngress(ctx, "u1", "ws-u1", ws)
		if !ok {
			t.Fatalf("expected probe to succeed")
		}
		if receivedCookie != "oa=u1; WorkspaceUser=u1" {
			t.Errorf("expected Cookie 'oa=u1; WorkspaceUser=u1', got %q", receivedCookie)
		}
	})
}

func TestHealthEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	t.Run("GET /health returns 200 ok", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/health", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rr.Code)
		}
		if rr.Body.String() != "ok" {
			t.Errorf("expected body 'ok', got %q", rr.Body.String())
		}
	})

	t.Run("GET /healthz returns 200 ok", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/healthz", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("expected status 200, got %d", rr.Code)
		}
		if rr.Body.String() != "ok" {
			t.Errorf("expected body 'ok', got %q", rr.Body.String())
		}
	})
}

// newLargeScaleCache builds a workspaceCache with n workspaces spread over
// namespaces ns-0..ns-49 with owners user-0..n-1, exercising realistic 5,000
// instance scale (Task 4.2 acceptance: 1,000+ QPS, stable memory).
func newLargeScaleCache(tb testing.TB, n int) *workspaceCache {
	tb.Helper()
	wsList := make([]*aiv1alpha1.Workspace, 0, n)
	for i := range n {
		wsList = append(wsList, &aiv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("ws-user-%05d", i),
				Namespace: fmt.Sprintf("ns-%d", i%50),
			},
			Spec: aiv1alpha1.WorkspaceSpec{
				Owner: fmt.Sprintf("user-%05d", i),
				Runtime: aiv1alpha1.RuntimeSpec{
					Image:  fmt.Sprintf("registry.example.com/devbox:v%d", i%7),
					CPU:    "2",
					Memory: "4Gi",
				},
			},
			Status: aiv1alpha1.WorkspaceStatus{
				Phase: aiv1alpha1.WorkspacePhase([]string{
					"Pending", "Starting", "Running", "Sleeping", "Stopped",
				}[i%5]),
			},
		})
	}
	return newTestWorkspaceCache(tb, wsList...)
}

// TestListWorkspacesLargeScaleThroughput verifies the acceptance criterion of
// Task 4.2: GET /api/workspaces sustains 1,000+ QPS at 5,000 instance scale.
func TestListWorkspacesLargeScaleThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large-scale throughput test in -short mode")
	}

	wc := newLargeScaleCache(t, 5000)
	if wc.size() != 5000 {
		t.Fatalf("expected 5000 workspaces in cache, got %d", wc.size())
	}

	srv := httptest.NewServer(listWorkspacesHandler(wc))
	defer srv.Close()

	const (
		workers    = 32
		duration   = 2 * time.Second
		minQPS     = 1000
		targetPath = "/api/workspaces?page=3&pageSize=50&namespace=ns-7"
	)

	// Tune connection pooling once, before spawning workers — the client and
	// its transport are shared, so mutating them inside a worker would race.
	transport := srv.Client().Transport.(*http.Transport)
	transport.MaxIdleConnsPerHost = workers

	var total atomic.Int64
	done := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			client := srv.Client()
			for {
				select {
				case <-done:
					return
				default:
				}
				resp, err := client.Get(srv.URL + targetPath)
				if err != nil {
					t.Errorf("request failed: %v", err)
					return
				}
				if resp.StatusCode != http.StatusOK {
					t.Errorf("unexpected status %d", resp.StatusCode)
					_ = resp.Body.Close()
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				total.Add(1)
			}
		})
	}

	time.Sleep(duration)
	close(done)
	wg.Wait()

	qps := float64(total.Load()) / duration.Seconds()
	t.Logf("GET %s: %d requests in %v → %.0f QPS (min required: %d)", targetPath, total.Load(), duration, qps, minQPS)
	if qps < minQPS {
		t.Errorf("throughput %.0f QPS below acceptance criterion of %d QPS", qps, minQPS)
	}
}

// TestListWorkspacesLargeScaleMemoryStability verifies that sustained traffic
// at 5,000 instance scale does not drift memory: heap after the load must
// return close to the pre-load baseline (snapshots are atomically swapped and
// per-request allocations are bounded by pageSize).
func TestListWorkspacesLargeScaleMemoryStability(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory stability test in -short mode")
	}

	wc := newLargeScaleCache(t, 5000)
	handler := listWorkspacesHandler(wc)
	fireRequests := func(n int) {
		for i := range n {
			paths := []string{
				"/api/workspaces",
				"/api/workspaces?page=1&pageSize=50",
				"/api/workspaces?page=2&pageSize=50&namespace=ns-7",
				"/api/workspaces?page=1&pageSize=50&owner=user-00042",
			}
			req := httptest.NewRequest("GET", paths[i%len(paths)], nil)
			rr := httptest.NewRecorder()
			handler(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("unexpected status %d", rr.Code)
			}
		}
	}

	gcHeap := func() uint64 {
		gomemstats.GC()
		gomemstats.GC()
		var m gomemstats.MemStats
		gomemstats.ReadMemStats(&m)
		return m.HeapAlloc
	}

	fireRequests(1000) // warm-up
	baseline := gcHeap()

	fireRequests(50000)
	after := gcHeap()

	growthMB := float64(int64(after)-int64(baseline)) / (1 << 20)
	t.Logf("heap: baseline=%.1fMB after=%.1fMB growth=%.2fMB over 50k requests", float64(baseline)/(1<<20), float64(after)/(1<<20), growthMB)
	if growthMB > 64 {
		t.Errorf("heap grew by %.2fMB over 50k requests — possible memory drift", growthMB)
	}
}

func BenchmarkListWorkspacesHandler5000(b *testing.B) {
	wc := newLargeScaleCache(b, 5000)
	handler := listWorkspacesHandler(wc)

	b.Run("paginated-50", func(b *testing.B) {
		req := httptest.NewRequest("GET", "/api/workspaces?page=3&pageSize=50", nil)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rr := httptest.NewRecorder()
			handler(rr, req)
		}
	})
	b.Run("namespace-filtered", func(b *testing.B) {
		req := httptest.NewRequest("GET", "/api/workspaces?namespace=ns-7", nil)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rr := httptest.NewRecorder()
			handler(rr, req)
		}
	})
	b.Run("legacy-full-list", func(b *testing.B) {
		req := httptest.NewRequest("GET", "/api/workspaces", nil)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rr := httptest.NewRecorder()
			handler(rr, req)
		}
	})
}
