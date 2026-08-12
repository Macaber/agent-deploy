package main

import (
	"context"
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

func TestGetEffectiveUserID(t *testing.T) {
	t.Run("non-bocomwork namespace returns original userId", func(t *testing.T) {
		envs := []aiv1alpha1.EnvVar{{Name: "USER_CODE", Value: "code123"}}
		got := getEffectiveUserID("user1", "default", envs)
		if got != "user1" {
			t.Errorf("expected 'user1', got '%s'", got)
		}
	})

	t.Run("bocomwork namespace without USER_CODE returns original userId", func(t *testing.T) {
		envs := []aiv1alpha1.EnvVar{{Name: "OTHER_VAR", Value: "code123"}}
		got := getEffectiveUserID("user1", "bocomwork", envs)
		if got != "user1" {
			t.Errorf("expected 'user1', got '%s'", got)
		}
	})

	t.Run("bocomwork namespace with empty USER_CODE returns original userId", func(t *testing.T) {
		envs := []aiv1alpha1.EnvVar{{Name: "USER_CODE", Value: "   "}}
		got := getEffectiveUserID("user1", "bocomwork", envs)
		if got != "user1" {
			t.Errorf("expected 'user1', got '%s'", got)
		}
	})

	t.Run("bocomwork namespace with USER_CODE returns USER_CODE value", func(t *testing.T) {
		envs := []aiv1alpha1.EnvVar{{Name: "USER_CODE", Value: "code123"}}
		got := getEffectiveUserID("user1", "bocomwork", envs)
		if got != "code123" {
			t.Errorf("expected 'code123', got '%s'", got)
		}
	})
}

func TestCreateWorkspace_BocomworkUserCode(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = aiv1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&aiv1alpha1.Workspace{}).Build()

	req := &workspaceRequest{
		UserID:    "original_user",
		Namespace: "bocomwork",
		Image:     "bocomwork:v1",
		Env: []aiv1alpha1.EnvVar{
			{Name: "USER_CODE", Value: "user_code_999"},
		},
	}

	effectiveUserID := getEffectiveUserID(req.UserID, req.Namespace, req.Env)
	if effectiveUserID != "user_code_999" {
		t.Fatalf("expected effectiveUserID 'user_code_999', got '%s'", effectiveUserID)
	}

	wsName := sanitizeK8sName("ws-" + effectiveUserID)
	if wsName != "ws-user-code-999" {
		t.Fatalf("expected wsName 'ws-user-code-999', got '%s'", wsName)
	}

	err := createNewWorkspace(context.Background(), fakeClient, req, wsName, req.Namespace, effectiveUserID)
	if err != nil {
		t.Fatalf("createNewWorkspace failed: %v", err)
	}

	created := &aiv1alpha1.Workspace{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{Namespace: "bocomwork", Name: "ws-user-code-999"}, created)
	if err != nil {
		t.Fatalf("failed to fetch created workspace: %v", err)
	}

	if created.Spec.Owner != "user_code_999" {
		t.Errorf("expected Spec.Owner to be 'user_code_999', got '%s'", created.Spec.Owner)
	}
}
