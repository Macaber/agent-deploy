package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

// This file implements the local Workspace informer cache required by the
// large-scale optimization plan (Task 4.2): the API server must NEVER issue a
// full, unpaginated List against the K8s API Server on the hot request path.
//
// Design (materialized view over a shared informer):
//   1. A single cluster-wide informer watches Workspace CRs (paginated initial
//      LIST + incremental WATCH, driven by the K8s API server itself).
//   2. Every informer event projects the CR into the small `workspaceItem` DTO
//      and upserts it into a pending map. Full CR objects are never retained.
//   3. Index rebuilds are debounced (coalesced bursts into one rebuild) and the
//      finished snapshot (sorted slice + namespace/owner/OA indexes) is
//      published atomically. Old snapshots become garbage immediately, so heap
//      usage stays bounded — no drift.
//   4. Request path: atomic pointer load + slice window + JSON encode. No
//      K8s API Server traffic, no locks held across encoding. The unfiltered,
//      non-paginated response allocates nothing (pre-encoded bytes are written
//      verbatim); filtered/paginated responses allocate one int32 offset slice
//      over the full result set plus the page window itself.
//
// The unfiltered, non-paginated portal response is pre-encoded once per
// rebuild so the hottest path is a single w.Write of ready bytes.

// workspaceSnapshot is an immutable materialized view of all Workspace CRs.
// Instances are published via atomic.Pointer and are safe for concurrent reads.
type workspaceSnapshot struct {
	// all is the full item list, sorted by namespace then name, so pages are
	// stable across requests.
	all []workspaceItem
	// byNamespace / byOwner / byOA map a filter value to offsets into `all`
	// (same sorted order). Only non-empty values are indexed.
	byNamespace map[string][]int32
	byOwner     map[string][]int32
	byOA        map[string][]int32
	// allJSON is the pre-encoded bare-array JSON of `all` — the exact response
	// shape of the legacy non-paginated, unfiltered list (portal UI contract).
	allJSON []byte
}

// workspaceCache maintains the materialized view from informer events and
// implements toolscache.ResourceEventHandler.
type workspaceCache struct {
	// rebuildDelay coalesces bursts of events into one index rebuild.
	rebuildDelay time.Duration

	mu      sync.RWMutex
	pending map[string]workspaceItem // key: "<namespace>/<name>"
	timer   *time.Timer

	snap  atomic.Pointer[workspaceSnapshot]
	ready atomic.Bool
}

// emptyWorkspaceSnapshot is the snapshot served before the first rebuild.
var emptyWorkspaceSnapshot = buildSnapshot([]workspaceItem{})

// newWorkspaceCache creates a cache with an empty snapshot already published,
// so early requests see a valid empty list instead of a nil dereference.
func newWorkspaceCache() *workspaceCache {
	c := &workspaceCache{
		rebuildDelay: cacheRebuildDelay(),
		pending:      make(map[string]workspaceItem),
	}
	c.snap.Store(emptyWorkspaceSnapshot)
	return c
}

// snapshot returns the current published snapshot; never nil.
func (c *workspaceCache) snapshot() *workspaceSnapshot {
	if s := c.snap.Load(); s != nil {
		return s
	}
	return emptyWorkspaceSnapshot
}

// size returns the number of workspaces in the current snapshot.
func (c *workspaceCache) size() int {
	return len(c.snapshot().all)
}

// isReady reports whether at least one successful rebuild has completed.
func (c *workspaceCache) isReady() bool {
	return c.ready.Load()
}

// get returns the projected view of a single Workspace. It reads the pending
// map rather than the published snapshot so a caller observes the newest
// informer event even while a debounced rebuild is still armed; the cost is one
// short read lock. Misses mean "the informer has not delivered this object
// yet" (freshly created) or "it is gone" (deleted) — callers decide whether to
// fall back to a direct API read.
func (c *workspaceCache) get(namespace, name string) (workspaceItem, bool) {
	if c == nil {
		return workspaceItem{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	item, ok := c.pending[namespace+"/"+name]
	return item, ok
}

// --- ResourceEventHandler implementation (informer events) ---

// OnAdd projects a Workspace CR into the view. Called for initial-list objects
// and watch ADD events.
func (c *workspaceCache) OnAdd(obj any, _ bool) {
	c.upsert(obj)
}

// OnUpdate replaces the projected item. Also fired by periodic resync, which
// doubles as a self-healing safety net against missed watch events.
func (c *workspaceCache) OnUpdate(_, newObj any) {
	c.upsert(newObj)
}

// OnDelete removes the projected item. Handles tombstones for objects whose
// last state is unknown (e.g. after a watch reconnection).
func (c *workspaceCache) OnDelete(obj any) {
	key, err := toolscache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Printf("Workspace cache: could not derive key for deleted object: %v", err)
		return
	}
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
	c.scheduleRebuild()
}

func (c *workspaceCache) upsert(obj any) {
	ws, ok := obj.(*aiv1alpha1.Workspace)
	if !ok || ws == nil {
		return
	}
	key := ws.Namespace + "/" + ws.Name
	c.mu.Lock()
	c.pending[key] = toWorkspaceItem(ws)
	c.mu.Unlock()
	c.scheduleRebuild()
}

// scheduleRebuild arms a debounced rebuild if none is pending.
func (c *workspaceCache) scheduleRebuild() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		return // a rebuild is already scheduled; it will see this change
	}
	c.timer = time.AfterFunc(c.rebuildDelay, c.rebuild)
}

// rebuild materializes a new snapshot from the pending map and publishes it
// atomically. Safe to call directly (initial sync, tests) or via the timer.
func (c *workspaceCache) rebuild() {
	c.mu.Lock()
	c.timer = nil
	items := make([]workspaceItem, 0, len(c.pending))
	for _, it := range c.pending {
		items = append(items, it)
	}
	c.mu.Unlock()

	snap := buildSnapshot(items)
	c.snap.Store(snap)
	c.ready.Store(true)
}

// rebuildNow forces an immediate rebuild, cancelling any pending debounce.
func (c *workspaceCache) rebuildNow() {
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
	c.rebuild()
}

// buildSnapshot sorts items, builds filter indexes and pre-encodes the
// legacy bare-array JSON for the hot unfiltered path.
func buildSnapshot(items []workspaceItem) *workspaceSnapshot {
	slices.SortStableFunc(items, func(a, b workspaceItem) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})

	snap := &workspaceSnapshot{
		all:         items,
		byNamespace: make(map[string][]int32),
		byOwner:     make(map[string][]int32),
		byOA:        make(map[string][]int32),
	}
	for i := range snap.all {
		it := &snap.all[i]
		if it.Namespace != "" {
			snap.byNamespace[it.Namespace] = append(snap.byNamespace[it.Namespace], int32(i))
		}
		if it.UserID != "" {
			snap.byOwner[it.UserID] = append(snap.byOwner[it.UserID], int32(i))
		}
		if it.OA != "" {
			snap.byOA[it.OA] = append(snap.byOA[it.OA], int32(i))
		}
	}
	if b, err := json.Marshal(snap.all); err == nil {
		snap.allJSON = b
	}
	return snap
}

// matchOffsets returns the offsets into `all` matching every filter, keeping
// the sorted order. An empty namespace (or the "all" sentinels) matches all.
//
// The three indexes are used as candidate sets: the scan starts from the most
// selective one, so a filtered query costs O(|smallest match set|) instead of
// O(total instances). Filtering the candidate set by the remaining conditions
// preserves the sorted order because every index list is built in `all` order.
func (s *workspaceSnapshot) matchOffsets(namespace, owner, oa string) []int32 {
	namespaceRestricted := !isAllNamespaceSentinel(namespace)
	candidates, ok := s.namespaceOffsets(namespace)
	if !ok {
		return nil // unknown namespace: no matches
	}
	if owner != "" {
		list, ok := s.byOwner[owner]
		if !ok {
			return nil
		}
		if candidates == nil || len(list) < len(candidates) {
			candidates = list
		}
	}
	if oa != "" {
		list, ok := s.byOA[oa]
		if !ok {
			return nil
		}
		if candidates == nil || len(list) < len(candidates) {
			candidates = list
		}
	}

	matches := func(it *workspaceItem) bool {
		if namespaceRestricted && it.Namespace != namespace {
			return false
		}
		if owner != "" && it.UserID != owner {
			return false
		}
		if oa != "" && it.OA != oa {
			return false
		}
		return true
	}

	if candidates == nil { // no filter at all: scan everything
		result := make([]int32, 0, len(s.all))
		for i := range s.all {
			if matches(&s.all[i]) {
				result = append(result, int32(i))
			}
		}
		return result
	}

	result := make([]int32, 0, len(candidates))
	for _, i := range candidates {
		if matches(&s.all[i]) {
			result = append(result, i)
		}
	}
	return result
}

// isAllNamespaceSentinel reports whether a namespace filter means "no restriction".
func isAllNamespaceSentinel(namespace string) bool {
	switch namespace {
	case "", "all", "*", "_all":
		return true
	default:
		return false
	}
}

// namespaceOffsets resolves a namespace filter to candidate offsets.
// Returns ok=false when the namespace is known to have no workspaces.
// A nil slice with ok=true means "no namespace restriction".
func (s *workspaceSnapshot) namespaceOffsets(namespace string) (offsets []int32, ok bool) {
	if isAllNamespaceSentinel(namespace) {
		return nil, true
	}
	list, exists := s.byNamespace[namespace]
	if !exists {
		return nil, false
	}
	return list, true
}

// toWorkspaceItem projects a Workspace CR into the response DTO. Only reads
// the shared informer object — never mutates or retains it.
func toWorkspaceItem(ws *aiv1alpha1.Workspace) workspaceItem {
	return workspaceItem{
		UserID:    ws.Spec.Owner,
		Name:      ws.Name,
		Namespace: ws.Namespace,
		OA:        getOAFromWorkspace(ws),
		Phase:     string(ws.Status.Phase),
		URL:       getWorkspaceURL(ws.Spec.Owner, ws.Name, ws.Status.Endpoint),
		Image:     ws.Spec.Runtime.Image,
		CPU:       ws.Spec.Runtime.CPU,
		Memory:    ws.Spec.Runtime.Memory,
		// Internal probe inputs (never serialized to clients).
		Endpoint:   ws.Status.Endpoint,
		HealthPath: ws.Spec.Runtime.HealthPath,
	}
}

// startWorkspaceCache builds the informer-backed cache: it registers the event
// handler before the initial LIST so no object is missed, starts the shared
// informer (paginated LIST + WATCH handled by the informer — the API server
// process itself never issues full unpaginated Lists) and returns once the
// initial LIST has been materialized. The informers keep running in the
// background until ctx is cancelled.
func startWorkspaceCache(ctx context.Context, cfg *rest.Config, scheme *runtime.Scheme) (*workspaceCache, error) {
	// Periodic resync regenerates Update events for every object, acting as a
	// self-healing safety net for the event-driven view.
	resync := 1 * time.Minute
	crCache, err := cache.New(cfg, cache.Options{
		Scheme:     scheme,
		SyncPeriod: &resync,
		// Only watch the Workspace type: bounds informer memory to what the
		// API server actually serves.
		ByObject: map[client.Object]cache.ByObject{
			&aiv1alpha1.Workspace{}: {},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create informer cache: %w", err)
	}

	// GetInformer before Start creates (but does not run) the informer, so the
	// event handler below is registered before the initial LIST begins.
	informer, err := crCache.GetInformer(ctx, &aiv1alpha1.Workspace{})
	if err != nil {
		return nil, fmt.Errorf("failed to get Workspace informer: %w", err)
	}

	wc := newWorkspaceCache()
	if _, err := informer.AddEventHandler(wc); err != nil {
		return nil, fmt.Errorf("failed to register Workspace event handler: %w", err)
	}

	// cache.Cache.Start blocks until ctx is cancelled — it runs the informers
	// for the lifetime of the process, so it must not be called synchronously
	// here. Wait for the initial LIST separately; select on both so a startup
	// failure surfaces as an error instead of hanging until shutdown.
	startErr := make(chan error, 1)
	go func() {
		if err := crCache.Start(ctx); err != nil && ctx.Err() == nil {
			startErr <- fmt.Errorf("informer cache stopped unexpectedly: %w", err)
		}
	}()
	synced := make(chan bool, 1)
	go func() { synced <- crCache.WaitForCacheSync(ctx) }()
	select {
	case err := <-startErr:
		return nil, err
	case ok := <-synced:
		if !ok {
			return nil, errors.New("informer cache failed to sync")
		}
	}

	// Deterministic initial snapshot; from here on rebuilds are event-driven.
	wc.rebuildNow()
	return wc, nil
}

// cacheRebuildDelay returns the event→rebuild debounce window.
func cacheRebuildDelay() time.Duration {
	if raw := os.Getenv("WORKSPACE_CACHE_REBUILD_MS"); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms >= 0 {
			return time.Duration(ms) * time.Millisecond
		}
		log.Printf("Invalid WORKSPACE_CACHE_REBUILD_MS %q, using default", raw)
	}
	return 500 * time.Millisecond
}
