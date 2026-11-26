package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed portal.html
var portalFS embed.FS

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// CLI flags
	cliSrc := flag.String("src", "", "Source OSS URI (e.g. oss://euvduatc-oss/workspaces/ws-9743616)")
	cliDst := flag.String("dst", "", "Destination OSS URI (e.g. oss://euvduatc-oss/workspaces/ws-974361-guwnb)")
	cliForce := flag.Bool("force", false, "Force overwrite if target exists")
	cliDryRun := flag.Bool("dry-run", false, "Simulate without copying")
	cliDirs := flag.String("dirs", DefaultDirs, "Comma-separated subdirectories to migrate (empty or * for full prefix)")
	cliExcludes := flag.String("excludes", "", "Comma-separated exclude patterns (default none)")
	cliPort := flag.String("port", "", "HTTP server port (defaults to env PORT or 3000)")
	flag.Parse()

	// 1. CLI Mode: if --src is provided, execute directly and exit
	if *cliSrc != "" {
		runCLI(*cliSrc, *cliDst, *cliForce, *cliDryRun, *cliDirs, *cliExcludes)
		return
	}

	// 2. Server Mode
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := MigrateConfig{
		SourceBucket: envOr("OSS_SOURCE_BUCKET", DefaultSourceBucket),
		SourcePrefix: envOr("OSS_SOURCE_PREFIX", DefaultSourcePrefix),
		TargetBucket: envOr("OSS_TARGET_BUCKET", DefaultTargetBucket),
		TargetPrefix: envOr("OSS_TARGET_PREFIX", DefaultTargetPrefix),
		Dirs:         splitList(envOr("OSS_MIGRATE_DIRS", DefaultDirs)),
		Excludes:     ResolveExcludes(os.Getenv("OSS_MIGRATE_EXCLUDES")),
		LogFile:      envOr("OSS_MIGRATE_LOG", DefaultLogFile),
		OSSUtilPath:  os.Getenv("OSSUTIL_PATH"),
	}

	mig, err := NewMigrator(cfg)
	if err != nil {
		log.Fatalf("invalid migrate config: %v", err)
	}

	srv := &server{
		mig:   mig,
		tasks: newTaskStore(),
		auth:  newAuthenticator(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/login", srv.auth.handleLoginPage)
	mux.HandleFunc("/login/submit", srv.auth.handleLogin)
	mux.HandleFunc("/logout", srv.auth.handleLogout)

	// Protected routes
	mux.HandleFunc("/", srv.auth.requireAuth(srv.route))

	port := *cliPort
	if port == "" {
		port = envOr("PORT", "3000")
	}

	hs := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("oss-migrate-server listening on :%s", port)
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down oss-migrate-server")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(shutCtx)
}

func runCLI(src, dst string, force, dryRun bool, dirs, excludes string) {
	if dst == "" {
		log.Fatal("missing required --dst OSS URI")
	}

	cfg := MigrateConfig{
		SourceBucket: DefaultSourceBucket,
		SourcePrefix: DefaultSourcePrefix,
		TargetBucket: DefaultTargetBucket,
		TargetPrefix: DefaultTargetPrefix,
		Dirs:         splitList(dirs),
		Excludes:     ResolveExcludes(excludes),
		LogFile:      envOr("OSS_MIGRATE_LOG", DefaultLogFile),
		OSSUtilPath:  os.Getenv("OSSUTIL_PATH"),
	}
	mig, err := NewMigrator(cfg)
	if err != nil {
		log.Fatalf("failed to create migrator: %v", err)
	}

	if err := mig.ValidateScope(src, false); err != nil {
		log.Fatalf("源路径受限: %v", err)
	}
	if err := mig.ValidateScope(dst, true); err != nil {
		log.Fatalf("目的路径受限: %v", err)
	}

	fmt.Printf("[INFO] Starting OSS to OSS migration\n")
	fmt.Printf("       Source     : %s\n", src)
	fmt.Printf("       Destination: %s\n", dst)
	fmt.Printf("       Force      : %v\n", force)
	fmt.Printf("       DryRun     : %v\n", dryRun)
	fmt.Printf("       Dirs       : %s\n", dirs)
	fmt.Printf("       Excludes   : %s\n", excludes)

	item := MigrationItem{
		Source:      src,
		Destination: dst,
		Force:       force,
		DryRun:      dryRun,
		Dirs:        splitList(dirs),
		Excludes:    ResolveExcludes(excludes),
	}

	ctx := context.Background()
	res := mig.MigrateOne(ctx, item)

	fmt.Printf("\n[RESULT] Status: %s\n", res.Status)
	fmt.Printf("         Note  : %s\n", res.Note)
	fmt.Printf("         Target: %d objects\n", res.TargetObjects)
	fmt.Printf("         Time  : %s\n", res.Elapsed)
	if len(res.Details) > 0 {
		fmt.Printf("         Details:\n")
		for _, d := range res.Details {
			fmt.Printf("           - %s\n", d)
		}
	}

	if res.Status == "FAIL" {
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type server struct {
	mig   *Migrator
	tasks *taskStore
	auth  *authenticator
}

func (s *server) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/" || path == "":
		s.serveUI(w, r)
	case path == "/api/status":
		s.handleStatus(w, r)
	case path == "/api/migrate/remote-check":
		s.handleRemoteCheck(w, r)
	case path == "/api/migrate/run":
		s.handleRun(w, r)
	case path == "/api/oss/list":
		s.handleOSSList(w, r)
	case path == "/api/tasks":
		s.handleTasksList(w, r)
	case strings.HasPrefix(path, "/api/tasks/"):
		s.handleTaskGet(w, r, strings.TrimPrefix(path, "/api/tasks/"))
	case path == "/api/logs":
		s.handleLogs(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) serveUI(w http.ResponseWriter, r *http.Request) {
	b, err := portalFS.ReadFile("portal.html")
	if err != nil {
		http.Error(w, "ui template not embedded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

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

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	endpoint := os.Getenv("OSS_ENDPOINT")
	if endpoint == "" {
		endpoint = "默认/自配置"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"endpoint":     endpoint,
		"authEnabled":  s.auth.enabled,
		"sourceBucket": s.mig.cfg.SourceBucket,
		"sourcePrefix": s.mig.cfg.SourcePrefix,
		"targetBucket": s.mig.cfg.TargetBucket,
		"targetPrefix": s.mig.cfg.TargetPrefix,
		"defaultDirs":  s.mig.cfg.Dirs,
	})
}

func (s *server) handleRemoteCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req RemoteCheckRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var results []RemoteCheckResult
	for _, uri := range req.URIs {
		res, _ := s.mig.ProbeRemote(ctx, uri)
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}

func (s *server) handleOSSList(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	prefix := r.URL.Query().Get("prefix")
	search := r.URL.Query().Get("search")
	limitStr := r.URL.Query().Get("limit")
	limit := 100
	if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
		limit = l
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	entries, err := s.mig.ListOSSWorkspaces(ctx, bucket, prefix, search, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list OSS workspaces: "+err.Error())
		return
	}

	// Probe target status for each entry (capped to first 20 for latency)
	probeLimit := 20
	if len(entries) < probeLimit {
		probeLimit = len(entries)
	}
	for i := 0; i < probeLimit; i++ {
		p, pErr := s.mig.ProbeRemote(ctx, entries[i].TargetURI)
		if pErr == nil {
			entries[i].TargetExists = p.Exists
			entries[i].TargetObjects = p.ObjectCount
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"entries": entries})
}

func (s *server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req RunMigrationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}
	if len(req.Tasks) == 0 {
		writeErr(w, http.StatusBadRequest, "tasks array is empty")
		return
	}

	// Validate all tasks against allowed directory scope upfront before launching
	for i, item := range req.Tasks {
		if err := s.mig.ValidateScope(item.Source, false); err != nil {
			writeErr(w, http.StatusForbidden, fmt.Sprintf("第 %d 项源路径受限: %v", i+1, err))
			return
		}
		if err := s.mig.ValidateScope(item.Destination, true); err != nil {
			writeErr(w, http.StatusForbidden, fmt.Sprintf("第 %d 项目的路径受限: %v", i+1, err))
			return
		}
	}

	t := s.tasks.create("migrate")
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		t.log(fmt.Sprintf("开始执行迁移任务，共 %d 个项目", len(req.Tasks)))
		for _, item := range req.Tasks {
			if t.canceled() {
				t.log("任务已被用户取消")
				break
			}
			// Apply global overrides if item specific fields are not set
			if !item.Force && req.Force {
				item.Force = req.Force
			}
			if !item.DryRun && req.DryRun {
				item.DryRun = req.DryRun
			}
			if len(item.Dirs) == 0 && len(req.Dirs) > 0 {
				item.Dirs = req.Dirs
			}
			if len(item.Excludes) == 0 && len(req.Excludes) > 0 {
				item.Excludes = req.Excludes
			}

			t.setRunning(item.Source + " -> " + item.Destination)
			t.log(fmt.Sprintf("处理: %s -> %s (force=%v, dryRun=%v)", item.Source, item.Destination, item.Force, item.DryRun))

			res := s.mig.MigrateOne(ctx, item)
			t.addItem(res)
			t.log(fmt.Sprintf("完成: %s -> %s [%s] %s (%s)", res.Source, res.Destination, res.Status, res.Note, res.Elapsed))
		}
		t.finish()
		t.log("所有迁移项目已执行完毕")
	}()

	writeJSON(w, http.StatusOK, map[string]interface{}{"taskId": t.id})
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
	rows, err := readLogTSV(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read log: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"rows": rows})
}

// ---- Task Store ----

type task struct {
	id        string
	kind      string
	mu        sync.Mutex
	status    string // running | done
	error     string
	running   string
	items     []MigrateResult
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

func (t *task) snapshot() map[string]interface{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return map[string]interface{}{
		"id":        t.id,
		"kind":      t.kind,
		"status":    t.status,
		"running":   t.running,
		"items":     t.items,
		"logs":      t.logs,
		"startedAt": t.startedAt.Format(time.RFC3339),
		"endedAt":   t.endedAt.Format(time.RFC3339),
	}
}

type taskStore struct {
	mu    sync.Mutex
	tasks map[string]*task
	order []string
}

func newTaskStore() *taskStore {
	return &taskStore{
		tasks: make(map[string]*task),
	}
}

func (ts *taskStore) create(kind string) *task {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	id := fmt.Sprintf("task-%d", time.Now().UnixNano())
	t := &task{
		id:        id,
		kind:      kind,
		status:    "running",
		startedAt: time.Now(),
		cancelCh:  make(chan struct{}),
	}
	ts.tasks[id] = t
	ts.order = append(ts.order, id)
	if len(ts.order) > 50 {
		oldest := ts.order[0]
		ts.order = ts.order[1:]
		delete(ts.tasks, oldest)
	}
	return t
}

func (ts *taskStore) get(id string) *task {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.tasks[id]
}

func (ts *taskStore) list() []map[string]interface{} {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var out []map[string]interface{}
	for i := len(ts.order) - 1; i >= 0; i-- {
		if t, ok := ts.tasks[ts.order[i]]; ok {
			out = append(out, t.snapshot())
		}
	}
	return out
}
