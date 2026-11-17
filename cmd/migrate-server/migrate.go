package main

// migrate.go is a Go port of hack/migrate-localpath-to-oss.sh.
//
// It moves Workspace PVC data from the node-local local-path directory into an
// OSS bucket. It runs inside a per-node DaemonSet Pod: every Pod only ever sees
// its own node's hostPath (/data/local-path-storage), so it can only migrate and
// clean up the Workspaces that physically live on that node.
//
// Directory mapping (identical to the shell script):
//
//	local : /data/local-path-storage/pvc-<uid>_<ns>_ws-<ID>-pvc/{workspace,bocomwork,bocomcode}
//	oss   : oss://<bucket>/<prefix>/ws-<ID>/{workspace,bocomwork,bocomcode}
//
// Sync semantics (identical to the shell script):
//   - Probe the remote prefix first.
//   - Already has real objects (ignoring "dir/" placeholder keys) -> skip the
//     whole Workspace, upload nothing, overwrite nothing, delete nothing.
//   - Remote empty -> upload.
//   - Cannot confirm (ossutil access error) -> FAIL this Workspace; never blindly
//     upload on top of possibly-existing data.
//   - Force -> ignore remote state and overwrite.
//
// Exclude rules (identical to the shell script): the builtin skills directory is
// not uploaded. Rather than ossutil --exclude wildcards, we "carve" the tree:
// for every level above an excluded path we only upload the non-excluded
// siblings, and never list or upload the excluded branch itself.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults mirror the shell script's defaults.
const (
	DefaultRoot       = "/data/work/local-path-storage"
	DefaultOSSBucket  = "oss://euvduatc-oss"
	DefaultOSSPrefix  = "workspaces"
	DefaultDirs       = "workspace,bocomwork,bocomcode"
	DefaultExcludes   = "bocomwork/server/skills/builtin"
	DefaultRetries    = 3
	DefaultRetrySleep = 5 * time.Second
)

// MigrateConfig holds the resolved settings for one migrator instance.
type MigrateConfig struct {
	Root       string   // local data root, e.g. /data/local-path-storage
	OSSBucket  string   // oss://bucket
	OSSPrefix  string   // key prefix, e.g. workspaces
	Dirs       []string // sub-directories to migrate
	Excludes   []string // relative paths under the PVC root to skip
	Retries    int
	RetrySleep time.Duration
	Force      bool // ignore remote state, force overwrite
	DryRun     bool // print commands, write nothing, upload nothing
	LogFile    string
	// OSSUtilPath overrides the ossutil binary (tests inject a fake).
	OSSUtilPath string
	// ExtraArgs are appended after the ossutil sub-command.
	ExtraArgs []string
}

// NormalizeConfig fills in defaults and validates the config.
func NormalizeConfig(c *MigrateConfig) error {
	if c.Root == "" {
		c.Root = DefaultRoot
	}
	if c.OSSBucket == "" {
		c.OSSBucket = DefaultOSSBucket
	}
	c.OSSBucket = strings.TrimSuffix(c.OSSBucket, "/")
	if c.OSSPrefix == "" {
		c.OSSPrefix = DefaultOSSPrefix
	}
	c.OSSPrefix = strings.Trim(c.OSSPrefix, "/")

	if len(c.Dirs) == 0 {
		c.Dirs = splitList(DefaultDirs)
	}
	if c.Retries <= 0 {
		c.Retries = DefaultRetries
	}
	if c.RetrySleep <= 0 {
		c.RetrySleep = DefaultRetrySleep
	}
	if len(c.Excludes) == 0 && !containsSentinelNoExclude(c.Excludes) {
		// Caller passing nil means "use defaults". A caller that wants to upload
		// everything must pass a non-nil empty slice with the no-exclude marker;
		// see ResolveExcludes for the CLI path. Here we simply apply defaults.
		c.Excludes = splitList(DefaultExcludes)
	}

	if !strings.HasPrefix(c.OSSBucket, "oss://") {
		return fmt.Errorf("oss bucket must start with oss:// : %q", c.OSSBucket)
	}
	if c.OSSPrefix == "" {
		return fmt.Errorf("oss prefix must not be empty")
	}
	for _, d := range c.Dirs {
		if d == "" || strings.ContainsAny(d, "/") {
			return fmt.Errorf("invalid migrate dir %q", d)
		}
	}
	for _, e := range c.Excludes {
		if strings.HasPrefix(e, "/") {
			return fmt.Errorf("exclude must be relative to PVC root: %q", e)
		}
	}
	return nil
}

// containsSentinelNoExclude is a placeholder for a future explicit "no excludes"
// flag; currently unused so defaults always apply when none are provided.
func containsSentinelNoExclude(_ []string) bool { return false }

// ResolveExcludes turns a CSV exclude spec into a normalized slice. An empty
// spec keeps defaults; the literal "none" disables all excludes.
func ResolveExcludes(spec string) []string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return splitList(DefaultExcludes)
	}
	if spec == "none" {
		return []string{}
	}
	var out []string
	for _, e := range splitList(spec) {
		e = strings.TrimPrefix(e, "./")
		e = strings.TrimSuffix(e, "/")
		if e == "" || e == "." || e == ".." || strings.HasPrefix(e, "../") || strings.Contains(e, "/../") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '，'
	})
	var out []string
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// Workspace is one migratable unit discovered on this node.
type Workspace struct {
	ID        string   `json:"id"`      // bare ID, e.g. "9660946"
	Name      string   `json:"name"`    // ws-<ID>
	Dir       string   `json:"dir"`     // absolute local PVC dir
	Subdirs   []string `json:"subdirs"` // present sub-directories
	SizeBytes int64    `json:"sizeBytes"`
	Files     int64    `json:"files"`
}

// MigrateResult is the outcome for one Workspace.
type MigrateResult struct {
	Workspace string   `json:"workspace"`
	Status    string   `json:"status"` // OK | SKIP | FAIL
	Dir       string   `json:"dir"`
	Remote    string   `json:"remote"`
	Dirs      string   `json:"dirs"`
	Size      string   `json:"size"`
	Files     string   `json:"files"`
	Note      string   `json:"note"`
	Elapsed   string   `json:"elapsed"`
	Synced    []string `json:"synced,omitempty"`  // 成功同步的子目录
	Skipped   []string `json:"skipped,omitempty"` // 跳过或排除的子目录/分支
	Missing   []string `json:"missing,omitempty"` // 本地不存在的子目录
	Details   []string `json:"details,omitempty"` // 逐步执行明细
}

// pvcNameRe matches pvc-<uuid>_<ns>_ws-<NAME>-pvc directory names, where NAME
// is the FULL workspace name including any random suffix, e.g.
//
//	pvc-4fcb..._bocomwork_ws-9660946-guwpb-pvc -> 9660946-guwpb
//	pvc-06aa..._bocomwork_ws-9526043-pvc       -> 9526043
//
// The suffix is part of the workspace name and must NOT be stripped (doing so
// would collide "ws-9660946-guwpb" with "ws-9660946" and upload to the wrong
// OSS prefix).
var pvcNameRe = regexp.MustCompile(`^pvc-[^_]+_[^_]+_ws-(.+)-pvc$`)

// DirToID extracts the full Workspace ID (including any -suffix) from a PVC
// directory name.
// pvc-xxx_bocomwork_ws-9660946-pvc        -> 9660946
// pvc-xxx_bocomwork_ws-9660946-guwpb-pvc  -> 9660946-guwpb
func DirToID(base string) (string, bool) {
	m := pvcNameRe.FindStringSubmatch(base)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ValidID reports whether id is a safe bare Workspace ID.
func ValidID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// normalizeID tolerates ws-9660946 / ws-9660946-pvc / full paths.
func normalizeID(raw string) string {
	id := strings.TrimSuffix(raw, "/")
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	if i := strings.LastIndex(id, "_ws-"); i >= 0 {
		id = id[i+len("_ws-"):]
	}
	id = strings.TrimPrefix(id, "ws-")
	id = strings.TrimSuffix(id, "-pvc")
	return id
}

// Migrator performs migration for one node.
type Migrator struct {
	cfg MigrateConfig
	mu  sync.Mutex // serializes ossutil invocations per instance
}

func NewMigrator(cfg MigrateConfig) (*Migrator, error) {
	if err := NormalizeConfig(&cfg); err != nil {
		return nil, err
	}
	return &Migrator{cfg: cfg}, nil
}

// ListLocal scans the local root and returns every Workspace directory found.
func (m *Migrator) ListLocal() ([]Workspace, error) {
	entries, err := os.ReadDir(m.cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("read root %q: %w", m.cfg.Root, err)
	}
	var out []Workspace
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, ok := DirToID(e.Name())
		if !ok {
			continue
		}
		dir := filepath.Join(m.cfg.Root, e.Name())
		ws := Workspace{ID: id, Name: "ws-" + id, Dir: dir}
		for _, d := range m.cfg.Dirs {
			if fi, err := os.Stat(filepath.Join(dir, d)); err == nil && fi.IsDir() {
				ws.Subdirs = append(ws.Subdirs, d)
			}
		}
		size, files := dirStats(dir, m.cfg.Dirs, m.cfg.Excludes)
		ws.SizeBytes = size
		ws.Files = files
		out = append(out, ws)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ResolveLocal maps a raw user ID (any tolerated spelling) to a discovered
// Workspace. Returns nil if not found; an error if ambiguous.
func (m *Migrator) ResolveLocal(raw string) (*Workspace, error) {
	all, err := m.ListLocal()
	if err != nil {
		return nil, err
	}
	want := normalizeID(raw)
	// exact match first
	for i := range all {
		if all[i].ID == want {
			return &all[i], nil
		}
	}
	// unique prefix match (ws-9743616-guwpb style / numeric normalization)
	var hits []Workspace
	for i := range all {
		if strings.HasPrefix(all[i].ID, want+"-") || all[i].ID == want {
			hits = append(hits, all[i])
		}
	}
	if len(hits) == 0 && isNumeric(want) {
		var norm []Workspace
		for i := range all {
			if isNumeric(all[i].ID) && trimLeadingZeros(all[i].ID) == trimLeadingZeros(want) {
				norm = append(norm, all[i])
			}
		}
		hits = norm
	}
	switch len(hits) {
	case 1:
		return &hits[0], nil
	case 0:
		return nil, nil
	default:
		return nil, fmt.Errorf("ws-%s matches multiple local directories", want)
	}
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func trimLeadingZeros(s string) string {
	t := strings.TrimLeft(s, "0")
	if t == "" {
		return "0"
	}
	return t
}

// MigrateOne runs the full migration for a single Workspace. Returns the
// result; the Status field conveys OK / SKIP / FAIL.
func (m *Migrator) MigrateOne(ctx context.Context, ws Workspace) MigrateResult {
	start := time.Now()
	remote := fmt.Sprintf("%s/%s/ws-%s", m.cfg.OSSBucket, m.cfg.OSSPrefix, ws.ID)
	res := MigrateResult{Workspace: ws.Name, Dir: ws.Dir, Remote: remote, Size: "-", Files: "-"}
	finish := func(status, note string) MigrateResult {
		res.Status = status
		res.Note = note
		res.Elapsed = time.Since(start).Round(time.Millisecond).String()
		m.logWrite(res)
		return res
	}

	if fi, err := os.Stat(ws.Dir); err != nil || !fi.IsDir() {
		return finish("FAIL", "local directory unreadable")
	}

	var present []string
	var missing []string
	for _, d := range m.cfg.Dirs {
		if fi, err := os.Stat(filepath.Join(ws.Dir, d)); err == nil && fi.IsDir() {
			present = append(present, d)
		} else {
			missing = append(missing, d)
		}
	}
	res.Missing = missing
	if len(missing) > 0 {
		res.Details = append(res.Details, fmt.Sprintf("本地未发现子目录 (未同步): %s", strings.Join(missing, ", ")))
	}
	if len(present) == 0 {
		return finish("SKIP", "no migratable sub-directories")
	}
	res.Dirs = strings.Join(present, ",")

	// 1) remote state gate
	if !m.cfg.Force && !m.cfg.DryRun {
		state, err := m.remoteState(ctx, remote+"/")
		if err != nil {
			return finish("FAIL", "cannot confirm remote state: "+err.Error())
		}
		if state == remoteHasData {
			res.Skipped = append(res.Skipped, "远端已存在数据 (跳过整个工作空间)")
			res.Details = append(res.Details, "远端 OSS 目标前缀已存在对象，整体跳过同步")
			return finish("SKIP", "remote already has data")
		}
	}

	// 2) upload each sub-directory (carving excluded branches)
	units := 0
	for _, d := range present {
		n, err := m.uploadTree(ctx, ws.Dir, remote, d)
		units += n
		if err != nil {
			return finish("FAIL", fmt.Sprintf("ossutil cp failed for %s: %v", d, err))
		}
		res.Synced = append(res.Synced, d)
		res.Details = append(res.Details, fmt.Sprintf("已同步子目录: %s", d))
	}
	if len(m.cfg.Excludes) > 0 {
		for _, e := range m.cfg.Excludes {
			res.Skipped = append(res.Skipped, fmt.Sprintf("排除规则: %s", e))
			res.Details = append(res.Details, fmt.Sprintf("已排除不上传分支: %s", e))
		}
	}
	if units == 0 {
		return finish("SKIP", "all contents excluded")
	}

	if m.cfg.DryRun {
		return finish("OK", "dry-run")
	}

	// 3) remote verification: fail only if a subdir has local files but the
	// remote prefix has zero objects.
	var localFiles int64
	for _, d := range present {
		localFiles += countFiles(filepath.Join(ws.Dir, d), m.cfg.Excludes)
	}
	remoteCnt := m.remoteCount(ctx, remote+"/", 500)
	if remoteCnt == 0 && localFiles > 0 {
		return finish("FAIL", fmt.Sprintf("local has %d files but remote has 0 objects", localFiles))
	}

	// 4) stats
	size, files := dirStats(ws.Dir, present, m.cfg.Excludes)
	res.Size = humanSize(size)
	res.Files = strconv.FormatInt(files, 10)
	return finish("OK", fmt.Sprintf("remote_objects=%d", remoteCnt))
}

type remoteStateResult int

const (
	remoteHasData remoteStateResult = iota
	remoteEmpty
)

// remoteState probes whether a remote prefix already holds real objects.
// Only explicit access errors are treated as failure (caller maps to FAIL).
func (m *Migrator) remoteState(ctx context.Context, prefix string) (remoteStateResult, error) {
	out, err := m.ossutilCapture(ctx, "ls", prefix)
	if err != nil {
		// Only a "not exist" error counts as empty. Anything else (permission,
		// network, empty output) is an access error we cannot confirm -> the
		// caller must treat it as FAIL, never as empty.
		if isNotExistErr(out) {
			return remoteEmpty, nil
		}
		// Surface ossutil's actual diagnostic instead of the bare Go exit
		// status ("exit status 1"), otherwise the failure is undebuggable.
		if detail := strings.TrimSpace(tailLines(out, 20)); detail != "" {
			return remoteEmpty, fmt.Errorf("%v: %s", err, detail)
		}
		return remoteEmpty, err
	}
	if hasRealObject(out) {
		return remoteHasData, nil
	}
	return remoteEmpty, nil
}

func (m *Migrator) remoteCount(ctx context.Context, prefix string, cap int) int64 {
	out, err := m.ossutilCapture(ctx, "ls", prefix)
	if err != nil {
		return 0
	}
	var n int64
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "oss://") {
			continue
		}
		if strings.HasSuffix(strings.TrimSpace(line), "/") {
			continue // directory placeholder
		}
		n++
		if int(n) >= cap {
			break
		}
	}
	return n
}

func hasRealObject(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "oss://") {
			continue
		}
		if strings.HasSuffix(strings.TrimSpace(line), "/") {
			continue
		}
		return true
	}
	return false
}

func isEmptyListing(out string) bool {
	// ossutil prints a summary line "Object Number is: 0" for empty listings.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Object Number is: 0") {
			return true
		}
	}
	return !strings.Contains(out, "oss://")
}

func isNotExistErr(out string) bool {
	low := strings.ToLower(out)
	for _, s := range []string{"nosuchkey", "nosuchbucket", "no such key", "not exist", "does not exist", "404"} {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

// uploadTree uploads rel (a file or dir) under pvc into remote/rel, skipping
// excluded branches. Returns the number of ossutil cp units performed.
func (m *Migrator) uploadTree(ctx context.Context, pvc, remote, rel string) (int, error) {
	if m.isExcluded(rel) {
		return 0, nil
	}
	abs := filepath.Join(pvc, rel)
	fi, err := os.Stat(abs)
	if err != nil {
		return 0, err
	}
	if fi.IsDir() && m.hasExcludedBelow(rel) {
		entries, err := os.ReadDir(abs)
		if err != nil {
			return 0, err
		}
		total := 0
		for _, e := range entries {
			n, err := m.uploadTree(ctx, pvc, remote, filepath.ToSlash(filepath.Join(rel, e.Name())))
			total += n
			if err != nil {
				return total, err
			}
		}
		return total, nil
	}
	// leaf: upload the whole file/dir
	if err := m.cpOne(ctx, pvc, remote, rel, fi.IsDir()); err != nil {
		return 1, err
	}
	return 1, nil
}

func (m *Migrator) cpOne(ctx context.Context, pvc, remote, rel string, isDir bool) error {
	if m.cfg.DryRun {
		return nil
	}
	args := []string{"cp"}
	if isDir {
		args = append(args, "-r")
	}
	args = append(args, "-f", rel)
	if isDir {
		args = append(args, remote+"/"+rel+"/")
	} else {
		args = append(args, remote+"/"+rel)
	}
	var lastErr error
	lastOut := ""
	for attempt := 1; attempt <= m.cfg.Retries; attempt++ {
		out, err := m.ossutilRun(ctx, pvc, args...)
		if err == nil {
			return nil
		}
		lastErr, lastOut = err, out
		if attempt < m.cfg.Retries {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.cfg.RetrySleep):
			}
		}
	}
	return fmt.Errorf("%v: %s", lastErr, strings.TrimSpace(tailLines(lastOut, 20)))
}

func (m *Migrator) isExcluded(rel string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	for _, e := range m.cfg.Excludes {
		if rel == e || strings.HasPrefix(rel, e+"/") {
			return true
		}
	}
	return false
}

func (m *Migrator) hasExcludedBelow(rel string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	for _, e := range m.cfg.Excludes {
		if strings.HasPrefix(e, rel+"/") {
			return true
		}
	}
	return false
}

// ossutilRun runs ossutil with the working directory set to dir (the script
// does `cd <pvc> && ossutil cp ...` so relative paths resolve under the PVC).
func (m *Migrator) ossutilRun(ctx context.Context, dir string, args ...string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	full := append([]string{}, args...)
	full = append(full, m.cfg.ExtraArgs...)
	cmd := exec.CommandContext(ctx, m.ossBin(), full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (m *Migrator) ossutilCapture(ctx context.Context, args ...string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	full := append([]string{}, args...)
	full = append(full, m.cfg.ExtraArgs...)
	cmd := exec.CommandContext(ctx, m.ossBin(), full...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (m *Migrator) ossBin() string {
	if m.cfg.OSSUtilPath != "" {
		return m.cfg.OSSUtilPath
	}
	return "ossutil"
}

// dirStats sums size (bytes) and file count over the given sub-directories,
// subtracting excluded paths.
func dirStats(root string, subdirs, excludes []string) (int64, int64) {
	var size, files int64
	for _, d := range subdirs {
		filepath.Walk(filepath.Join(root, d), func(p string, info os.FileInfo, err error) error {
			if err != nil || info == nil {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			if isExcludedRel(rel, excludes) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !info.IsDir() {
				size += info.Size()
				files++
			}
			return nil
		})
	}
	return size, files
}

func countFiles(dir string, excludes []string) int64 {
	var n int64
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if isExcludedAbs(p, dir, excludes) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func isExcludedRel(rel string, excludes []string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	for _, e := range excludes {
		if rel == e || strings.HasPrefix(rel, e+"/") {
			return true
		}
	}
	return false
}

func isExcludedAbs(p, root string, excludes []string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return isExcludedRel(rel, excludes)
}

func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// logWrite appends one TSV row to the log file (skips on dry-run / empty path).
func (m *Migrator) logWrite(r MigrateResult) {
	if m.cfg.DryRun || m.cfg.LogFile == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := os.OpenFile(m.cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	if info != nil && info.Size() == 0 {
		fmt.Fprintln(f, "# time\tstatus\tworkspace\tlocal_dir\tremote_prefix\tdirs\tsize\tfiles\tnote")
	}
	fmt.Fprintf(f, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		time.Now().Format("2006-01-02T15:04:05-0700"),
		r.Status, r.Workspace, r.Dir, r.Remote, r.Dirs, r.Size, r.Files, r.Note)
}

// LogHasOK reports whether the log already recorded success for ws.
func LogHasOK(logFile, ws string) bool {
	f, err := os.Open(logFile)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		cols := strings.Split(sc.Text(), "\t")
		if len(cols) >= 3 && cols[1] == "OK" && cols[2] == ws {
			return true
		}
	}
	return false
}

// LogRow is one parsed TSV log line for the UI.
type LogRow struct {
	Time      string `json:"time"`
	Status    string `json:"status"`
	Workspace string `json:"workspace"`
	LocalDir  string `json:"localDir"`
	Remote    string `json:"remote"`
	Dirs      string `json:"dirs"`
	Size      string `json:"size"`
	Files     string `json:"files"`
	Note      string `json:"note"`
}

// readLogTSV parses the migration log into rows (newest first).
func readLogTSV(path string) ([]LogRow, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []LogRow{}, nil
		}
		return nil, err
	}
	defer f.Close()
	var rows []LogRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		c := strings.Split(line, "\t")
		if len(c) < 9 {
			continue
		}
		rows = append(rows, LogRow{
			Time: c[0], Status: c[1], Workspace: c[2], LocalDir: c[3],
			Remote: c[4], Dirs: c[5], Size: c[6], Files: c[7], Note: c[8],
		})
	}
	// newest first
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}
