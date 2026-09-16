package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

func TestListWorkspacesHandler(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = aiv1alpha1.AddToScheme(scheme)

	ws1 := &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-u1",
			Namespace: "bocomwork",
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

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&aiv1alpha1.Workspace{}).WithObjects(ws1, ws2).Build()
	handler := listWorkspacesHandler(fakeClient)

	t.Run("filter by namespace bocomwork", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/workspaces?namespace=bocomwork", nil)
		rr := httptest.NewRecorder()
		handler(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rr.Code)
		}
		var items []workspaceItem
		if err := json.Unmarshal(rr.Body.Bytes(), &items); err != nil {
			t.Fatalf("failed to unmarshal JSON: %v", err)
		}
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
		var items []workspaceItem
		if err := json.Unmarshal(rr.Body.Bytes(), &items); err != nil {
			t.Fatalf("failed to unmarshal JSON: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("expected 2 items, got %d", len(items))
		}
	})
}
