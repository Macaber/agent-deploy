package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NormalizeConfig applies defaults and validates settings.
func NormalizeConfig(c *MigrateConfig) error {
	if c.SourceBucket == "" {
		c.SourceBucket = DefaultSourceBucket
	}
	c.SourceBucket = strings.TrimSuffix(c.SourceBucket, "/")
	if c.SourcePrefix == "" {
		c.SourcePrefix = DefaultSourcePrefix
	}
	c.SourcePrefix = strings.Trim(c.SourcePrefix, "/")

	if c.TargetBucket == "" {
		c.TargetBucket = DefaultTargetBucket
	}
	c.TargetBucket = strings.TrimSuffix(c.TargetBucket, "/")
	if c.TargetPrefix == "" {
		c.TargetPrefix = DefaultTargetPrefix
	}
	c.TargetPrefix = strings.Trim(c.TargetPrefix, "/")

	if len(c.Dirs) == 0 {
		c.Dirs = splitList(DefaultDirs)
	}
	if c.Retries <= 0 {
		c.Retries = DefaultRetries
	}
	if c.RetrySleep <= 0 {
		c.RetrySleep = DefaultRetrySleep
	}
	if c.LogFile == "" {
		c.LogFile = DefaultLogFile
	}
	if len(c.Excludes) == 0 {
		c.Excludes = splitList(DefaultExcludes)
	}

	if !strings.HasPrefix(c.SourceBucket, "oss://") {
		return fmt.Errorf("source bucket must start with oss:// : %q", c.SourceBucket)
	}
	if !strings.HasPrefix(c.TargetBucket, "oss://") {
		return fmt.Errorf("target bucket must start with oss:// : %q", c.TargetBucket)
	}

	return nil
}

// ResolveExcludes turns a CSV exclude spec into a normalized slice.
// An empty spec or "none" means no files/directories are excluded.
func ResolveExcludes(spec string) []string {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "none" {
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

// normalizeOSSUri ensures an OSS URI has the oss:// prefix and no trailing slash.
func normalizeOSSUri(uri string) string {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return ""
	}
	if !strings.HasPrefix(uri, "oss://") {
		uri = "oss://" + strings.TrimPrefix(uri, "/")
	}
	return strings.TrimRight(uri, "/")
}

// Migrator coordinates OSS-to-OSS migration tasks.
type Migrator struct {
	cfg MigrateConfig
	mu  sync.Mutex // serializes ossutil invocations per instance
}

// NewMigrator creates a new Migrator instance.
func NewMigrator(cfg MigrateConfig) (*Migrator, error) {
	if err := NormalizeConfig(&cfg); err != nil {
		return nil, err
	}
	return &Migrator{cfg: cfg}, nil
}

// ValidateScope verifies that an OSS URI is strictly within the allowed workspace directory scope.
// It ensures that only paths under oss://<bucket>/workspaces/ws-xxxx can be accessed or migrated,
// preventing access to or operations on any other directory.
func (m *Migrator) ValidateScope(uri string, isTarget bool) error {
	uri = normalizeOSSUri(uri)
	if uri == "" {
		return fmt.Errorf("OSS 路径不能为空")
	}

	// 1. Path traversal prevention
	cleanURI := strings.ReplaceAll(uri, "\\", "/")
	for _, seg := range strings.Split(cleanURI, "/") {
		if seg == ".." {
			return fmt.Errorf("非法路径：禁止包含相对路径符号 (..)")
		}
	}

	// 2. Expected base prefix
	bucket := m.cfg.SourceBucket
	prefix := m.cfg.SourcePrefix
	if isTarget {
		bucket = m.cfg.TargetBucket
		prefix = m.cfg.TargetPrefix
	}

	allowedBase := fmt.Sprintf("%s/%s", strings.TrimRight(bucket, "/"), strings.Trim(prefix, "/"))

	// Must start with allowedBase + "/"
	if !strings.HasPrefix(uri, allowedBase+"/") {
		return fmt.Errorf("操作被禁止：仅允许访问和操作 %s/ 目录下的工作空间数据，禁止操作其他目录 (%s)", allowedBase, uri)
	}

	// Must specify a concrete sub-workspace, cannot operate on the base workspaces directory itself
	sub := strings.TrimPrefix(uri, allowedBase+"/")
	sub = strings.Trim(sub, "/")
	if sub == "" {
		return fmt.Errorf("操作被拒绝：不能直接对整个 %s 根目录执行迁移，请指定具体的 ws-xxxx 工作空间", allowedBase)
	}

	return nil
}

// ValidateProbeScope checks if an OSS URI is permitted for probe / inspection.
// It allows probing paths within either the source workspace prefix or target workspace prefix.
func (m *Migrator) ValidateProbeScope(uri string) error {
	errSrc := m.ValidateScope(uri, false)
	if errSrc == nil {
		return nil
	}
	errDst := m.ValidateScope(uri, true)
	if errDst == nil {
		return nil
	}
	return errSrc
}

type remoteStateResult int

const (
	remoteHasData remoteStateResult = iota
	remoteEmpty
)

// MigrateOne transfers data from item.Source to item.Destination.
// Supports:
// - Skip if target exists (!item.Force && remote has data)
// - Force overwrite (item.Force)
// - Dry run (item.DryRun)
// - Exclude patterns (item.Excludes)
// - Subdirectories (item.Dirs)
func (m *Migrator) MigrateOne(ctx context.Context, item MigrationItem) MigrateResult {
	start := time.Now()
	src := normalizeOSSUri(item.Source)
	dst := normalizeOSSUri(item.Destination)

	res := MigrateResult{
		Source:      src,
		Destination: dst,
		Size:        "-",
	}

	finish := func(status, note string) MigrateResult {
		res.Status = status
		res.Note = note
		res.Elapsed = time.Since(start).Round(time.Millisecond).String()
		m.logWrite(res)
		return res
	}

	if src == "" || !strings.HasPrefix(src, "oss://") {
		return finish("FAIL", fmt.Sprintf("invalid source OSS URI: %q", item.Source))
	}
	if dst == "" || !strings.HasPrefix(dst, "oss://") {
		return finish("FAIL", fmt.Sprintf("invalid destination OSS URI: %q", item.Destination))
	}
	if src == dst {
		return finish("FAIL", "source and destination OSS URIs cannot be identical")
	}

	// Strict directory scope check: only allow operations within the designated workspaces directory
	if err := m.ValidateScope(src, false); err != nil {
		return finish("FAIL", err.Error())
	}
	if err := m.ValidateScope(dst, true); err != nil {
		return finish("FAIL", err.Error())
	}

	for _, d := range item.Dirs {
		if strings.Contains(d, "..") {
			return finish("FAIL", fmt.Sprintf("非法待迁移子目录: %q 禁止包含 ..", d))
		}
	}

	// 1. Check Source Existence & Objects
	srcCount := m.remoteCount(ctx, src+"/", 1000)
	res.SourceObjects = srcCount
	if srcCount == 0 {
		// Probe explicitly to see if it's truly empty or inaccessible
		state, err := m.remoteState(ctx, src+"/")
		if err != nil {
			return finish("FAIL", fmt.Sprintf("无法访问源端 OSS (%s): %v", src, err))
		}
		if state == remoteEmpty {
			return finish("FAIL", fmt.Sprintf("源端 OSS 路径不存在或无任何对象数据: %s", src))
		}
	}

	// 2. Target check: "支持存在就跳过，也支持 force 覆盖"
	initialTargetState, err := m.remoteState(ctx, dst+"/")
	targetExisted := false
	var initialTargetCount int64 = 0
	if err == nil && initialTargetState == remoteHasData {
		targetExisted = true
		initialTargetCount = m.remoteCount(ctx, dst+"/", 1000)
		res.TargetObjects = initialTargetCount
	}

	if targetExisted {
		res.Details = append(res.Details, fmt.Sprintf("目标端状态: [已存在] (远端发现 %d 个对象)", initialTargetCount))
		if !item.Force && !item.DryRun {
			res.Skipped = append(res.Skipped, "目标已存在数据 (跳过)")
			res.Details = append(res.Details, "执行策略为【存在就跳过】，已安全跳过不覆盖已有数据")
			return finish("SKIP", fmt.Sprintf("目标已存在 (%d个对象)，按【存在就跳过】策略已跳过 (destination already has data, use force to overwrite)", initialTargetCount))
		}
		if item.Force {
			res.Details = append(res.Details, "执行策略为【强制覆盖】，将覆盖目标端已有数据")
		}
	} else {
		res.Details = append(res.Details, "目标端状态: [不存在/全新空间]，将自动创建并同步数据")
	}

	// 3. Dry Run Check
	if item.DryRun {
		res.Details = append(res.Details, fmt.Sprintf("[演练模式] 将执行从 %s 到 %s 的同步", src, dst))
		if item.Force {
			res.Details = append(res.Details, "[演练模式] 已指定 force 覆盖模式")
		}
		dirs := item.Dirs
		if len(dirs) == 0 {
			dirs = m.cfg.Dirs
		}
		if len(dirs) > 0 {
			res.Details = append(res.Details, fmt.Sprintf("[演练模式] 待迁移子目录: %s", strings.Join(dirs, ", ")))
		} else {
			res.Details = append(res.Details, "[演练模式] 待迁移全量前缀")
		}
		return finish("OK", "dry-run preview")
	}

	// 4. Determine directories to copy
	dirs := item.Dirs
	if len(dirs) == 0 {
		dirs = m.cfg.Dirs
	}

	excludes := item.Excludes
	if len(excludes) == 0 {
		excludes = m.cfg.Excludes
	}
	if len(excludes) > 0 {
		for _, e := range excludes {
			res.Skipped = append(res.Skipped, fmt.Sprintf("排除规则: %s", e))
			res.Details = append(res.Details, fmt.Sprintf("排除分支: %s", e))
		}
	}

	// If subdirectories are specified, we migrate each present subdirectory.
	// If no subdirectories or wildcard "*", we copy the whole prefix.
	if len(dirs) > 0 && !(len(dirs) == 1 && (dirs[0] == "*" || dirs[0] == "all")) {
		var present []string
		var missing []string
		for _, d := range dirs {
			d = strings.Trim(d, "/")
			subSrc := fmt.Sprintf("%s/%s/", src, d)
			cnt := m.remoteCount(ctx, subSrc, 1)
			if cnt > 0 {
				present = append(present, d)
			} else {
				missing = append(missing, d)
			}
		}
		res.Missing = missing
		if len(missing) > 0 {
			res.Details = append(res.Details, fmt.Sprintf("源端未发现子目录 (跳过): %s", strings.Join(missing, ", ")))
		}
		if len(present) == 0 {
			return finish("FAIL", fmt.Sprintf("源端未找到任何指定的待迁移子目录: %s", strings.Join(dirs, ", ")))
		}

		// Copy each present sub-directory
		for _, d := range present {
			subSrc := fmt.Sprintf("%s/%s/", src, d)
			subDst := fmt.Sprintf("%s/%s/", dst, d)

			// Adapt excludes: include both original pattern and pattern relative to sub-directory
			var subExcludes []string
			for _, e := range excludes {
				subExcludes = append(subExcludes, e)
				if strings.HasPrefix(e, d+"/") {
					subExcludes = append(subExcludes, strings.TrimPrefix(e, d+"/"))
				}
			}

			err := m.cpDirectory(ctx, subSrc, subDst, item.Force, subExcludes)
			if err != nil {
				return finish("FAIL", fmt.Sprintf("ossutil cp failed for %s: %v", d, err))
			}
			res.Synced = append(res.Synced, d)
			res.Details = append(res.Details, fmt.Sprintf("成功同步子目录: %s", d))
		}
	} else {
		// Whole prefix copy
		err := m.cpDirectory(ctx, src+"/", dst+"/", item.Force, excludes)
		if err != nil {
			return finish("FAIL", fmt.Sprintf("ossutil cp failed: %v", err))
		}
		res.Synced = append(res.Synced, "*")
		res.Details = append(res.Details, "成功同步全量前缀")
	}

	// 5. Remote Verification
	dstCount := m.remoteCount(ctx, dst+"/", 1000)
	res.TargetObjects = dstCount
	if dstCount == 0 && srcCount > 0 {
		return finish("FAIL", fmt.Sprintf("source had %d objects but destination has 0 objects after copy", srcCount))
	}

	var note string
	if targetExisted {
		if item.Force {
			note = fmt.Sprintf("目标初始已存在(%d个对象)，已强制覆盖完成 (现有 %d 对象)", initialTargetCount, dstCount)
		} else {
			note = fmt.Sprintf("目标初始已存在(%d个对象)，已同步更新 (现有 %d 对象)", initialTargetCount, dstCount)
		}
	} else {
		note = fmt.Sprintf("目标初始不存在(全新空间)，数据同步成功 (写入 %d 对象)", dstCount)
	}
	return finish("OK", note)
}

// cpDirectory runs ossutil cp -r between two OSS URIs with retries.
func (m *Migrator) cpDirectory(ctx context.Context, src, dst string, force bool, excludes []string) error {
	args := []string{"cp", "-r"}
	if force {
		args = append(args, "-f")
	}
	for _, e := range excludes {
		if e != "" {
			args = append(args, "--exclude", fmt.Sprintf("*%s*", e))
		}
	}
	args = append(args, src, dst)

	var lastErr error
	var lastOut string
	for attempt := 1; attempt <= m.cfg.Retries; attempt++ {
		out, err := m.ossutilRun(ctx, args...)
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

// ProbeRemote checks if a given OSS URI exists and counts its objects.
func (m *Migrator) ProbeRemote(ctx context.Context, uri string) (RemoteCheckResult, error) {
	uri = normalizeOSSUri(uri)
	res := RemoteCheckResult{URI: uri}
	if err := m.ValidateProbeScope(uri); err != nil {
		res.Error = err.Error()
		return res, err
	}
	state, err := m.remoteState(ctx, uri+"/")
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	if state == remoteHasData {
		res.Exists = true
		res.ObjectCount = m.remoteCount(ctx, uri+"/", 1000)
	}
	return res, nil
}

// ListOSSWorkspaces queries the OSS bucket/prefix for workspace subdirectories (e.g. ws-xxx).
// If search is provided, it applies prefix filtering on the OSS server side to avoid scanning
// large buckets (e.g. 100k+ workspaces). limit caps the number of entries returned.
func (m *Migrator) ListOSSWorkspaces(ctx context.Context, bucket, prefix, search string, limit int) ([]OSSWorkspaceEntry, error) {
	if bucket == "" {
		bucket = m.cfg.SourceBucket
	}
	bucket = normalizeOSSUri(bucket)
	if prefix == "" {
		prefix = m.cfg.SourcePrefix
	}
	prefix = strings.Trim(prefix, "/")
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	// Strict directory scope check: only allowed source or target bucket & prefix are permitted
	isSourceScope := (bucket == m.cfg.SourceBucket && prefix == m.cfg.SourcePrefix)
	isTargetScope := (bucket == m.cfg.TargetBucket && prefix == m.cfg.TargetPrefix)
	if !isSourceScope && !isTargetScope {
		return nil, fmt.Errorf("操作被禁止：仅允许检索受控的工作空间目录 (%s/%s)，禁止访问其他目录", m.cfg.SourceBucket, m.cfg.SourcePrefix)
	}

	if strings.Contains(search, "..") {
		return nil, fmt.Errorf("非法搜索参数：禁止包含 .. 相对路径")
	}

	search = strings.TrimSpace(search)
	var queryURI string
	if search != "" {
		if strings.HasPrefix(search, "oss://") {
			queryURI = search
		} else {
			queryURI = fmt.Sprintf("%s/%s/%s", bucket, prefix, strings.TrimPrefix(search, "/"))
		}
	} else {
		queryURI = fmt.Sprintf("%s/%s/", bucket, prefix)
	}

	limitStr := strconv.Itoa(limit)
	// Try -d (directory only) + --limited-num first for fast server-side listing
	out, err := m.ossutilCapture(ctx, "ls", "-d", "--limited-num", limitStr, queryURI)
	if err != nil || (!strings.Contains(out, "oss://")) {
		// Fallback to standard ls with limit
		out, err = m.ossutilCapture(ctx, "ls", "--limited-num", limitStr, queryURI)
	}
	if err != nil {
		if isNotExistErr(out) {
			return []OSSWorkspaceEntry{}, nil
		}
		return nil, fmt.Errorf("ossutil ls %s: %w (%s)", queryURI, err, tailLines(out, 5))
	}

	basePrefix := fmt.Sprintf("%s/%s/", bucket, prefix)
	seen := make(map[string]bool)
	var entries []OSSWorkspaceEntry

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		idx := strings.Index(line, "oss://")
		if idx == -1 {
			continue
		}
		rawURI := line[idx:]
		if !strings.HasPrefix(rawURI, basePrefix) {
			// Might be queryURI itself
			if strings.HasPrefix(rawURI, strings.TrimSuffix(basePrefix, "/")) {
				rawURI = strings.TrimPrefix(rawURI, strings.TrimSuffix(basePrefix, "/"))
				rawURI = strings.TrimPrefix(rawURI, "/")
			} else {
				continue
			}
		} else {
			rawURI = strings.TrimPrefix(rawURI, basePrefix)
		}

		parts := strings.Split(rawURI, "/")
		if len(parts) == 0 || parts[0] == "" {
			continue
		}
		wsName := parts[0]
		// 只要看 ws-xxxx 的信息：只保留以 ws- 开头的工作空间目录
		if !strings.HasPrefix(wsName, "ws-") {
			continue
		}
		// 若提供搜索词，支持包含匹配（如搜 974361 能命中 ws-9743616）
		if search != "" && !strings.Contains(wsName, search) && !strings.Contains(strings.TrimPrefix(wsName, "ws-"), search) {
			continue
		}

		if !seen[wsName] && wsName != "." && wsName != ".." {
			seen[wsName] = true
			srcURI := fmt.Sprintf("%s/%s/%s", bucket, prefix, wsName)
			dstURI := fmt.Sprintf("%s/%s/%s", m.cfg.TargetBucket, m.cfg.TargetPrefix, wsName)
			entries = append(entries, OSSWorkspaceEntry{
				Name:      wsName,
				SourceURI: srcURI,
				TargetURI: dstURI,
			})
			if len(entries) >= limit {
				break
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// remoteState probes whether an OSS prefix holds real objects.
func (m *Migrator) remoteState(ctx context.Context, prefix string) (remoteStateResult, error) {
	out, err := m.ossutilCapture(ctx, "ls", prefix)
	if err != nil {
		if isNotExistErr(out) {
			return remoteEmpty, nil
		}
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

// remoteCount counts objects up to cap in the remote prefix.
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

func isNotExistErr(out string) bool {
	low := strings.ToLower(out)
	for _, s := range []string{"nosuchkey", "nosuchbucket", "no such key", "not exist", "does not exist", "404"} {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// ossutilRun executes ossutil and returns combined stdout+stderr.
func (m *Migrator) ossutilRun(ctx context.Context, args ...string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	bin := m.cfg.OSSUtilPath
	if bin == "" {
		bin = "ossutil"
	}
	fullArgs := append([]string{}, m.cfg.ExtraArgs...)
	fullArgs = append(fullArgs, args...)

	cmd := exec.CommandContext(ctx, bin, fullArgs...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ossutilCapture runs ossutil in capture mode.
func (m *Migrator) ossutilCapture(ctx context.Context, args ...string) (string, error) {
	return m.ossutilRun(ctx, args...)
}

// logWrite records a completed migration to the TSV audit log file.
func (m *Migrator) logWrite(r MigrateResult) {
	if m.cfg.LogFile == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(m.cfg.LogFile), 0o755)
	f, err := os.OpenFile(m.cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	now := time.Now().Format(time.RFC3339)
	line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
		now,
		r.Status,
		r.Source,
		r.Destination,
		strings.Join(r.Synced, ","),
		r.Size,
		r.TargetObjects,
		r.Elapsed,
		strings.ReplaceAll(r.Note, "\t", " "),
	)
	_, _ = f.WriteString(line)
}

// readLogTSV reads the TSV log file and returns recent LogRows.
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
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 8 {
			continue
		}
		row := LogRow{
			Timestamp:   parts[0],
			Status:      parts[1],
			Source:      parts[2],
			Destination: parts[3],
			Dirs:        parts[4],
			Size:        parts[5],
			Files:       parts[6],
			Elapsed:     parts[7],
		}
		if len(parts) >= 9 {
			row.Note = parts[8]
		}
		rows = append(rows, row)
	}
	// Reverse so latest logs appear first
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	if len(rows) > 1000 {
		rows = rows[:1000]
	}
	return rows, scanner.Err()
}
