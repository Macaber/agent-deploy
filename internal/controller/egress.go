package controller

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	ai "github.com/example/workspace-operator/api/v1alpha1"
	"github.com/example/workspace-operator/internal/egress"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	net "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func unifiedEgressEnabled() bool { return os.Getenv("EGRESS_ENABLED") == "true" }

func (r *WorkspaceReconciler) prepareUnifiedEgress(ctx context.Context, ws *ai.Workspace, d *apps.Deployment) (bool, error) {
	if !unifiedEgressEnabled() {
		return false, nil
	}
	c, _, err := egress.Read(ctx, r.Client, egress.Namespace())
	if err != nil {
		return false, err
	}
	return r.prepareEgressConfig(ctx, ws, c)
}

// Use one configuration snapshot for both policies and the Pod template.
func (r *WorkspaceReconciler) prepareEgressConfig(ctx context.Context, ws *ai.Workspace, c egress.Config) (bool, error) {
	if err := egress.RememberConfiguration(ctx, r.Client, egress.Namespace()); err != nil {
		return false, err
	}
	if err := r.cleanupUnusedEgressPolicies(ctx, ws.Namespace, c.EffectiveMode()); err != nil {
		return false, err
	}
	if c.EffectiveMode() != egress.ModeAllowlist {
		// Policies select immutable per-Pod mode labels. Old proxy Pods retain
		// their strict policy until rollout removes them; no union opens them.
		if c.EffectiveMode() == egress.ModeUnrestricted {
			policy := &net.NetworkPolicy{ObjectMeta: meta.ObjectMeta{Name: "workspace-egress-unrestricted", Namespace: ws.Namespace}, Spec: net.NetworkPolicySpec{PodSelector: meta.LabelSelector{MatchLabels: map[string]string{"app": "workspace", egress.MigrationLabel: "unrestricted-v1"}}, PolicyTypes: []net.PolicyType{net.PolicyTypeEgress}, Egress: []net.NetworkPolicyEgressRule{{}}}}
			if err := r.upsertEgressPolicy(ctx, policy); err != nil {
				return false, err
			}
		}
		return false, r.ensureSharedNetworkPolicy(ctx, ws.Namespace)
	}
	if err := c.MigrationCheck(); err != nil {
		return false, err
	}
	protected := []string{}
	for _, s := range strings.Split(os.Getenv("EGRESS_PROTECTED_CIDRS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			protected = append(protected, s)
		}
	}
	desired, err := c.WorkspacePolicy(ws.Namespace, egress.Namespace(), protected)
	if err != nil {
		return false, err
	}
	// Refuse migration if another policy would add egress allowances. Do not
	// silently delete or rewrite administrator-owned/test policies.
	candidate := buildPodLabels(ws)
	candidate[egress.MigrationLabel] = egress.MigrationVersion
	policies := &net.NetworkPolicyList{}
	if err = r.List(ctx, policies, client.InNamespace(ws.Namespace)); err != nil {
		return false, err
	}
	for _, p := range policies.Items {
		if p.Name == sharedNetpolName || p.Name == desired.Name {
			continue
		}
		sel, e := meta.LabelSelectorAsSelector(&p.Spec.PodSelector)
		if e != nil {
			return false, e
		}
		if sel.Matches(labels.Set(candidate)) && len(p.Spec.Egress) > 0 {
			return false, fmt.Errorf("migration blocked by additive egress policy %s/%s", p.Namespace, p.Name)
		}
	}
	if err := r.requireLoadedProxy(ctx, c.Revision()); err != nil {
		return false, err
	}
	// Preserve legacy ingress separately before excluding migrated Pods from
	// the old combined ingress/egress policy. Disabled legacy Pods retain their
	// absent managed-by label; migration does not force an ingress change.
	ingress := &net.NetworkPolicy{ObjectMeta: meta.ObjectMeta{Name: "workspace-shared-ingress", Namespace: ws.Namespace}, Spec: net.NetworkPolicySpec{PodSelector: meta.LabelSelector{MatchLabels: map[string]string{managedByLabelKey: managedByLabelValue}}, PolicyTypes: []net.PolicyType{net.PolicyTypeIngress}, Ingress: []net.NetworkPolicyIngressRule{{}}}}
	if err = r.upsertEgressPolicy(ctx, ingress); err != nil {
		return false, err
	}
	if err = r.upsertEgressPolicy(ctx, desired); err != nil {
		return false, err
	}
	if err = r.ensureSharedNetworkPolicy(ctx, ws.Namespace); err != nil {
		return false, err
	}
	return true, nil
}

func (r *WorkspaceReconciler) upsertEgressPolicy(ctx context.Context, desired *net.NetworkPolicy) error {
	existing := &net.NetworkPolicy{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Merge patch preserves metadata belonging to other actors.
	before := existing.DeepCopy()
	existing.Spec = desired.Spec
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	for k, v := range desired.Annotations {
		existing.Annotations[k] = v
	}
	if reflect.DeepEqual(before.Spec, existing.Spec) && reflect.DeepEqual(before.Annotations, existing.Annotations) {
		return nil
	}
	return r.Patch(ctx, existing, client.MergeFrom(before))
}

func proxyEnvironment(env []core.EnvVar) []core.EnvVar {
	out := make([]core.EnvVar, 0, len(env)+6)
	for _, v := range env {
		switch strings.ToUpper(v.Name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "WORKSPACE_POD_IP":
			continue
		}
		out = append(out, v)
	}
	out = append(out, core.EnvVar{Name: "WORKSPACE_POD_IP", ValueFrom: &core.EnvVarSource{FieldRef: &core.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}})
	proxy := "http://workspace-egress." + egress.Namespace() + ".svc:3128"
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		out = append(out, core.EnvVar{Name: name, Value: proxy})
	}
	// Stable local/cluster service routing bypass does not authorize network access.
	// NetworkPolicy still enforces every direct destination. IP direct callers select direct mode;
	// changing allowlists never changes these env vars or rolls Workspace Pods.
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		out = append(out, core.EnvVar{Name: name, Value: "localhost,127.0.0.1,::1,$(WORKSPACE_POD_IP),.svc,.svc.cluster.local"})
	}
	return out
}

func (r *WorkspaceReconciler) mapEgressConfig(ctx context.Context, obj client.Object) []reconcile.Request {
	if !unifiedEgressEnabled() || obj.GetNamespace() != egress.Namespace() || obj.GetName() != egress.ConfigMapName {
		return nil
	}
	list := &ai.WorkspaceList{}
	if err := r.List(ctx, list); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for _, ws := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&ws)})
	}
	return out
}

// New/waking Pods cannot switch to proxy-only egress until all desired proxy
// replicas have acknowledged the exact revision. A missing deployment is not
// equivalent to an empty successfully-loaded replica set.
func (r *WorkspaceReconciler) requireLoadedProxy(ctx context.Context, revision string) error {
	d := &apps.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: egress.Namespace(), Name: "workspace-egress"}, d); err != nil {
		return fmt.Errorf("proxy is not ready for migration: %w", err)
	}
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	if desired < 1 || d.Status.ObservedGeneration < d.Generation {
		return fmt.Errorf("proxy deployment has not converged")
	}
	pods := &core.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(egress.Namespace()), client.MatchingLabels{"app.kubernetes.io/name": "workspace-egress"}); err != nil {
		return err
	}
	active := int32(0)
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		active++
		if s := egress.ObserveReplica(p, revision, time.Now()); s.State != "loaded" {
			return fmt.Errorf("proxy %s has not loaded revision %s: %s", p.Name, revision, s.State)
		}
	}
	if active != desired {
		return fmt.Errorf("proxy replica count %d does not match desired %d", active, desired)
	}
	return nil
}

// cleanupUnusedEgressPolicies waits for all old Pods (including terminating
// Pods) and active Deployment templates before removing a mode-specific policy.
func (r *WorkspaceReconciler) cleanupUnusedEgressPolicies(ctx context.Context, namespace string, target egress.Mode) error {
	pods := &core.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return err
	}
	deployments := &apps.DeploymentList{}
	if err := r.List(ctx, deployments, client.InNamespace(namespace)); err != nil {
		return err
	}
	for _, item := range []struct {
		mode        egress.Mode
		label, name string
	}{
		{egress.ModeUnrestricted, "unrestricted-v1", "workspace-egress-unrestricted"},
		{egress.ModeAllowlist, egress.MigrationVersion, "workspace-egress-v1"},
	} {
		if target == item.mode {
			continue
		}
		used := false
		for _, p := range pods.Items {
			if p.Labels[egress.MigrationLabel] == item.label {
				used = true
			}
		}
		for _, d := range deployments.Items {
			if d.Spec.Replicas != nil && *d.Spec.Replicas == 0 {
				continue
			}
			if d.Spec.Template.Labels[egress.MigrationLabel] == item.label {
				used = true
			}
		}
		if used {
			continue
		}
		policy := &net.NetworkPolicy{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: item.name}, policy); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		// Refuse to delete a same-name resource whose selector was repurposed.
		if policy.Spec.PodSelector.MatchLabels[egress.MigrationLabel] != item.label {
			return fmt.Errorf("refusing cleanup of modified policy %s/%s", namespace, item.name)
		}
		if err := r.Delete(ctx, policy); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
