package controller

import (
	"context"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	ai "github.com/example/workspace-operator/api/v1alpha1"
	"github.com/example/workspace-operator/internal/egress"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Broadcast pacing for shared-object watches (the egress ConfigMap and the
// NetworkPolicy templates). A naive mapping handler dumps every Workspace
// into the queue at once: with tens of thousands of Workspaces a single
// config save enqueues the whole fleet behind one event, starving ordinary
// per-Workspace events and concentrating reconcile load into a burst. The
// handlers below spread that work out instead:
//
//   - batches: requests enter the queue in bounded groups spaced by
//     batchInterval (workqueue AddAfter), so the queue always keeps slack
//     for other events and consumers see a steady rate, never a cliff;
//   - coalescing: a burst of source-object changes re-paces itself behind
//     the previous broadcast, merging into one paced pass instead of
//     stacking N full passes on top of each other;
//   - bounded: the workqueue deduplicates by key, so a Workspace already
//     queued stays a single pending entry no matter how often the source
//     object changes.
//
// Correctness is unaffected: every Workspace is eventually enqueued and each
// reconcile reads the latest configuration, so propagation only becomes
// paced, never lost.
type broadcastPacer struct {
	batchSize     int
	batchInterval time.Duration
	coalesce      time.Duration
	lastBase      atomic.Int64 // pacing base (unix nanos) of the most recent broadcast
}

func newBroadcastPacer() *broadcastPacer {
	p := &broadcastPacer{batchSize: 500, batchInterval: 2 * time.Second, coalesce: 5 * time.Second}
	if v, err := strconv.Atoi(os.Getenv("EGRESS_BROADCAST_BATCH_SIZE")); err == nil && v > 0 {
		p.batchSize = v
	}
	if d, err := time.ParseDuration(os.Getenv("EGRESS_BROADCAST_BATCH_INTERVAL")); err == nil && d > 0 {
		p.batchInterval = d
	}
	if d, err := time.ParseDuration(os.Getenv("EGRESS_BROADCAST_COALESCE")); err == nil && d > 0 {
		p.coalesce = d
	}
	return p
}

// base returns how long this broadcast must wait before its first batch and
// advances the shared pacing point. A broadcast started inside the coalescing
// window is pushed behind the previous one so bursts merge into a single
// paced pass; the first broadcast after a quiet period starts immediately.
func (p *broadcastPacer) base() time.Duration {
	for {
		last := p.lastBase.Load()
		now := time.Now().UnixNano()
		next := now
		delay := time.Duration(0)
		if last > 0 {
			if wait := p.coalesce - time.Duration(now-last); wait > 0 {
				delay = wait
				next = last + int64(p.coalesce)
			}
		}
		if p.lastBase.CompareAndSwap(last, next) {
			return delay
		}
	}
}

// batchDelay is the queue delay for request index i: batches of batchSize
// share one slot, spaced batchInterval apart, after base.
func (p *broadcastPacer) batchDelay(i int, base time.Duration) time.Duration {
	return base + time.Duration(i/p.batchSize)*p.batchInterval
}

func (p *broadcastPacer) pace(q workqueue.TypedRateLimitingInterface[reconcile.Request], reqs []reconcile.Request) {
	base := p.base()
	for i, req := range reqs {
		q.AddAfter(req, p.batchDelay(i, base))
	}
}

func workspaceRequests(items []ai.Workspace) []reconcile.Request {
	out := make([]reconcile.Request, 0, len(items))
	for i := range items {
		if !items[i].DeletionTimestamp.IsZero() {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&items[i])})
	}
	return out
}

// egressConfigBroadcastHandler replaces a mapping handler that enqueued every
// Workspace at once when the egress ConfigMap changed.
func egressConfigBroadcastHandler(r *WorkspaceReconciler) handler.EventHandler {
	pacer := newBroadcastPacer()
	handle := func(ctx context.Context, obj client.Object, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
		if !unifiedEgressEnabled() || obj.GetNamespace() != egress.Namespace() || obj.GetName() != egress.ConfigMapName {
			return
		}
		list := &ai.WorkspaceList{}
		if err := r.List(ctx, list); err != nil {
			return
		}
		pacer.pace(q, workspaceRequests(list.Items))
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.TypedCreateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			handle(ctx, e.Object, q)
		},
		UpdateFunc: func(ctx context.Context, e event.TypedUpdateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			handle(ctx, e.ObjectNew, q)
		},
		DeleteFunc: func(ctx context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			handle(ctx, e.Object, q)
		},
		GenericFunc: func(ctx context.Context, e event.TypedGenericEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			handle(ctx, e.Object, q)
		},
	}
}

// netpolBroadcastHandler watches the shared NetworkPolicy templates. Outside
// unified egress it keeps the historical behavior: enqueue a single
// representative Workspace (smallest name) so the shared template is
// re-converged without an N-request burst or concurrent workers fighting
// over the same Update. Under unified egress every live Workspace must
// re-converge, so the work is paced in batches instead of dumped at once.
func netpolBroadcastHandler(r *WorkspaceReconciler) handler.EventHandler {
	pacer := newBroadcastPacer()
	handle := func(ctx context.Context, obj client.Object, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
		wsList := &ai.WorkspaceList{}
		if err := r.List(ctx, wsList, client.InNamespace(obj.GetNamespace())); err != nil {
			return
		}
		keys := make(map[string]reconcile.Request, len(wsList.Items))
		names := make([]string, 0, len(wsList.Items))
		for i := range wsList.Items {
			item := &wsList.Items[i]
			if !item.DeletionTimestamp.IsZero() {
				continue
			}
			names = append(names, item.Name)
			keys[item.Name] = reconcile.Request{NamespacedName: client.ObjectKeyFromObject(item)}
		}
		if len(names) == 0 {
			return
		}
		if !unifiedEgressEnabled() {
			q.Add(keys[slices.Min(names)])
			return
		}
		reqs := make([]reconcile.Request, 0, len(names))
		for _, name := range names {
			reqs = append(reqs, keys[name])
		}
		pacer.pace(q, reqs)
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.TypedCreateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			handle(ctx, e.Object, q)
		},
		UpdateFunc: func(ctx context.Context, e event.TypedUpdateEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			handle(ctx, e.ObjectNew, q)
		},
		DeleteFunc: func(ctx context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			handle(ctx, e.Object, q)
		},
		GenericFunc: func(ctx context.Context, e event.TypedGenericEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			handle(ctx, e.Object, q)
		},
	}
}
