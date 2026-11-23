package controller

import (
	"context"
	"testing"
	"time"

	ai "github.com/example/workspace-operator/api/v1alpha1"
	"github.com/example/workspace-operator/internal/egress"
	core "k8s.io/api/core/v1"
	net "k8s.io/api/networking/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestBroadcastPacerFirstBurstStartsImmediately(t *testing.T) {
	p := &broadcastPacer{batchSize: 10, batchInterval: time.Second, coalesce: 5 * time.Second}
	if d := p.base(); d != 0 {
		t.Fatalf("first broadcast must start immediately, got %v", d)
	}
	d := p.base()
	if d <= 0 || d > 5*time.Second {
		t.Fatalf("broadcast inside coalesce window must be deferred, got %v", d)
	}
}

func TestBroadcastPacerBatchDelay(t *testing.T) {
	p := &broadcastPacer{batchSize: 2, batchInterval: time.Second, coalesce: time.Hour}
	want := []time.Duration{0, 0, time.Second, time.Second, 2 * time.Second}
	for i, w := range want {
		if got := p.batchDelay(i, 0); got != w {
			t.Fatalf("batchDelay(%d)=%v, want %v", i, got, w)
		}
	}
	base := 3 * time.Second
	if got := p.batchDelay(4, base); got != base+2*time.Second {
		t.Fatalf("batchDelay must add base, got %v", got)
	}
}

func newBroadcastTestClient(t *testing.T, namespace string, names ...string) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := ai.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := []client.Object{}
	for _, name := range names {
		objs = append(objs, &ai.Workspace{ObjectMeta: meta.ObjectMeta{Namespace: namespace, Name: name}})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return c
}

func TestEgressConfigBroadcastHandlerEnqueuesAllPaced(t *testing.T) {
	t.Setenv("EGRESS_ENABLED", "true")
	t.Setenv("EGRESS_CONFIG_NAMESPACE", "system")
	t.Setenv("EGRESS_BROADCAST_BATCH_SIZE", "2")
	t.Setenv("EGRESS_BROADCAST_BATCH_INTERVAL", "10ms")
	r := &WorkspaceReconciler{Client: newBroadcastTestClient(t, "bocomwork", "ws-a", "ws-b", "ws-c")}
	h := egressConfigBroadcastHandler(r)

	q := workqueue.NewTypedRateLimitingQueue[reconcile.Request](workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	t.Cleanup(q.ShutDown)

	cm := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "system", Name: egress.ConfigMapName}}
	h.Update(context.Background(), event.TypedUpdateEvent[client.Object]{ObjectNew: cm}, q)

	got := map[types.NamespacedName]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < 3 && time.Now().Before(deadline) {
		item, shutdown := q.Get()
		if shutdown {
			break
		}
		q.Done(item)
		got[item.NamespacedName] = true
	}
	if len(got) != 3 {
		t.Fatalf("expected all 3 workspaces enqueued, got %v", got)
	}
}

func TestEgressConfigBroadcastHandlerIgnoresOtherConfigMaps(t *testing.T) {
	t.Setenv("EGRESS_ENABLED", "true")
	t.Setenv("EGRESS_CONFIG_NAMESPACE", "system")
	r := &WorkspaceReconciler{Client: newBroadcastTestClient(t, "bocomwork", "ws-a")}
	h := egressConfigBroadcastHandler(r)
	q := workqueue.NewTypedRateLimitingQueue[reconcile.Request](workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	h.Update(context.Background(), event.TypedUpdateEvent[client.Object]{ObjectNew: &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Namespace: "system", Name: "unrelated"}}}, q)
	if q.Len() != 0 {
		t.Fatalf("unrelated configmap must not enqueue anything, len=%d", q.Len())
	}
}

func TestNetpolBroadcastHandlerSingleRepresentativeWhenDisabled(t *testing.T) {
	r := &WorkspaceReconciler{Client: newBroadcastTestClient(t, "bocomwork", "ws-b", "ws-a", "ws-c")}
	h := netpolBroadcastHandler(r)
	q := workqueue.NewTypedRateLimitingQueue[reconcile.Request](workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	h.Update(context.Background(), event.TypedUpdateEvent[client.Object]{ObjectNew: &net.NetworkPolicy{ObjectMeta: meta.ObjectMeta{Namespace: "bocomwork", Name: "workspace-egress-v1"}}}, q)
	if q.Len() != 1 {
		t.Fatalf("expected exactly one representative request, len=%d", q.Len())
	}
	item, _ := q.Get()
	if item.Name != "ws-a" {
		t.Fatalf("expected smallest-name representative ws-a, got %v", item.NamespacedName)
	}
}
