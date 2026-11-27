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

package main

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/example/workspace-operator/internal/controller"
)

func TestNewClientOptionsCachesStorageClass(t *testing.T) {
	opts := newClientOptions()
	if opts.Cache == nil {
		t.Fatal("expected Client.Cache options to be configured")
	}
	var hasPV, hasPVC, hasSC bool
	for _, obj := range opts.Cache.DisableFor {
		switch obj.(type) {
		case *corev1.PersistentVolume:
			hasPV = true
		case *corev1.PersistentVolumeClaim:
			hasPVC = true
		case *storagev1.StorageClass:
			hasSC = true
		}
	}
	if !hasPV || !hasPVC {
		t.Fatalf("expected PersistentVolume and PersistentVolumeClaim in DisableFor, got pv=%v pvc=%v", hasPV, hasPVC)
	}
	if hasSC {
		t.Fatal("StorageClass must not be in DisableFor so reconciles use the informer cache")
	}
}

func TestStripPodForCache(t *testing.T) {
	now := metav1.Now()
	delTime := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "ws-a-pod",
			Namespace:         "default",
			UID:               "pod-uid-1",
			ResourceVersion:   "42",
			Labels:            map[string]string{"app": "workspace", "workspace": "ws-a"},
			Annotations:       map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{huge}"},
			DeletionTimestamp: &delTime,
			ManagedFields: []metav1.ManagedFieldsEntry{
				{Manager: "kube-controller-manager"},
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "runtime", Image: "nginx:alpine"}},
		},
		Status: corev1.PodStatus{
			Phase:     corev1.PodRunning,
			PodIP:     "10.0.0.5",
			StartTime: &now,
			HostIP:    "192.168.1.10",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "runtime", Ready: true},
			},
		},
	}

	gotObj, err := stripPodForCache(pod)
	if err != nil {
		t.Fatalf("stripPodForCache failed: %v", err)
	}
	got := gotObj.(*corev1.Pod)
	if got.Name != "ws-a-pod" || got.Namespace != "default" || got.UID != "pod-uid-1" {
		t.Fatalf("ObjectMeta identity lost: %+v", got.ObjectMeta)
	}
	if got.Labels["workspace"] != "ws-a" {
		t.Fatalf("labels lost: %+v", got.Labels)
	}
	if got.DeletionTimestamp == nil || !got.DeletionTimestamp.Equal(&delTime) {
		t.Fatalf("DeletionTimestamp lost: %v", got.DeletionTimestamp)
	}
	if got.ManagedFields != nil || got.Annotations != nil {
		t.Fatalf("expected ManagedFields and Annotations to be stripped, got mf=%v ann=%v",
			got.ManagedFields, got.Annotations)
	}
	if len(got.Spec.Containers) != 0 || got.Spec.NodeName != "" {
		t.Fatalf("expected PodSpec to be stripped, got %+v", got.Spec)
	}
	if got.Status.Phase != corev1.PodRunning || got.Status.PodIP != "10.0.0.5" ||
		got.Status.StartTime == nil || !got.Status.StartTime.Equal(&now) {
		t.Fatalf("expected Phase/PodIP/StartTime preserved, got %+v", got.Status)
	}
	if len(got.Status.ContainerStatuses) != 0 || len(got.Status.Conditions) != 0 || got.Status.HostIP != "" {
		t.Fatalf("expected bulky PodStatus fields to be stripped, got %+v", got.Status)
	}
}

func TestStripDeploymentForCache(t *testing.T) {
	replicas := int32(1)
	revLimit := int32(0)
	uid := int64(10001)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-a-deploy",
			Namespace: "default",
			Annotations: map[string]string{
				controller.DeploymentSpecHashAnnotation:            "abc123",
				"kubectl.kubernetes.io/last-applied-configuration": "{huge}",
				"deployment.kubernetes.io/revision":                "3",
			},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "workspace-operator"}},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas:             &replicas,
			RevisionHistoryLimit: &revLimit,
			Strategy:             appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "workspace"},
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{RunAsUser: &uid},
					Containers:      []corev1.Container{{Name: "runtime", Image: "nginx:alpine"}},
					Volumes:         []corev1.Volume{{Name: "workspace-data"}},
				},
			},
		},
	}

	gotObj, err := stripDeploymentForCache(deploy)
	if err != nil {
		t.Fatalf("stripDeploymentForCache failed: %v", err)
	}
	got := gotObj.(*appsv1.Deployment)
	if got.ManagedFields != nil {
		t.Fatalf("expected ManagedFields to be stripped, got %v", got.ManagedFields)
	}
	if len(got.Annotations) != 1 || got.Annotations[controller.DeploymentSpecHashAnnotation] != "abc123" {
		t.Fatalf("expected only spec-hash annotation to survive, got %+v", got.Annotations)
	}
	if got.Spec.Template.Spec.SecurityContext == nil || *got.Spec.Template.Spec.SecurityContext.RunAsUser != uid {
		t.Fatalf("expected Pod SecurityContext to survive, got %+v", got.Spec.Template.Spec.SecurityContext)
	}
	if len(got.Spec.Template.Spec.Containers) != 0 || len(got.Spec.Template.Spec.Volumes) != 0 {
		t.Fatalf("expected Containers and Volumes to be stripped, got %+v", got.Spec.Template.Spec)
	}

	noHashDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"unrelated": "value"},
		},
	}
	noHashObj, err := stripDeploymentForCache(noHashDeploy)
	if err != nil {
		t.Fatalf("stripDeploymentForCache without spec-hash failed: %v", err)
	}
	if noHashObj.(*appsv1.Deployment).Annotations != nil {
		t.Fatalf("expected nil Annotations when spec-hash is absent, got %+v", noHashObj.(*appsv1.Deployment).Annotations)
	}
}

func TestNewCacheOptionsRegistersPodAndDeploymentTransforms(t *testing.T) {
	opts := newCacheOptions()
	var podOpt, deployOpt bool
	for obj, byObj := range opts.ByObject {
		switch obj.(type) {
		case *corev1.Pod:
			podOpt = byObj.Transform != nil && byObj.Label != nil
		case *appsv1.Deployment:
			deployOpt = byObj.Transform != nil && byObj.Label != nil
		case client.Object:
		}
	}
	if !podOpt {
		t.Fatal("expected Pod cache option with label selector and transform")
	}
	if !deployOpt {
		t.Fatal("expected Deployment cache option with label selector and transform")
	}
}

func TestParseNamespaceRuleJSON(t *testing.T) {
	t.Run("empty value means no controller-wide exceptions", func(t *testing.T) {
		rules, err := parseNamespaceRuleJSON("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rules != nil {
			t.Fatalf("expected nil rules, got %+v", rules)
		}
	})

	t.Run("whitespace value means no controller-wide exceptions", func(t *testing.T) {
		rules, err := parseNamespaceRuleJSON("   \n\t ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rules != nil {
			t.Fatalf("expected nil rules, got %+v", rules)
		}
	})

	t.Run("valid rules", func(t *testing.T) {
		raw := `[{"namespace":"monitoring","podSelector":{"app.kubernetes.io/name":"otel-collector"},"ports":[4318]},{"namespace":"logging"}]`
		rules, err := parseNamespaceRuleJSON(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(rules) != 2 {
			t.Fatalf("expected 2 rules, got %d", len(rules))
		}
		if rules[0].Namespace != "monitoring" ||
			rules[0].PodSelector["app.kubernetes.io/name"] != "otel-collector" ||
			len(rules[0].Ports) != 1 || rules[0].Ports[0] != 4318 {
			t.Fatalf("unexpected first rule: %+v", rules[0])
		}
		if rules[1].Namespace != "logging" || rules[1].PodSelector != nil || rules[1].Ports != nil {
			t.Fatalf("unexpected second rule: %+v", rules[1])
		}
	})

	t.Run("malformed json is rejected", func(t *testing.T) {
		if _, err := parseNamespaceRuleJSON(`[{"namespace":`); err == nil {
			t.Fatal("expected an error for malformed JSON")
		}
	})

	t.Run("json object instead of array is rejected", func(t *testing.T) {
		if _, err := parseNamespaceRuleJSON(`{"namespace":"monitoring"}`); err == nil {
			t.Fatal("expected an error for a JSON object")
		}
	})

	t.Run("rule without namespace is rejected", func(t *testing.T) {
		if _, err := parseNamespaceRuleJSON(`[{"ports":[4318]}]`); err == nil {
			t.Fatal("expected an error for a missing namespace")
		}
	})
}
