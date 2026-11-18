package main

// main.go is the HTTP entry point for the migrate-server DaemonSet.
//
// Deployment: one Pod per node. Each Pod mounts ITS OWN node's
// /data/local-path-storage hostPath, so it can only migrate/clean the
// Workspaces that physically live on that node.
//
// Multi-node UI (method 2): a single page lists every node. The page is served
// by any Pod, but "which node" a request targets is carried in the URL prefix
// /node/<name>/... which nginx routes to that node's Pod (upstream per node,
// externalTrafficPolicy: Local). Every Pod refuses to act for any node other
// than its own NODE_NAME, so a misrouted request can never touch another node's
// local disk.

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

//go:embed portal.html
var portalFS embed.FS

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	ctx := ctrl.SetupSignalHandler()

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		nodeName = os.Getenv("HOSTNAME")
	}

	cfg := MigrateConfig{
		Root:        envOr("LP_ROOT", DefaultRoot),
		OSSBucket:   envOr("OSS_BUCKET", DefaultOSSBucket),
		OSSPrefix:   envOr("OSS_PREFIX", DefaultOSSPrefix),
		Dirs:        splitList(envOr("OSS_MIGRATE_DIRS", DefaultDirs)),
		Excludes:    ResolveExcludes(os.Getenv("OSS_MIGRATE_EXCLUDES")),
		LogFile:     envOr("OSS_MIGRATE_LOG", "/var/log/migrate-server/oss-migrate.log"),
		OSSUtilPath: os.Getenv("OSSUTIL_PATH"),
	}
	mig, err := NewMigrator(cfg)
	if err != nil {
		log.Fatalf("invalid migrate config: %v", err)
	}

	k8sClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: mustScheme()})
	if err != nil {
		log.Fatalf("failed to create k8s client: %v", err)
	}

	srv := &server{
		nodeName: nodeName,
		mig:      mig,
		k8s:      k8sClient,
		tasks:    newTaskStore(),
		auth:     newAuthenticator(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Login/logout are reachable without a session (obviously), and are matched
	// both at the root and under /node/<self>/ so the console works either way.
	mux.HandleFunc("/login", srv.auth.handleLoginPage)
	mux.HandleFunc("/login/submit", srv.auth.handleLogin)
	mux.HandleFunc("/logout", srv.auth.handleLogout)
	// /node/<self>/... is guarded for everything except its login/logout paths
	// (routeNode decides), so the per-node console cannot bypass auth.
	mux.HandleFunc("/node/", srv.routeNode)
	// Everything else requires a valid session.
	mux.HandleFunc("/", srv.auth.requireAuth(srv.route))
	mux.HandleFunc("/api/", srv.auth.requireAuth(srv.route))

	port := envOr("PORT", "3000")
	hs := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("migrate-server on node=%s listening :%s", nodeName, port)
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down migrate-server")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(shutCtx)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = aiv1alpha1.AddToScheme(s)
	return s
}

type server struct {
	nodeName string
	mig      *Migrator
	k8s      client.Client
	tasks    *taskStore
	auth     *authenticator
}

// routeNode guards the /node/<self>/... tree: login/logout sub-paths are open,
// everything else requires a session. This closes the hole where the per-node
// console UI (served under /node/<self>/) could be reached without logging in.
func (s *server) routeNode(w http.ResponseWriter, r *http.Request) {
	_, rest := splitNodePrefix(r.URL.Path)
	switch rest {
	case "/login", "/login/submit", "/logout":
		s.route(w, r)
	default:
		s.auth.requireAuth(s.route)(w, r)
	}
}

// route parses an optional /node/<name>/ prefix. When the prefix names a
// different node, the request is rejected with 409 so nginx misrouting cannot
// cause cross-node operations.
func (s *server) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	// serve embedded UI at "/" and "/node/<self>/"
	targetNode, rest := splitNodePrefix(path)

	if targetNode != "" && targetNode != s.nodeName {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("this Pod serves node %q, not %q (check nginx upstream)", s.nodeName, targetNode))
		return
	}

	switch {
	case rest == "/login":
		s.auth.handleLoginPage(w, r)
	case rest == "/login/submit":
		s.auth.handleLogin(w, r)
	case rest == "/logout":
		s.auth.handleLogout(w, r)
	case rest == "/" || rest == "":
		s.serveUI(w, r)
	case rest == "/api/node/info":
		s.handleNodeInfo(w, r)
	case rest == "/api/nodes":
		s.handleNodes(w, r)
	case rest == "/api/workspaces":
		s.handleWorkspaces(w, r)
	case rest == "/api/migrate/remote-check":
		s.handleRemoteCheck(w, r)
	case rest == "/api/migrate/run":
		s.handleRun(w, r)
	case rest == "/api/migrate/cleanup":
		s.handleCleanup(w, r)
	case rest == "/api/migrate/resources":
		s.handleResources(w, r)
	case rest == "/api/tasks":
		s.handleTasksList(w, r)
	case strings.HasPrefix(rest, "/api/tasks/"):
		s.handleTaskGet(w, r, strings.TrimPrefix(rest, "/api/tasks/"))
	case rest == "/api/logs":
		s.handleLogs(w, r)
	default:
		http.NotFound(w, r)
	}
}

func splitNodePrefix(p string) (node, rest string) {
	if !strings.HasPrefix(p, "/node/") {
		return "", p
	}
	tail := strings.TrimPrefix(p, "/node/")
	if i := strings.IndexByte(tail, '/'); i >= 0 {
		return tail[:i], tail[i:]
	}
	return tail, "/"
}

func (s *server) serveUI(w http.ResponseWriter, r *http.Request) {
	b, err := portalFS.ReadFile("portal.html")
	if err != nil {
		http.Error(w, "ui not embedded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// ---- request/response helpers ----

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(v)
}

// ---- handlers ----

type nodeInfo struct {
	Name       string `json:"name"`
	HostIP     string `json:"hostIP"`
	Port       string `json:"port"`
	Root       string `json:"root"`
	OSS        string `json:"oss"`
	GatewayURL string `json:"gatewayURL"` // /node/<name>/ prefix for the UI
}

func (s *server) handleNodeInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildNodeInfo(r))
}

func (s *server) buildNodeInfo(r *http.Request) nodeInfo {
	host := r.URL.Query().Get("hostIP")
	return nodeInfo{
		Name:       s.nodeName,
		HostIP:     host,
		Port:       envOr("PORT", "3000"),
		Root:       s.mig.cfg.Root,
		OSS:        fmt.Sprintf("%s/%s", s.mig.cfg.OSSBucket, s.mig.cfg.OSSPrefix),
		GatewayURL: "/node/" + s.nodeName + "/",
	}
}

// handleNodes lists cluster nodes from the K8s API. This Pod answers for the
// whole cluster because the UI needs the full node list, but each node's
// workspaces are fetched from that node's own Pod by the frontend.
func (s *server) handleNodes(w http.ResponseWriter, r *http.Request) {
	list := &corev1.NodeList{}
	if err := s.k8s.List(r.Context(), list); err != nil {
		writeErr(w, http.StatusInternalServerError, "list nodes: "+err.Error())
		return
	}
	type nodeRow struct {
		Name       string `json:"name"`
		HostIP     string `json:"hostIP"`
		Ready      bool   `json:"ready"`
		Self       bool   `json:"self"`
		GatewayURL string `json:"gatewayURL"`
	}
	var rows []nodeRow
	for _, n := range list.Items {
		var ip string
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				ip = a.Address
				break
			}
		}
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		rows = append(rows, nodeRow{
			Name:       n.Name,
			HostIP:     ip,
			Ready:      ready,
			Self:       n.Name == s.nodeName,
			GatewayURL: "/node/" + n.Name + "/",
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"self": s.nodeName, "nodes": rows})
}

func (s *server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	list, err := s.mig.ListLocal()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list local workspaces: "+err.Error())
		return
	}
	_ = ctx
	if list == nil {
		list = []Workspace{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"node": s.nodeName, "workspaces": list})
}

type remoteCheckReq struct {
	IDs []string `json:"ids"`
}

func (s *server) handleRemoteCheck(w http.ResponseWriter, r *http.Request) {
	var req remoteCheckReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	type row struct {
		Workspace string `json:"workspace"`
		HasData   bool   `json:"hasData"`
		Status    string `json:"status"` // empty|data|unknown
		Error     string `json:"error,omitempty"`
	}
	var out []row
	for _, raw := range req.IDs {
		ws, err := s.mig.ResolveLocal(raw)
		if err != nil || ws == nil {
			out = append(out, row{Workspace: raw, Status: "unknown", Error: "not found or ambiguous"})
			continue
		}
		remote := fmt.Sprintf("%s/%s/ws-%s/", s.mig.cfg.OSSBucket, s.mig.cfg.OSSPrefix, ws.ID)
		st, err := s.mig.remoteState(ctx, remote)
		if err != nil {
			out = append(out, row{Workspace: ws.Name, Status: "unknown", Error: err.Error()})
			continue
		}
		if st == remoteHasData {
			out = append(out, row{Workspace: ws.Name, HasData: true, Status: "data"})
		} else {
			out = append(out, row{Workspace: ws.Name, Status: "empty"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"node": s.nodeName, "results": out})
}

type runReq struct {
	IDs     []string `json:"ids"`
	Dirs    []string `json:"dirs,omitempty"`
	Force   bool     `json:"force,omitempty"`
	DryRun  bool     `json:"dryRun,omitempty"`
	Cleanup bool     `json:"cleanup,omitempty"`
}

func (s *server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req runReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	if len(req.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "ids is required")
		return
	}
	t := s.tasks.create("migrate")
	go s.runMigration(t, req)
	writeJSON(w, http.StatusAccepted, map[string]string{"taskId": t.id})
}

func (s *server) runMigration(t *task, req runReq) {
	defer t.finish()
	migCfg := s.mig.cfg
	migCfg.Force = req.Force
	migCfg.DryRun = req.DryRun
	if len(req.Dirs) > 0 {
		migCfg.Dirs = req.Dirs
	}
	mig, err := NewMigrator(migCfg)
	if err != nil {
		t.fail(err.Error())
		return
	}
	for _, raw := range req.IDs {
		if t.canceled() {
			t.log("任务已取消")
			return
		}
		ws, err := mig.ResolveLocal(raw)
		if err != nil || ws == nil {
			t.addItem(MigrateResult{Workspace: raw, Status: "FAIL", Note: "not found or ambiguous"})
			t.log(fmt.Sprintf("%s: 未找到对应本地目录或名称有二义性", raw))
			continue
		}
		t.setRunning(ws.Name)
		t.log(fmt.Sprintf("开始处理 %s (目录: %s)", ws.Name, ws.Dir))
		res := mig.MigrateOne(context.Background(), *ws)
		t.addItem(res)
		for _, det := range res.Details {
			t.log(fmt.Sprintf("  ↳ %s", det))
		}
		t.log(fmt.Sprintf("%s 处理完毕: 状态 %s (说明: %s, 大小: %s, 文件数: %s, 耗时: %s)",
			ws.Name, res.Status, res.Note, res.Size, res.Files, res.Elapsed))

		if req.Cleanup && res.Status == "OK" && !req.DryRun {
			t.log(fmt.Sprintf("开始清理 %s K8s 资源...", ws.Name))
			cl := NewCleaner(s.k8s, s.mig.cfg.Root, false, mig, mig.cfg.LogFile)
			cr := cl.Clean(context.Background(), *ws)
			t.addCleanup(cr)
			for _, act := range cr.Actions {
				t.log(fmt.Sprintf("  ↳ [清理] %s", act))
			}
			t.log(fmt.Sprintf("%s 清理完成: 状态 %s", ws.Name, cr.Status))
		}
	}
	t.log("所有指定工作空间处理完毕")
}

type cleanupReq struct {
	IDs    []string `json:"ids"`
	DryRun bool     `json:"dryRun,omitempty"`
}

func (s *server) handleCleanup(w http.ResponseWriter, r *http.Request) {
	var req cleanupReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	cl := NewCleaner(s.k8s, s.mig.cfg.Root, req.DryRun, s.mig, s.mig.cfg.LogFile)
	var results []CleanupResult
	for _, raw := range req.IDs {
		ws, err := s.mig.ResolveLocal(raw)
		if err != nil || ws == nil {
			results = append(results, CleanupResult{Workspace: raw, Status: "FAIL", Error: "not found or ambiguous", DryRun: req.DryRun})
			continue
		}
		results = append(results, cl.Clean(ctx, *ws))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"node": s.nodeName, "results": results})
}

// handleResources is a READ-ONLY listing of the local-path CR/PVC/PV behind the
// selected workspaces, so an operator can inspect exactly what cleanup would
// touch before deleting. It never deletes anything and never fails the request
// for "nothing found" — missing objects come back as Found=false with a Note.
func (s *server) handleResources(w http.ResponseWriter, r *http.Request) {
	var req cleanupReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cl := NewCleaner(s.k8s, s.mig.cfg.Root, true, s.mig, s.mig.cfg.LogFile)
	var results []ResourceInfo
	for _, raw := range req.IDs {
		ws, err := s.mig.ResolveLocal(raw)
		if err != nil || ws == nil {
			// Not a local dir (already cleaned, or ambiguous): still answer with
			// an empty row rather than an error, so the operator sees "无".
			results = append(results, ResourceInfo{Workspace: makeWsDisplayName(raw), Note: "本机未找到对应本地目录（可能已清理或名称二义）"})
			continue
		}
		results = append(results, cl.InspectResources(ctx, *ws))
	}
	if results == nil {
		results = []ResourceInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"node": s.nodeName, "results": results})
}

// makeWsDisplayName normalizes a raw id to the "ws-<id>" display form.
func makeWsDisplayName(raw string) string {
	id := normalizeID(raw)
	if id == "" {
		return raw
	}
	return "ws-" + id
}

func (s *server) handleTasksList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"tasks": s.tasks.list()})
}

func (s *server) handleTaskGet(w http.ResponseWriter, r *http.Request, id string) {
	t := s.tasks.get(id)
	if t == nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	writeJSON(w, http.StatusOK, t.snapshot())
}

func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	path := s.mig.cfg.LogFile
	if path == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{"rows": []interface{}{}})
		return
	}
	rows, err := readLogTSV(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read log: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"rows": rows})
}

// ---- task store ----

type task struct {
	id        string
	kind      string
	mu        sync.Mutex
	status    string // running | done
	error     string
	running   string
	items     []MigrateResult
	cleanups  []CleanupResult
	logs      []string
	startedAt time.Time
	endedAt   time.Time
	cancelCh  chan struct{}
}

func (t *task) canceled() bool {
	select {
	case <-t.cancelCh:
		return true
	default:
		return false
	}
}
func (t *task) setRunning(name string) {
	t.mu.Lock()
	t.running = name
	t.mu.Unlock()
}
func (t *task) addItem(r MigrateResult) {
	t.mu.Lock()
	t.items = append(t.items, r)
	t.mu.Unlock()
}
func (t *task) addCleanup(c CleanupResult) {
	t.mu.Lock()
	t.cleanups = append(t.cleanups, c)
	t.mu.Unlock()
}
func (t *task) log(msg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), msg)
	t.logs = append(t.logs, entry)
	if len(t.logs) > 500 {
		t.logs = t.logs[len(t.logs)-500:]
	}
}
func (t *task) finish() {
	t.mu.Lock()
	t.status = "done"
	t.running = ""
	t.endedAt = time.Now()
	t.mu.Unlock()
}
func (t *task) fail(msg string) {
	t.mu.Lock()
	t.status = "done"
	t.error = msg
	t.endedAt = time.Now()
	t.mu.Unlock()
}
func (t *task) snapshot() map[string]interface{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return map[string]interface{}{
		"id":        t.id,
		"kind":      t.kind,
		"status":    t.status,
		"error":     t.error,
		"running":   t.running,
		"items":     append([]MigrateResult{}, t.items...),
		"cleanups":  append([]CleanupResult{}, t.cleanups...),
		"logs":      append([]string{}, t.logs...),
		"startedAt": t.startedAt.Format(time.RFC3339),
		"endedAt":   t.endedAt.Format(time.RFC3339),
	}
}

type taskStore struct {
	mu    sync.Mutex
	seq   int
	items map[string]*task
	order []string
}

func newTaskStore() *taskStore {
	return &taskStore{items: map[string]*task{}}
}

func (ts *taskStore) create(kind string) *task {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.seq++
	id := fmt.Sprintf("%s-%d-%d", kind, time.Now().Unix(), ts.seq)
	t := &task{id: id, kind: kind, status: "running", startedAt: time.Now(), cancelCh: make(chan struct{})}
	ts.items[id] = t
	ts.order = append(ts.order, id)
	// keep the last 50
	if len(ts.order) > 50 {
		drop := ts.order[0]
		ts.order = ts.order[1:]
		delete(ts.items, drop)
	}
	return t
}

func (ts *taskStore) get(id string) *task {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.items[id]
}

func (ts *taskStore) list() []interface{} {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var out []interface{}
	for i := len(ts.order) - 1; i >= 0; i-- {
		if t, ok := ts.items[ts.order[i]]; ok {
			out = append(out, t.snapshot())
		}
	}
	return out
}
