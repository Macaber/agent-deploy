package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

func batchTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := aiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func batchTestWorkspace(name, namespace, owner, phase string) *aiv1alpha1.Workspace {
	return &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: aiv1alpha1.WorkspaceSpec{
			Owner:   owner,
			Stopped: phase == string(aiv1alpha1.WorkspaceStopped),
			Runtime: aiv1alpha1.RuntimeSpec{
				Image: "bocomwork:v0.0.1",
				Env: []aiv1alpha1.EnvVar{
					{Name: "OA", Value: "oa_" + owner},
					{Name: "GUWP_TOKEN", Value: "tok_" + owner},
				},
			},
		},
		Status: aiv1alpha1.WorkspaceStatus{Phase: aiv1alpha1.WorkspacePhase(phase)},
	}
}

func batchTestCache(t *testing.T, workspaces ...*aiv1alpha1.Workspace) *workspaceCache {
	t.Helper()
	wc := newWorkspaceCache()
	for _, ws := range workspaces {
		wc.upsert(ws)
	}
	wc.rebuildNow()
	return wc
}

func TestMergeEnv(t *testing.T) {
	existing := []aiv1alpha1.EnvVar{
		{Name: "OA", Value: "alice"},
		{Name: "GUWP_TOKEN", Value: "tok"},
		{Name: "FOO", Value: "old"},
		{Name: "TZ", Value: "UTC"},
	}
	configured := []aiv1alpha1.EnvVar{
		{Name: "OPENCODE_CONFIG", Value: "/root/config/bocomcode.json"},
		{Name: "FOO", Value: "new"},
	}
	merged := mergeEnv(existing, configured)

	byName := map[string]string{}
	for _, env := range merged {
		byName[env.Name] = env.Value
	}
	if byName["FOO"] != "new" {
		t.Errorf("configured FOO should override existing, got %q", byName["FOO"])
	}
	if byName["OA"] != "alice" || byName["GUWP_TOKEN"] != "tok" {
		t.Errorf("personal env vars must be preserved, got %#v", byName)
	}
	if byName["TZ"] != defaultTimezone {
		t.Errorf("TZ must be forced to %s even when existing sets UTC, got %q", defaultTimezone, byName["TZ"])
	}
	if len(merged) != 5 {
		t.Errorf("expected 5 merged vars, got %d: %#v", len(merged), merged)
	}

	configuredTZ := []aiv1alpha1.EnvVar{{Name: "TZ", Value: "America/New_York"}}
	if got := mergeEnv(existing, configuredTZ); func() string {
		for _, env := range got {
			if env.Name == "TZ" {
				return env.Value
			}
		}
		return ""
	}() != "America/New_York" {
		t.Errorf("page-configured TZ must win, got %#v", got)
	}
}

func TestParseBeijingTime(t *testing.T) {
	got, err := parseBeijingTime("2026-11-23T08:00")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Beijing 08:00 must be 00:00 UTC, got %v", got)
	}

	got, err = parseBeijingTime("2026-11-23T08:00:00+08:00")
	if err != nil || !got.Equal(want) {
		t.Errorf("RFC3339 with +08:00 must parse to same instant, got %v err %v", got, err)
	}

	if _, err := parseBeijingTime("next monday"); err == nil {
		t.Error("invalid time must be rejected")
	}
}

func TestBatchTickWaitingDelaysStart(t *testing.T) {
	base := time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)
	ws := batchTestWorkspace("ws-a", "bocomwork", "a", string(aiv1alpha1.WorkspaceSleeping))
	scheme := batchTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&aiv1alpha1.Workspace{}).
		WithObjects(ws).Build()
	wc := batchTestCache(t, ws)
	ctx := context.Background()

	setBatchClock(t, base)
	targets := enumerateBatchTargets(wc, "bocomwork")
	doc := newBatchTestDoc(t, targets, 20, base)
	doc.State = batchStateWaiting
	doc.StartTime = base.Add(10 * time.Minute)
	if err := saveBatchDoc(ctx, fakeClient, doc, ""); err != nil {
		t.Fatal(err)
	}

	if _, err := batchTick(ctx, fakeClient, wc, "test-owner"); err != nil {
		t.Fatal(err)
	}
	saved, _, err := readBatchDoc(ctx, fakeClient)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != batchStateWaiting || saved.Items[0].State != itemStatePending {
		t.Errorf("before startTime nothing must run or submit, got state=%q item=%q", saved.State, saved.Items[0].State)
	}

	setBatchClock(t, base.Add(11*time.Minute))
	if _, err := batchTick(ctx, fakeClient, wc, "test-owner"); err != nil {
		t.Fatal(err)
	}
	saved, _, err = readBatchDoc(ctx, fakeClient)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != batchStateRunning || saved.Items[0].State != itemStateSubmitted {
		t.Errorf("after startTime task must run and submit, got state=%q item=%q", saved.State, saved.Items[0].State)
	}
}

func TestEnumerateBatchTargets(t *testing.T) {
	wc := batchTestCache(t,
		batchTestWorkspace("ws-a", "bocomwork", "a", string(aiv1alpha1.WorkspaceSleeping)),
		batchTestWorkspace("ws-b", "bocomwork", "b", string(aiv1alpha1.WorkspaceStopped)),
		batchTestWorkspace("ws-c", "bocomwork", "c", string(aiv1alpha1.WorkspaceRunning)),
		batchTestWorkspace("ws-d", "default", "d", string(aiv1alpha1.WorkspaceSleeping)),
	)
	targets := enumerateBatchTargets(wc, "bocomwork")
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets (Sleeping+Stopped in bocomwork), got %d", len(targets))
	}
	if targets[0].Name != "ws-a" || targets[1].Name != "ws-b" {
		t.Errorf("targets must be sorted by name, got %#v", targets)
	}
	if targets[0].State != itemStatePending || targets[0].UserID != "a" {
		t.Errorf("unexpected target fields: %#v", targets[0])
	}
}

func newBatchTestDoc(t *testing.T, targets []batchItem, batchSize int, now time.Time) *batchDocument {
	t.Helper()
	return &batchDocument{
		Owner:      "test-owner",
		State:      batchStateRunning,
		Namespace:  "bocomwork",
		Image:      "bocomwork:v1.0.3",
		BatchSize:  batchSize,
		WakeWindow: "10m",
		RunTimeout: "5m",
		Deadline:   now.Add(24 * time.Hour),
		Total:      len(targets),
		StartedAt:  now,
		UpdatedAt:  now,
		Items:      targets,
		Config: workspaceRequest{
			Image:   "bocomwork:v1.0.3",
			Command: []string{"bocomwork-entrypoint"},
		},
	}
}

func setBatchClock(t *testing.T, now time.Time) {
	t.Helper()
	previous := batchNow
	batchNow = func() time.Time { return now }
	t.Cleanup(func() { batchNow = previous })
}

func TestBatchTickSubmitRunStop(t *testing.T) {
	base := time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)
	wsA := batchTestWorkspace("ws-a", "bocomwork", "a", string(aiv1alpha1.WorkspaceSleeping))
	wsB := batchTestWorkspace("ws-b", "bocomwork", "b", string(aiv1alpha1.WorkspaceStopped))
	scheme := batchTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&aiv1alpha1.Workspace{}).
		WithObjects(wsA, wsB).Build()
	wc := batchTestCache(t, wsA, wsB)
	ctx := context.Background()

	setBatchClock(t, base)
	targets := enumerateBatchTargets(wc, "bocomwork")
	doc := newBatchTestDoc(t, targets, 20, base)
	if err := saveBatchDoc(ctx, fakeClient, doc, ""); err != nil {
		t.Fatalf("saveBatchDoc failed: %v", err)
	}

	done, err := batchTick(ctx, fakeClient, wc, "test-owner")
	if err != nil || done {
		t.Fatalf("tick1 failed: done=%v err=%v", done, err)
	}
	for _, name := range []string{"ws-a", "ws-b"} {
		updated := &aiv1alpha1.Workspace{}
		if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: "bocomwork", Name: name}, updated); err != nil {
			t.Fatal(err)
		}
		if updated.Spec.Stopped {
			t.Errorf("%s must be un-stopped on submit", name)
		}
		if updated.Spec.Runtime.Image != "bocomwork:v1.0.3" {
			t.Errorf("%s image must be replaced, got %q", name, updated.Spec.Runtime.Image)
		}
		envByName := map[string]string{}
		for _, env := range updated.Spec.Runtime.Env {
			envByName[env.Name] = env.Value
		}
		owner := strings.TrimPrefix(name, "ws-")
		if envByName["OA"] != "oa_"+owner || envByName["GUWP_TOKEN"] != "tok_"+owner {
			t.Errorf("%s personal env must survive submit, got %#v", name, envByName)
		}
		if envByName["TZ"] != defaultTimezone {
			t.Errorf("%s TZ must be injected, got %#v", name, envByName)
		}
		if updated.Status.LastActiveTime == nil {
			t.Errorf("%s lastActiveTime must be reset to wake the workspace", name)
		}
	}

	runningA := wsA.DeepCopy()
	runningA.Status.Phase = aiv1alpha1.WorkspaceRunning
	runningB := wsB.DeepCopy()
	runningB.Status.Phase = aiv1alpha1.WorkspaceRunning
	wc.upsert(runningA)
	wc.upsert(runningB)
	wc.rebuildNow()

	setBatchClock(t, base.Add(10*time.Second))
	if done, err := batchTick(ctx, fakeClient, wc, "test-owner"); err != nil || done {
		t.Fatalf("tick2 failed: done=%v err=%v", done, err)
	}

	setBatchClock(t, base.Add(11*time.Minute))
	if done, err := batchTick(ctx, fakeClient, wc, "test-owner"); err != nil || done {
		t.Fatalf("tick3 failed: done=%v err=%v", done, err)
	}

	finalDoc, _, err := readBatchDoc(ctx, fakeClient)
	if err != nil {
		t.Fatal(err)
	}
	if finalDoc.State != batchStateCompleted {
		t.Errorf("expected completed state, got %q", finalDoc.State)
	}
	for i, item := range finalDoc.Items {
		if item.State != itemStateStopped {
			t.Errorf("item %d must be stopped, got %q", i, item.State)
		}
		updated := &aiv1alpha1.Workspace{}
		if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: "bocomwork", Name: item.Name}, updated); err != nil {
			t.Fatal(err)
		}
		if !updated.Spec.Stopped {
			t.Errorf("%s must be stopped after wake window", item.Name)
		}
	}
}

func TestBatchTickBatchSizeWindow(t *testing.T) {
	base := time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)
	var windows []*aiv1alpha1.Workspace
	for _, name := range []string{"ws-1", "ws-2", "ws-3"} {
		windows = append(windows, batchTestWorkspace(name, "bocomwork", strings.TrimPrefix(name, "ws-"), string(aiv1alpha1.WorkspaceSleeping)))
	}
	scheme := batchTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&aiv1alpha1.Workspace{}).
		WithObjects(windows[0], windows[1], windows[2]).Build()
	wc := batchTestCache(t, windows...)
	ctx := context.Background()

	setBatchClock(t, base)
	targets := enumerateBatchTargets(wc, "bocomwork")
	doc := newBatchTestDoc(t, targets, 2, base)
	if err := saveBatchDoc(ctx, fakeClient, doc, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := batchTick(ctx, fakeClient, wc, "test-owner"); err != nil {
		t.Fatal(err)
	}

	saved, _, err := readBatchDoc(ctx, fakeClient)
	if err != nil {
		t.Fatal(err)
	}
	submitted, pending := 0, 0
	for _, item := range saved.Items {
		switch item.State {
		case itemStateSubmitted:
			submitted++
		case itemStatePending:
			pending++
		}
	}
	if submitted != 2 || pending != 1 {
		t.Errorf("batchSize=2 must submit exactly 2 (got %d) and leave 1 pending (got %d)", submitted, pending)
	}
}

func TestBatchTickRunTimeoutFailsAndStops(t *testing.T) {
	base := time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)
	ws := batchTestWorkspace("ws-slow", "bocomwork", "slow", string(aiv1alpha1.WorkspaceSleeping))
	scheme := batchTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&aiv1alpha1.Workspace{}).
		WithObjects(ws).Build()
	wc := batchTestCache(t, ws)
	ctx := context.Background()

	setBatchClock(t, base)
	targets := enumerateBatchTargets(wc, "bocomwork")
	doc := newBatchTestDoc(t, targets, 20, base)
	doc.RunTimeout = "5m"
	if err := saveBatchDoc(ctx, fakeClient, doc, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := batchTick(ctx, fakeClient, wc, "test-owner"); err != nil {
		t.Fatal(err)
	}

	setBatchClock(t, base.Add(6*time.Minute))
	if _, err := batchTick(ctx, fakeClient, wc, "test-owner"); err != nil {
		t.Fatal(err)
	}
	saved, _, err := readBatchDoc(ctx, fakeClient)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != batchStateCompleted {
		t.Errorf("expected completed, got %q", saved.State)
	}
	if saved.Items[0].State != itemStateFailed || saved.Items[0].Error == "" {
		t.Errorf("timed-out item must be failed with error, got %#v", saved.Items[0])
	}
	updated := &aiv1alpha1.Workspace{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Namespace: "bocomwork", Name: "ws-slow"}, updated); err != nil {
		t.Fatal(err)
	}
	if !updated.Spec.Stopped {
		t.Error("timed-out workspace must be stopped")
	}
}

func TestBatchTickStopRequestedSkipsPending(t *testing.T) {
	base := time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)
	ws := batchTestWorkspace("ws-x", "bocomwork", "x", string(aiv1alpha1.WorkspaceSleeping))
	scheme := batchTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&aiv1alpha1.Workspace{}).
		WithObjects(ws).Build()
	wc := batchTestCache(t, ws)
	ctx := context.Background()

	setBatchClock(t, base)
	targets := enumerateBatchTargets(wc, "bocomwork")
	targets = append(targets, batchItem{UserID: "ghost", Name: "ws-ghost", Namespace: "bocomwork", State: itemStatePending})
	doc := newBatchTestDoc(t, targets, 1, base)
	if err := saveBatchDoc(ctx, fakeClient, doc, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := batchTick(ctx, fakeClient, wc, "test-owner"); err != nil {
		t.Fatal(err)
	}

	setBatchClock(t, base.Add(5*time.Second))
	saved, rv, err := readBatchDoc(ctx, fakeClient)
	if err != nil {
		t.Fatal(err)
	}
	saved.StopRequested = true
	if err := saveBatchDoc(ctx, fakeClient, saved, rv); err != nil {
		t.Fatal(err)
	}

	if _, err := batchTick(ctx, fakeClient, wc, "test-owner"); err != nil {
		t.Fatal(err)
	}
	saved, _, err = readBatchDoc(ctx, fakeClient)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != batchStateStopping {
		t.Errorf("expected stopping after stop request, got %q", saved.State)
	}
	ghostState := ""
	for _, item := range saved.Items {
		if item.Name == "ws-ghost" {
			ghostState = item.State
		}
	}
	if ghostState != itemStateSkipped {
		t.Errorf("pending item must be skipped on stop, got %q", ghostState)
	}

	running := ws.DeepCopy()
	running.Status.Phase = aiv1alpha1.WorkspaceRunning
	wc.upsert(running)
	wc.rebuildNow()
	setBatchClock(t, base.Add(11*time.Minute))
	if _, err := batchTick(ctx, fakeClient, wc, "test-owner"); err != nil {
		t.Fatal(err)
	}
	setBatchClock(t, base.Add(22*time.Minute))
	if _, err := batchTick(ctx, fakeClient, wc, "test-owner"); err != nil {
		t.Fatal(err)
	}
	saved, _, err = readBatchDoc(ctx, fakeClient)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != batchStateStopped {
		t.Errorf("halted task must end in stopped, got %q", saved.State)
	}
}

func TestClaimBatchTask(t *testing.T) {
	base := time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)
	scheme := batchTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	setBatchClock(t, base)
	doc := newBatchTestDoc(t, []batchItem{{UserID: "a", Name: "ws-a", Namespace: "bocomwork", State: itemStatePending}}, 20, base)
	if err := claimBatchTask(ctx, fakeClient, doc); err != nil {
		t.Fatalf("initial claim failed: %v", err)
	}

	rival := newBatchTestDoc(t, doc.Items, 20, base)
	rival.Owner = "rival"
	if err := claimBatchTask(ctx, fakeClient, rival); err != errBatchBusy {
		t.Fatalf("fresh active task must reject rival claim, got %v", err)
	}

	setBatchClock(t, base.Add(5*time.Minute))
	if err := claimBatchTask(ctx, fakeClient, rival); err != nil {
		t.Fatalf("stale active task must be adoptable, got %v", err)
	}
	saved, _, err := readBatchDoc(ctx, fakeClient)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Owner != "rival" {
		t.Errorf("adoption must transfer ownership, got %q", saved.Owner)
	}
}

func TestBatchRestartHandlerLifecycle(t *testing.T) {
	previousEnabled := authEnabled
	authEnabled = false
	t.Cleanup(func() { authEnabled = previousEnabled })

	base := time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)
	setBatchClock(t, base)

	ws := batchTestWorkspace("ws-a", "bocomwork", "a", string(aiv1alpha1.WorkspaceSleeping))
	scheme := batchTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&aiv1alpha1.Workspace{}).
		WithObjects(ws).Build()
	wc := batchTestCache(t, ws)
	handler := batchRestartHandler(context.Background(), fakeClient, wc)

	body := `{
		"namespace": "bocomwork",
		"batchSize": 20,
		"wakeWindow": "10m",
		"deadline": "2026-11-23T08:00",
		"config": {"image": "bocomwork:v1.0.3", "command": ["bocomwork-entrypoint"], "env": [{"name": "AIONUI_HOST", "value": "0.0.0.0"}]}
	}`

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/api/workspaces/batch-restart", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST start expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var started batchStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if !started.Active || started.Total != 1 || started.State != batchStateRunning {
		t.Errorf("unexpected start status: %#v", started)
	}
	if started.ServerTime != base.In(beijingLoc).Format("2006-01-02 15:04:05") {
		t.Errorf("serverTime must be rendered in Beijing time, got %q", started.ServerTime)
	}

	rec = httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/api/workspaces/batch-restart", strings.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second start expected 409, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodDelete, "/api/workspaces/batch-restart", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE expected 200, got %d", rec.Code)
	}
	var stopping batchStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &stopping); err != nil {
		t.Fatal(err)
	}
	if stopping.State != batchStateStopping {
		t.Errorf("expected stopping state, got %#v", stopping)
	}

	rec = httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/api/workspaces/batch-restart", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/api/workspaces/batch-restart", strings.NewReader(`{"config": {"image": ""}, "deadline": "x"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid body expected 400, got %d", rec.Code)
	}

	futureBody := `{"deadline": "2026-11-23T08:00", "startTime": "2026-11-19T08:00", "config": {"image": "x"}}`
	rec = httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/api/workspaces/batch-restart", strings.NewReader(futureBody)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("startTime in the past expected 400, got %d", rec.Code)
	}
	futureBody = `{"deadline": "2026-11-23T08:00", "startTime": "2026-11-23T09:00", "config": {"image": "x"}}`
	rec = httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/api/workspaces/batch-restart", strings.NewReader(futureBody)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("startTime after deadline expected 400, got %d", rec.Code)
	}
}
