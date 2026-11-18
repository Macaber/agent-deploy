package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"time"
	_ "time/tzdata"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

const (
	batchConfigMapName     = "api-server-batch-restart"
	batchConfigMapKey      = "status.json"
	defaultTimezone        = "Asia/Shanghai"
	tzEnvName              = "TZ"
	batchTickInterval      = 5 * time.Second
	batchStaleThreshold    = 90 * time.Second
	batchMaxItems          = 5000
	defaultBatchSize       = 20
	defaultWakeWindow      = "10m"
	defaultRunTimeout      = "5m"
	defaultTargetNamespace = "bocomwork"
)

const (
	batchStateWaiting   = "waiting"
	batchStateRunning   = "running"
	batchStateStopping  = "stopping"
	batchStateCompleted = "completed"
	batchStateStopped   = "stopped"

	itemStatePending   = "pending"
	itemStateSubmitted = "submitted"
	itemStateRunning   = "running"
	itemStateStopped   = "stopped"
	itemStateFailed    = "failed"
	itemStateSkipped   = "skipped"
)

var beijingLoc = func() *time.Location {
	loc, err := time.LoadLocation(defaultTimezone)
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

var batchNow = func() time.Time { return time.Now() }

type batchItem struct {
	UserID      string    `json:"userId"`
	Name        string    `json:"name"`
	Namespace   string    `json:"namespace"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submittedAt,omitempty"`
	RunningAt   time.Time `json:"runningAt,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type batchDocument struct {
	Owner         string           `json:"owner"`
	State         string           `json:"state"`
	Namespace     string           `json:"namespace"`
	Image         string           `json:"image"`
	BatchSize     int              `json:"batchSize"`
	WakeWindow    string           `json:"wakeWindow"`
	RunTimeout    string           `json:"runTimeout"`
	StartTime     time.Time        `json:"startTime"`
	Deadline      time.Time        `json:"deadline"`
	StopRequested bool             `json:"stopRequested"`
	Total         int              `json:"total"`
	StartedAt     time.Time        `json:"startedAt"`
	UpdatedAt     time.Time        `json:"updatedAt"`
	Items         []batchItem      `json:"items"`
	Config        workspaceRequest `json:"config"`
}

type batchRestartRequest struct {
	Namespace  string           `json:"namespace,omitempty"`
	BatchSize  int              `json:"batchSize,omitempty"`
	WakeWindow string           `json:"wakeWindow,omitempty"`
	RunTimeout string           `json:"runTimeout,omitempty"`
	StartTime  string           `json:"startTime,omitempty"`
	Deadline   string           `json:"deadline"`
	Config     workspaceRequest `json:"config"`
}

func (d *batchDocument) isActive() bool {
	return d.State == batchStateWaiting || d.State == batchStateRunning || d.State == batchStateStopping
}

func (d *batchDocument) isStale(now time.Time) bool {
	return now.Sub(d.UpdatedAt) > batchStaleThreshold
}

func mergeEnv(existing, configured []aiv1alpha1.EnvVar) []aiv1alpha1.EnvVar {
	merged := make([]aiv1alpha1.EnvVar, 0, len(existing)+len(configured)+1)
	seen := make(map[string]bool, len(existing)+len(configured)+1)
	tzConfigured := false
	for _, env := range configured {
		if env.Name == "" {
			continue
		}
		if env.Name == tzEnvName {
			tzConfigured = true
		}
		merged = append(merged, env)
		seen[env.Name] = true
	}
	for _, env := range existing {
		if env.Name == "" || seen[env.Name] || env.Name == tzEnvName {
			continue
		}
		merged = append(merged, env)
		seen[env.Name] = true
	}
	if !tzConfigured {
		merged = append(merged, aiv1alpha1.EnvVar{Name: tzEnvName, Value: defaultTimezone})
	}
	return merged
}

func parseBeijingTime(value string) (time.Time, error) {
	layouts := []string{"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02T15:04:05", time.RFC3339}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, value, beijingLoc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid Beijing time %q, expected like 2026-11-23T08:00", value)
}

func batchControlNamespace() string {
	if ns := os.Getenv("BATCH_CONTROL_NAMESPACE"); ns != "" {
		return ns
	}
	return "default"
}

func readBatchDoc(ctx context.Context, c client.Client) (*batchDocument, string, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Namespace: batchControlNamespace(), Name: batchConfigMapName}, cm)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	raw, ok := cm.Data[batchConfigMapKey]
	if !ok {
		return nil, cm.ResourceVersion, nil
	}
	doc := &batchDocument{}
	if err := json.Unmarshal([]byte(raw), doc); err != nil {
		return nil, "", fmt.Errorf("corrupted batch control configmap: %w", err)
	}
	return doc, cm.ResourceVersion, nil
}

func saveBatchDoc(ctx context.Context, c client.Client, doc *batchDocument, resourceVersion string) error {
	payload, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	ns := batchControlNamespace()
	if resourceVersion == "" {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: batchConfigMapName, Namespace: ns},
			Data:       map[string]string{batchConfigMapKey: string(payload)},
		}
		if err := c.Create(ctx, cm); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return errBatchConflict
			}
			return err
		}
		return nil
	}
	update := func() error {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: batchConfigMapName}, cm); err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[batchConfigMapKey] = string(payload)
		return c.Update(ctx, cm)
	}
	return retry.OnError(retry.DefaultRetry, func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsNotFound(err)
	}, update)
}

var errBatchBusy = fmt.Errorf("batch restart task already active")
var errBatchConflict = fmt.Errorf("batch control configmap conflict")

func claimBatchTask(ctx context.Context, c client.Client, doc *batchDocument) error {
	for attempt := 0; attempt < 4; attempt++ {
		existing, rv, err := readBatchDoc(ctx, c)
		if err != nil {
			return err
		}
		if existing != nil && existing.isActive() && !existing.isStale(batchNow()) {
			return errBatchBusy
		}
		if err := saveBatchDoc(ctx, c, doc, rv); err != nil {
			if errors.Is(err, errBatchConflict) || apierrors.IsConflict(err) {
				continue
			}
			return err
		}
		return nil
	}
	return errBatchBusy
}

func wcPhase(wc *workspaceCache, item *batchItem) string {
	if cached, ok := wc.get(item.Namespace, item.Name); ok {
		return cached.Phase
	}
	return ""
}

func submitBatchItem(ctx context.Context, c client.Client, doc *batchDocument, item *batchItem) error {
	ws := &aiv1alpha1.Workspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: item.Namespace, Name: item.Name}, ws); err != nil {
		return err
	}
	req := doc.Config
	req.UserID = item.UserID
	req.Namespace = item.Namespace
	req.Env = mergeEnv(ws.Spec.Runtime.Env, req.Env)
	return updateExistingWorkspace(ctx, c, ws, &req, item.Name, item.Namespace)
}

func stopWorkspaceByName(ctx context.Context, c client.Client, namespace, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ws := &aiv1alpha1.Workspace{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, ws); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if ws.Spec.Stopped {
			return nil
		}
		ws.Spec.Stopped = true
		return c.Update(ctx, ws)
	})
}

func batchTick(ctx context.Context, c client.Client, wc *workspaceCache, owner string) (bool, error) {
	doc, rv, err := readBatchDoc(ctx, c)
	if err != nil {
		return false, err
	}
	if doc == nil || doc.Owner != owner || !doc.isActive() {
		return true, nil
	}

	now := batchNow()
	wakeWindow, err := time.ParseDuration(doc.WakeWindow)
	if err != nil {
		wakeWindow, _ = time.ParseDuration(defaultWakeWindow)
	}
	runTimeout, err := time.ParseDuration(doc.RunTimeout)
	if err != nil {
		runTimeout, _ = time.ParseDuration(defaultRunTimeout)
	}

	halting := doc.StopRequested || !now.Before(doc.Deadline)
	changed := false
	if halting {
		if doc.State == batchStateRunning || doc.State == batchStateWaiting {
			doc.State = batchStateStopping
			for i := range doc.Items {
				if doc.Items[i].State == itemStatePending {
					doc.Items[i].State = itemStateSkipped
				}
			}
			changed = true
		}
	} else if doc.State == batchStateWaiting && !now.Before(doc.StartTime) {
		doc.State = batchStateRunning
		changed = true
	}
	waiting := doc.State == batchStateWaiting

	inFlight := 0
	for i := range doc.Items {
		item := &doc.Items[i]
		switch item.State {
		case itemStateSubmitted:
			switch {
			case wcPhase(wc, item) == string(aiv1alpha1.WorkspaceRunning):
				item.State = itemStateRunning
				item.RunningAt = now
				changed = true
			case now.Sub(item.SubmittedAt) > runTimeout:
				if stopErr := stopWorkspaceByName(ctx, c, item.Namespace, item.Name); stopErr != nil {
					log.Printf("Batch restart: failed to stop timed-out Workspace %s/%s: %v", item.Namespace, item.Name, stopErr)
				}
				item.State = itemStateFailed
				item.Error = "start timed out"
				changed = true
			}
		case itemStateRunning:
			if !now.Before(item.RunningAt.Add(wakeWindow)) {
				if stopErr := stopWorkspaceByName(ctx, c, item.Namespace, item.Name); stopErr != nil {
					log.Printf("Batch restart: failed to stop Workspace %s/%s: %v", item.Namespace, item.Name, stopErr)
				} else {
					item.State = itemStateStopped
					changed = true
				}
			}
		}
		if item.State == itemStateSubmitted || item.State == itemStateRunning {
			inFlight++
		}
	}

	if !halting && !waiting {
		slots := doc.BatchSize - inFlight
		for slots > 0 {
			var next *batchItem
			for i := range doc.Items {
				if doc.Items[i].State == itemStatePending {
					next = &doc.Items[i]
					break
				}
			}
			if next == nil {
				break
			}
			if err := submitBatchItem(ctx, c, doc, next); err != nil {
				next.State = itemStateFailed
				next.Error = err.Error()
				log.Printf("Batch restart: failed to submit Workspace %s/%s: %v", next.Namespace, next.Name, err)
			} else {
				next.State = itemStateSubmitted
				next.SubmittedAt = now
			}
			changed = true
			slots--
		}
	}

	if allItemsTerminal(doc) {
		if halting {
			doc.State = batchStateStopped
		} else {
			doc.State = batchStateCompleted
		}
		changed = true
	}

	if changed || now.Sub(doc.UpdatedAt) > 30*time.Second {
		doc.UpdatedAt = now
		if err := saveBatchDoc(ctx, c, doc, rv); err != nil {
			if errors.Is(err, errBatchConflict) || apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
	}
	return false, nil
}

func allItemsTerminal(doc *batchDocument) bool {
	for i := range doc.Items {
		switch doc.Items[i].State {
		case itemStatePending, itemStateSubmitted, itemStateRunning:
			return false
		}
	}
	return true
}

func runBatchLoop(ctx context.Context, c client.Client, wc *workspaceCache, owner string) {
	ticker := time.NewTicker(batchTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			done, err := batchTick(ctx, c, wc, owner)
			if err != nil {
				log.Printf("Batch restart tick failed: %v", err)
			}
			if done {
				return
			}
		}
	}
}

func enumerateBatchTargets(wc *workspaceCache, namespace string) []batchItem {
	var items []batchItem
	for _, it := range wc.snapshot().all {
		if it.Namespace != namespace {
			continue
		}
		if it.Phase != string(aiv1alpha1.WorkspaceSleeping) && it.Phase != string(aiv1alpha1.WorkspaceStopped) {
			continue
		}
		items = append(items, batchItem{
			UserID:    it.UserID,
			Name:      it.Name,
			Namespace: it.Namespace,
			State:     itemStatePending,
		})
		if len(items) >= batchMaxItems {
			break
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items
}

type batchFailedSample struct {
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

type batchStatusResponse struct {
	Active        bool                `json:"active"`
	State         string              `json:"state"`
	Orphaned      bool                `json:"orphaned"`
	ServerTime    string              `json:"serverTime"`
	Timezone      string              `json:"timezone"`
	Namespace     string              `json:"namespace,omitempty"`
	Image         string              `json:"image,omitempty"`
	Total         int                 `json:"total"`
	Pending       int                 `json:"pending"`
	Submitted     int                 `json:"submitted"`
	Running       int                 `json:"running"`
	Stopped       int                 `json:"stopped"`
	Failed        int                 `json:"failed"`
	Skipped       int                 `json:"skipped"`
	InFlight      int                 `json:"inFlight"`
	BatchSize     int                 `json:"batchSize,omitempty"`
	WakeWindow    string              `json:"wakeWindow,omitempty"`
	StartTime     string              `json:"startTime,omitempty"`
	Deadline      string              `json:"deadline,omitempty"`
	StartedAt     string              `json:"startedAt,omitempty"`
	UpdatedAt     string              `json:"updatedAt,omitempty"`
	FailedSamples []batchFailedSample `json:"failedSamples,omitempty"`
}

func statusFromDoc(doc *batchDocument) batchStatusResponse {
	now := batchNow()
	resp := batchStatusResponse{
		State:      "none",
		ServerTime: now.In(beijingLoc).Format("2006-01-02 15:04:05"),
		Timezone:   fmt.Sprintf("%s (UTC+8)", defaultTimezone),
	}
	if doc == nil {
		return resp
	}
	resp.State = doc.State
	resp.Namespace = doc.Namespace
	resp.Image = doc.Image
	resp.Total = doc.Total
	resp.BatchSize = doc.BatchSize
	resp.WakeWindow = doc.WakeWindow
	resp.Active = doc.isActive() && !doc.isStale(now)
	resp.Orphaned = doc.isActive() && doc.isStale(now)
	resp.Deadline = doc.Deadline.In(beijingLoc).Format("2006-01-02 15:04")
	resp.StartTime = doc.StartTime.In(beijingLoc).Format("2006-01-02 15:04")
	resp.StartedAt = doc.StartedAt.In(beijingLoc).Format("2006-01-02 15:04:05")
	resp.UpdatedAt = doc.UpdatedAt.In(beijingLoc).Format("2006-01-02 15:04:05")
	for i := range doc.Items {
		item := &doc.Items[i]
		switch item.State {
		case itemStatePending:
			resp.Pending++
		case itemStateSubmitted:
			resp.Submitted++
		case itemStateRunning:
			resp.Running++
		case itemStateStopped:
			resp.Stopped++
		case itemStateFailed:
			resp.Failed++
			if len(resp.FailedSamples) < 10 {
				resp.FailedSamples = append(resp.FailedSamples, batchFailedSample{Name: item.Name, Error: item.Error})
			}
		case itemStateSkipped:
			resp.Skipped++
		}
	}
	resp.InFlight = resp.Submitted + resp.Running
	return resp
}

func writeBatchStatus(w http.ResponseWriter, resp batchStatusResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

func batchRestartHandler(ctx context.Context, c client.Client, wc *workspaceCache) http.HandlerFunc {
	return requireAuth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			doc, _, err := readBatchDoc(r.Context(), c)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeBatchStatus(w, statusFromDoc(doc))
		case http.MethodPost:
			r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
			var req batchRestartRequest
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var trailing any
			if err := decoder.Decode(&trailing); err != io.EOF {
				http.Error(w, "Expected one JSON document", http.StatusBadRequest)
				return
			}
			if req.Config.Image == "" {
				http.Error(w, "Missing required parameter: config.image", http.StatusBadRequest)
				return
			}
			namespace := req.Namespace
			if namespace == "" {
				namespace = defaultTargetNamespace
			}
			wakeWindow := req.WakeWindow
			if wakeWindow == "" {
				wakeWindow = defaultWakeWindow
			}
			if _, err := time.ParseDuration(wakeWindow); err != nil {
				http.Error(w, fmt.Sprintf("Invalid wakeWindow %q: %v", req.WakeWindow, err), http.StatusBadRequest)
				return
			}
			runTimeout := req.RunTimeout
			if runTimeout == "" {
				runTimeout = defaultRunTimeout
			}
			if _, err := time.ParseDuration(runTimeout); err != nil {
				http.Error(w, fmt.Sprintf("Invalid runTimeout %q: %v", req.RunTimeout, err), http.StatusBadRequest)
				return
			}
			batchSize := req.BatchSize
			if batchSize <= 0 {
				batchSize = defaultBatchSize
			}
			if batchSize > 200 {
				batchSize = 200
			}
			deadline, err := parseBeijingTime(req.Deadline)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			now := batchNow()
			if !deadline.After(now.Add(time.Minute)) {
				http.Error(w, "deadline must be at least 1 minute in the future (Beijing time)", http.StatusBadRequest)
				return
			}
			startTime := now
			if req.StartTime != "" {
				startTime, err = parseBeijingTime(req.StartTime)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if !startTime.After(now) {
					http.Error(w, "startTime must be in the future (Beijing time); omit it to start immediately", http.StatusBadRequest)
					return
				}
				if !startTime.Before(deadline) {
					http.Error(w, "startTime must be earlier than deadline", http.StatusBadRequest)
					return
				}
			}
			if !wc.isReady() {
				http.Error(w, "workspace informer cache not synced yet", http.StatusServiceUnavailable)
				return
			}
			targets := enumerateBatchTargets(wc, namespace)
			if len(targets) == 0 {
				http.Error(w, fmt.Sprintf("no Sleeping/Stopped workspaces found in namespace %q", namespace), http.StatusUnprocessableEntity)
				return
			}
			owner, _ := os.Hostname()
			initialState := batchStateRunning
			if startTime.After(now) {
				initialState = batchStateWaiting
			}
			doc := &batchDocument{
				Owner:      owner,
				State:      initialState,
				Namespace:  namespace,
				Image:      req.Config.Image,
				BatchSize:  batchSize,
				WakeWindow: wakeWindow,
				RunTimeout: runTimeout,
				StartTime:  startTime,
				Deadline:   deadline,
				Total:      len(targets),
				StartedAt:  now,
				UpdatedAt:  now,
				Items:      targets,
				Config:     req.Config,
			}
			if err := claimBatchTask(ctx, c, doc); err != nil {
				if err == errBatchBusy {
					http.Error(w, "a batch restart task is already active on another replica", http.StatusConflict)
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			go runBatchLoop(ctx, c, wc, owner)
			log.Printf("Batch restart started: %d targets in namespace %s, batchSize=%d, wakeWindow=%s, deadline=%s",
				len(targets), namespace, batchSize, wakeWindow, deadline.In(beijingLoc).Format("2006-01-02 15:04"))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(statusFromDoc(doc))
		case http.MethodDelete:
			doc, rv, err := readBatchDoc(r.Context(), c)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if doc == nil {
				http.Error(w, "no batch restart task exists", http.StatusNotFound)
				return
			}
			if doc.isActive() && !doc.StopRequested {
				doc.StopRequested = true
				if doc.State == batchStateRunning || doc.State == batchStateWaiting {
					doc.State = batchStateStopping
					for i := range doc.Items {
						if doc.Items[i].State == itemStatePending {
							doc.Items[i].State = itemStateSkipped
						}
					}
				}
				doc.UpdatedAt = batchNow()
				if err := saveBatchDoc(r.Context(), c, doc, rv); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				log.Printf("Batch restart stop requested by operator")
			}
			writeBatchStatus(w, statusFromDoc(doc))
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
