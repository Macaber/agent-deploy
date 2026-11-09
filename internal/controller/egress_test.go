package controller

import (
	"context"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"testing"

	ai "github.com/example/workspace-operator/api/v1alpha1"
	"github.com/example/workspace-operator/internal/egress"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	net "k8s.io/api/networking/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEgressMigrationWaitsForAllLegacyPods(t *testing.T) {
	d := &apps.Deployment{}
	if eligibleForEgressMigration(d, []core.Pod{{}}) {
		t.Fatal("live legacy Pod eligible")
	}
	now := meta.Now()
	if eligibleForEgressMigration(d, []core.Pod{{ObjectMeta: meta.ObjectMeta{DeletionTimestamp: &now}}}) {
		t.Fatal("terminating legacy Pod eligible")
	}
	if !eligibleForEgressMigration(d, nil) {
		t.Fatal("sleeping Workspace cannot migrate")
	}
	d.Spec.Template.Labels = map[string]string{egress.MigrationLabel: egress.MigrationVersion}
	if !eligibleForEgressMigration(d, []core.Pod{{}}) {
		t.Fatal("daily config sync requires restart")
	}
}
func TestEgressProxyEnvStableAndNoUserOverride(t *testing.T) {
	env := proxyEnvironment([]core.EnvVar{{Name: "HTTPS_PROXY", Value: "http://bypass"}, {Name: "ALL_PROXY", Value: "socks://bypass"}, {Name: "TOKEN", Value: "preserved"}})
	count := 0
	for _, v := range env {
		if v.Name == "ALL_PROXY" {
			t.Fatal("ALL_PROXY survived")
		}
		if v.Name == "HTTPS_PROXY" {
			count++
			if v.Value == "http://bypass" {
				t.Fatal("proxy override survived")
			}
		}
	}
	if count != 1 || env[0].Name != "TOKEN" {
		t.Fatal("unexpected env", env)
	}
}
func TestEgressMigrationBlocksAdditivePolicyWithoutDeletingIt(t *testing.T) {
	t.Setenv("EGRESS_ENABLED", "true")
	t.Setenv("EGRESS_CONFIG_NAMESPACE", "system")
	t.Setenv("EGRESS_PROTECTED_CIDRS", "10.0.0.0/8")
	scheme := runtime.NewScheme()
	_ = core.AddToScheme(scheme)
	_ = net.AddToScheme(scheme)
	_ = ai.AddToScheme(scheme)
	cm := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: egress.ConfigMapName, Namespace: "system"}, Data: map[string]string{egress.ConfigKey: `{"ips":[],"domains":[],"services":[{"name":"dns","namespace":"kube-system","podSelector":{"k8s-app":"kube-dns"},"protocol":"TCP","port":53},{"name":"dns","namespace":"kube-system","podSelector":{"k8s-app":"kube-dns"},"protocol":"UDP","port":53}]}`}}
	legacy := &net.NetworkPolicy{ObjectMeta: meta.ObjectMeta{Name: "workspace-test-whitelist", Namespace: "work"}, Spec: net.NetworkPolicySpec{PodSelector: meta.LabelSelector{MatchLabels: map[string]string{"app": "workspace"}}, Egress: []net.NetworkPolicyEgressRule{{}}}}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm, legacy).Build()
	r := &WorkspaceReconciler{Client: k}
	ws := &ai.Workspace{ObjectMeta: meta.ObjectMeta{Name: "example", Namespace: "work"}}
	if _, err := r.prepareUnifiedEgress(context.Background(), ws, &apps.Deployment{}); err == nil {
		t.Fatal("additive policy accepted")
	}
	found := &net.NetworkPolicyList{}
	if err := k.List(context.Background(), found); err != nil {
		t.Fatal(err)
	}
	if len(found.Items) != 1 || found.Items[0].Name != legacy.Name {
		t.Fatal("test policy modified")
	}
}

func TestEgressConfigFailureDoesNotBlockStop(t *testing.T) {
	t.Setenv("EGRESS_ENABLED", "true")
	t.Setenv("EGRESS_CONFIG_NAMESPACE", "missing")
	t.Setenv("EGRESS_PROTECTED_CIDRS", "")
	scheme := runtime.NewScheme()
	_ = apps.AddToScheme(scheme)
	_ = core.AddToScheme(scheme)
	one := int32(1)
	d := &apps.Deployment{ObjectMeta: meta.ObjectMeta{Name: "example-deploy", Namespace: "work"}, Spec: apps.DeploymentSpec{Replicas: &one, Template: core.PodTemplateSpec{ObjectMeta: meta.ObjectMeta{Labels: map[string]string{egress.MigrationLabel: egress.MigrationVersion}}}}}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d).Build()
	r := &WorkspaceReconciler{Client: k}
	ws := &ai.Workspace{ObjectMeta: meta.ObjectMeta{Name: "example", Namespace: "work"}, Spec: ai.WorkspaceSpec{Stopped: true}}
	_, replicas, err := r.reconcileDeployment(context.Background(), ws, "")
	if err != nil || replicas != 0 {
		t.Fatalf("stop failed: %d %v", replicas, err)
	}
	got := &apps.Deployment{}
	if err = k.Get(context.Background(), client.ObjectKeyFromObject(d), got); err != nil {
		t.Fatal(err)
	}
	if *got.Spec.Replicas != 0 || got.Spec.Template.Labels[egress.MigrationLabel] != egress.MigrationVersion {
		t.Fatal("scale down changed migration template")
	}
}

func TestEgressPreflightReportsMissingDNS(t *testing.T) {
	if err := (egress.Config{}).MigrationCheck(); err == nil || !strings.Contains(err.Error(), "DNS") {
		t.Fatal("missing explicit DNS diagnostic", err)
	}
}

func TestEgressMigrationRequiresProxyDeployment(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = apps.AddToScheme(scheme)
	_ = core.AddToScheme(scheme)
	k := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &WorkspaceReconciler{Client: k}
	if err := r.requireLoadedProxy(context.Background(), "revision"); err == nil {
		t.Fatal("missing proxy accepted")
	}
	zero := int32(0)
	d := &apps.Deployment{ObjectMeta: meta.ObjectMeta{Name: "workspace-egress", Namespace: egress.Namespace()}, Spec: apps.DeploymentSpec{Replicas: &zero}}
	if err := k.Create(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := r.requireLoadedProxy(context.Background(), "revision"); err == nil {
		t.Fatal("zero proxy replicas accepted")
	}
}
