package main

import (
	"encoding/json"
	"net/http"
	"time"

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
		_ = json.NewEncoder(w).Encode(map[string]any{"revision": c.Revision(), "proxyState": state, "desiredReplicas": desired, "replicas": replicas, "policyState": "unverified", "migrationState": "unverified"})
	})
}
