package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	ai "github.com/example/workspace-operator/api/v1alpha1"
	"github.com/example/workspace-operator/internal/egress"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func egressStatusHandler(k client.Client) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "Method not allowed", 405)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		c, _, err := egress.Read(r.Context(), k, egress.Namespace())
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		migrationState, pending, observeErr := observeEgressMigration(r.Context(), k, c.EffectiveMode())
		if observeErr != nil {
			http.Error(w, "Workspace observation unavailable: "+observeErr.Error(), 503)
			return
		}
		if c.EffectiveMode() != egress.ModeAllowlist {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"revision": c.Revision(), "targetMode": c.EffectiveMode(), "proxyState": "not_required", "desiredReplicas": 0, "replicas": []egress.ReplicaStatus{}, "policyState": "unverified", "migrationState": migrationState, "pendingWorkspaces": pending})
			return
		}
		deployment := &appsv1.Deployment{}
		if err = k.Get(r.Context(), types.NamespacedName{Namespace: egress.Namespace(), Name: "workspace-egress"}, deployment); err != nil {
			http.Error(w, "Proxy deployment unavailable: "+err.Error(), 503)
			return
		}
		pods := &corev1.PodList{}
		if err = k.List(r.Context(), pods, client.InNamespace(egress.Namespace()), client.MatchingLabels{"app.kubernetes.io/name": "workspace-egress"}); err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		desired := int32(1)
		if deployment.Spec.Replicas != nil {
			desired = *deployment.Spec.Replicas
		}
		replicas := []egress.ReplicaStatus{}
		state := "syncing"
		loaded := int32(0)
		for _, p := range pods.Items {
			if p.DeletionTimestamp != nil {
				continue
			}
			s := egress.ObserveReplica(p, c.Revision(), time.Now())
			replicas = append(replicas, s)
			if s.State == "failed" {
				state = "failed"
			}
			if s.State == "loaded" {
				loaded++
			}
		}
		if state != "failed" && desired > 0 && loaded == desired && int32(len(replicas)) == desired && deployment.Status.ObservedGeneration >= deployment.Generation {
			state = "loaded"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"revision": c.Revision(), "targetMode": c.EffectiveMode(), "proxyState": state, "desiredReplicas": desired, "replicas": replicas, "policyState": "unverified", "migrationState": migrationState, "pendingWorkspaces": pending})
	})
}

// This observes templates and live Pods, not CNI enforcement. Policy state stays
// unverified until independent policy/data-plane checks can establish it.
func observeEgressMigration(ctx context.Context, k client.Client, mode egress.Mode) (string, []string, error) {
	deployments := &appsv1.DeploymentList{}
	if err := k.List(ctx, deployments, client.MatchingLabels{"app": "workspace"}); err != nil {
		return "unverified", nil, err
	}
	pods := &corev1.PodList{}
	if err := k.List(ctx, pods, client.MatchingLabels{"app": "workspace"}); err != nil {
		return "unverified", nil, err
	}
	expected := ""
	if mode == egress.ModeAllowlist {
		expected = egress.MigrationVersion
	}
	if mode == egress.ModeUnrestricted {
		expected = "unrestricted-v1"
	}
	pending := []string{}
	workspaces := &ai.WorkspaceList{}
	if err := k.List(ctx, workspaces); err != nil {
		return "unverified", nil, err
	}
	for _, ws := range workspaces.Items {
		if ws.DeletionTimestamp != nil || ws.Spec.Stopped || ws.Status.Phase == ai.WorkspaceSleeping || ws.Status.Phase == ai.WorkspaceStopped {
			continue
		}
		found := false
		for _, d := range deployments.Items {
			if d.Namespace == ws.Namespace && d.Name == ws.Name+"-deploy" {
				found = true
				break
			}
		}
		if !found {
			pending = append(pending, ws.Namespace+"/"+ws.Name+" (Deployment missing)")
		}
	}

	for _, d := range deployments.Items {
		replicas := int32(1)
		if d.Spec.Replicas != nil {
			replicas = *d.Spec.Replicas
		}
		if replicas == 0 {
			continue
		}
		if d.Spec.Template.Labels[egress.MigrationLabel] != expected || d.Status.ObservedGeneration < d.Generation || d.Status.UpdatedReplicas != replicas || d.Status.ReadyReplicas != replicas {
			pending = append(pending, d.Namespace+"/"+d.Name)
		}
	}
	for _, p := range pods.Items {
		if p.Labels[egress.MigrationLabel] != expected || p.DeletionTimestamp != nil {
			pending = append(pending, p.Namespace+"/"+p.Name)
		}
	}
	if len(pending) > 0 {
		return "pending", pending, nil
	}
	return "observed", pending, nil
}
