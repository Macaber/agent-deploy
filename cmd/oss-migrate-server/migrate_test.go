package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeFakeOSSUtil creates a fake ossutil script that operates against a simulated OSS directory tree.
func makeFakeOSSUtil(t *testing.T) (binPath string, fakeOSSRoot string) {
	t.Helper()
	dir := t.TempDir()
	fakeOSSRoot = filepath.Join(dir, "fake_oss")
	_ = os.MkdirAll(fakeOSSRoot, 0o755)

	binPath = filepath.Join(dir, "ossutil")
	script := `#!/usr/bin/env bash
set -eu
ROOT="${FAKE_OSS_ROOT:?}"
sub="$1"; shift

uri_to_path() {
  local uri="$1"
  # strip oss://
  local clean="${uri#oss://}"
  clean="${clean%/}"
  echo "$ROOT/$clean"
}

if [ "$sub" = "ls" ]; then
  prefix=""
  for a in "$@"; do
    case "$a" in
      -*) ;;
      *) prefix="$a" ;;
    esac
  done
  
  target=$(uri_to_path "$prefix")
  found=0
  if [ -d "$target" ]; then
    while IFS= read -r f; do
      if [ -f "$f" ]; then
        rel=${f#$ROOT/}
        echo "2026-11-23 16:00:00 1024 Standard ETAG_123 oss://$rel"
        found=1
      elif [ -d "$f" ]; then
        rel=${f#$ROOT/}
        echo "                             oss://$rel/"
      fi
    done < <(find "$target" -mindepth 1 2>/dev/null)
  fi
  if [ "$found" = 0 ]; then
    echo "Object Number is: 0"
  fi
  exit 0
fi

if [ "$sub" = "cp" ]; then
  pos=()
  excludes=()
  force=0
  skip_next=0

  for ((i=1; i<=$#; i++)); do
    if [ "$skip_next" -eq 1 ]; then
      skip_next=0
      continue
    fi
    arg="${!i}"
    case "$arg" in
      -r) ;;
      -f) force=1 ;;
      --exclude)
        j=$((i+1))
        excludes+=("${!j}")
        skip_next=1
        ;;
      *)
        pos+=("$arg")
        ;;
    esac
  done

  src="${pos[0]}"
  dst="${pos[1]}"
  srcPath=$(uri_to_path "$src")
  dstPath=$(uri_to_path "$dst")

  if [ ! -e "$srcPath" ]; then
    echo "Error: source not found: $srcPath" >&2
    exit 1
  fi

  # Copy files
  if [ -d "$srcPath" ]; then
    mkdir -p "$dstPath"
    while IFS= read -r f; do
      rel=${f#$srcPath/}
      
      # Check excludes
      excluded=0
      if [ ${#excludes[@]} -gt 0 ]; then
        for pat in "${excludes[@]}"; do
          cleanPat=$(echo "$pat" | tr -d '*')
          if [[ "$rel" == *"$cleanPat"* ]]; then
            excluded=1
            break
          fi
        done
      fi
      
      if [ "$excluded" -eq 0 ]; then
        destFile="$dstPath/$rel"
        mkdir -p "$(dirname "$destFile")"
        cp "$f" "$destFile"
      fi
    done < <(find "$srcPath" -type f 2>/dev/null)
  else
    mkdir -p "$(dirname "$dstPath")"
    cp "$srcPath" "$dstPath"
  fi
  exit 0
fi

exit 0
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binPath, fakeOSSRoot
}

func TestNormalizeConfig(t *testing.T) {
	cfg := MigrateConfig{}
	if err := NormalizeConfig(&cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SourceBucket != DefaultSourceBucket {
		t.Errorf("expected %s, got %s", DefaultSourceBucket, cfg.SourceBucket)
	}
	if cfg.TargetBucket != DefaultTargetBucket {
		t.Errorf("expected %s, got %s", DefaultTargetBucket, cfg.TargetBucket)
	}
	if len(cfg.Dirs) != 3 {
		t.Errorf("expected 3 default dirs, got %d", len(cfg.Dirs))
	}
}

func TestResolveExcludes(t *testing.T) {
	ex := ResolveExcludes("bocomwork/server/skills/builtin, custom/temp/")
	if len(ex) != 2 || ex[0] != "bocomwork/server/skills/builtin" || ex[1] != "custom/temp" {
		t.Errorf("unexpected excludes: %v", ex)
	}

	none := ResolveExcludes("none")
	if len(none) != 0 {
		t.Errorf("expected empty for 'none', got %v", none)
	}

	empty := ResolveExcludes("")
	if len(empty) != 0 {
		t.Errorf("expected empty for empty spec, got %v", empty)
	}
}

func TestNormalizeOSSUri(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"oss://euvduatc-oss/workspaces/ws-1/", "oss://euvduatc-oss/workspaces/ws-1"},
		{"euvduatc-oss/workspaces/ws-2", "oss://euvduatc-oss/workspaces/ws-2"},
		{"  oss://bucket/p/  ", "oss://bucket/p"},
	}
	for _, tt := range tests {
		got := normalizeOSSUri(tt.in)
		if got != tt.want {
			t.Errorf("normalizeOSSUri(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMigrateOne_SkipWhenTargetExists(t *testing.T) {
	bin, fakeRoot := makeFakeOSSUtil(t)
	t.Setenv("FAKE_OSS_ROOT", fakeRoot)

	// Create source files
	srcDir := filepath.Join(fakeRoot, "euvduatc-oss", "workspaces", "ws-9743616", "workspace")
	_ = os.MkdirAll(srcDir, 0o755)
	_ = os.WriteFile(filepath.Join(srcDir, "code.py"), []byte("print(1)"), 0o644)

	// Create target files (target already has data!)
	dstDir := filepath.Join(fakeRoot, "euvduatc-oss", "workspaces", "ws-974361-guwnb", "workspace")
	_ = os.MkdirAll(dstDir, 0o755)
	_ = os.WriteFile(filepath.Join(dstDir, "existing.txt"), []byte("old"), 0o644)

	cfg := MigrateConfig{
		OSSUtilPath: bin,
		LogFile:     filepath.Join(t.TempDir(), "test.log"),
	}
	mig, err := NewMigrator(cfg)
	if err != nil {
		t.Fatal(err)
	}

	item := MigrationItem{
		Source:      "oss://euvduatc-oss/workspaces/ws-9743616",
		Destination: "oss://euvduatc-oss/workspaces/ws-974361-guwnb",
		Force:       false, // Do not force
		DryRun:      false,
	}

	res := mig.MigrateOne(context.Background(), item)
	if res.Status != "SKIP" {
		t.Fatalf("expected SKIP when target exists and force=false, got %s (note: %s)", res.Status, res.Note)
	}
	if !strings.Contains(res.Note, "use force to overwrite") {
		t.Errorf("expected skip note to mention force, got: %s", res.Note)
	}

	// Verify target was NOT overwritten
	if _, err := os.Stat(filepath.Join(dstDir, "code.py")); !os.IsNotExist(err) {
		t.Errorf("code.py should not have been copied to target on SKIP")
	}
}

func TestMigrateOne_ForceOverwrite(t *testing.T) {
	bin, fakeRoot := makeFakeOSSUtil(t)
	t.Setenv("FAKE_OSS_ROOT", fakeRoot)

	// Create source files
	srcDir := filepath.Join(fakeRoot, "euvduatc-oss", "workspaces", "ws-9743616", "workspace")
	_ = os.MkdirAll(srcDir, 0o755)
	_ = os.WriteFile(filepath.Join(srcDir, "code.py"), []byte("print('new')"), 0o644)

	// Create target files (target already has data)
	dstDir := filepath.Join(fakeRoot, "euvduatc-oss", "workspaces", "ws-974361-guwnb", "workspace")
	_ = os.MkdirAll(dstDir, 0o755)
	_ = os.WriteFile(filepath.Join(dstDir, "old.txt"), []byte("old"), 0o644)

	cfg := MigrateConfig{
		OSSUtilPath: bin,
		LogFile:     filepath.Join(t.TempDir(), "test.log"),
	}
	mig, err := NewMigrator(cfg)
	if err != nil {
		t.Fatal(err)
	}

	item := MigrationItem{
		Source:      "oss://euvduatc-oss/workspaces/ws-9743616",
		Destination: "oss://euvduatc-oss/workspaces/ws-974361-guwnb",
		Force:       true, // Force enabled!
		DryRun:      false,
	}

	res := mig.MigrateOne(context.Background(), item)
	if res.Status != "OK" {
		t.Fatalf("expected OK when force=true, got %s (note: %s)", res.Status, res.Note)
	}

	// Verify target now has the new file!
	copiedFile := filepath.Join(dstDir, "code.py")
	b, err := os.ReadFile(copiedFile)
	if err != nil {
		t.Fatalf("failed to read copied file: %v", err)
	}
	if string(b) != "print('new')" {
		t.Errorf("content mismatch, got: %s", string(b))
	}
}

func TestMigrateOne_NormalWhenTargetEmpty(t *testing.T) {
	bin, fakeRoot := makeFakeOSSUtil(t)
	t.Setenv("FAKE_OSS_ROOT", fakeRoot)

	// Create source files
	srcDir := filepath.Join(fakeRoot, "euvduatc-oss", "workspaces", "ws-9743616", "workspace")
	_ = os.MkdirAll(srcDir, 0o755)
	_ = os.WriteFile(filepath.Join(srcDir, "main.go"), []byte("package main"), 0o644)

	cfg := MigrateConfig{
		OSSUtilPath: bin,
		LogFile:     filepath.Join(t.TempDir(), "test.log"),
	}
	mig, err := NewMigrator(cfg)
	if err != nil {
		t.Fatal(err)
	}

	item := MigrationItem{
		Source:      "oss://euvduatc-oss/workspaces/ws-9743616",
		Destination: "oss://euvduatc-oss/workspaces/ws-974361-guwnb",
		Force:       false,
		DryRun:      false,
	}

	res := mig.MigrateOne(context.Background(), item)
	if res.Status != "OK" {
		t.Fatalf("expected OK for empty target, got %s (note: %s)", res.Status, res.Note)
	}

	// Check copied
	dstFile := filepath.Join(fakeRoot, "euvduatc-oss", "workspaces", "ws-974361-guwnb", "workspace", "main.go")
	if _, err := os.Stat(dstFile); err != nil {
		t.Fatalf("expected file to exist at %s: %v", dstFile, err)
	}
}

func TestMigrateOne_DryRun(t *testing.T) {
	bin, fakeRoot := makeFakeOSSUtil(t)
	t.Setenv("FAKE_OSS_ROOT", fakeRoot)

	// Create source files
	srcDir := filepath.Join(fakeRoot, "euvduatc-oss", "workspaces", "ws-9743616", "workspace")
	_ = os.MkdirAll(srcDir, 0o755)
	_ = os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("aaa"), 0o644)

	cfg := MigrateConfig{
		OSSUtilPath: bin,
		LogFile:     filepath.Join(t.TempDir(), "test.log"),
	}
	mig, err := NewMigrator(cfg)
	if err != nil {
		t.Fatal(err)
	}

	item := MigrationItem{
		Source:      "oss://euvduatc-oss/workspaces/ws-9743616",
		Destination: "oss://euvduatc-oss/workspaces/ws-974361-guwnb",
		DryRun:      true,
	}

	res := mig.MigrateOne(context.Background(), item)
	if res.Status != "OK" || !strings.Contains(res.Note, "dry-run") {
		t.Fatalf("expected OK dry-run, got %s (note: %s)", res.Status, res.Note)
	}

	// Target should NOT have been created
	dstDir := filepath.Join(fakeRoot, "euvduatc-oss", "workspaces", "ws-974361-guwnb")
	if _, err := os.Stat(dstDir); !os.IsNotExist(err) {
		t.Errorf("destination should not exist on dry-run")
	}
}

func TestMigrateOne_SourceEmptyFails(t *testing.T) {
	bin, fakeRoot := makeFakeOSSUtil(t)
	t.Setenv("FAKE_OSS_ROOT", fakeRoot)

	cfg := MigrateConfig{
		OSSUtilPath: bin,
		LogFile:     filepath.Join(t.TempDir(), "test.log"),
	}
	mig, err := NewMigrator(cfg)
	if err != nil {
		t.Fatal(err)
	}

	item := MigrationItem{
		Source:      "oss://euvduatc-oss/workspaces/ws-nonexistent",
		Destination: "oss://euvduatc-oss/workspaces/ws-target",
	}

	res := mig.MigrateOne(context.Background(), item)
	if res.Status != "FAIL" {
		t.Fatalf("expected FAIL for nonexistent source, got %s", res.Status)
	}
}

func TestMigrateOne_WithSubdirsAndExcludes(t *testing.T) {
	bin, fakeRoot := makeFakeOSSUtil(t)
	t.Setenv("FAKE_OSS_ROOT", fakeRoot)

	wsSrc := filepath.Join(fakeRoot, "euvduatc-oss", "workspaces", "ws-9743616")
	_ = os.MkdirAll(filepath.Join(wsSrc, "workspace"), 0o755)
	_ = os.MkdirAll(filepath.Join(wsSrc, "bocomwork", "server", "skills", "builtin"), 0o755)
	_ = os.MkdirAll(filepath.Join(wsSrc, "bocomwork", "server", "custom"), 0o755)

	_ = os.WriteFile(filepath.Join(wsSrc, "workspace", "code.py"), []byte("code"), 0o644)
	_ = os.WriteFile(filepath.Join(wsSrc, "bocomwork", "server", "skills", "builtin", "tool.go"), []byte("builtin"), 0o644)
	_ = os.WriteFile(filepath.Join(wsSrc, "bocomwork", "server", "custom", "mytool.go"), []byte("custom"), 0o644)

	cfg := MigrateConfig{
		OSSUtilPath: bin,
		LogFile:     filepath.Join(t.TempDir(), "test.log"),
	}
	mig, err := NewMigrator(cfg)
	if err != nil {
		t.Fatal(err)
	}

	item := MigrationItem{
		Source:      "oss://euvduatc-oss/workspaces/ws-9743616",
		Destination: "oss://euvduatc-oss/workspaces/ws-974361-guwnb",
		Dirs:        []string{"workspace", "bocomwork"},
		Excludes:    []string{"bocomwork/server/skills/builtin"},
	}

	res := mig.MigrateOne(context.Background(), item)
	if res.Status != "OK" {
		t.Fatalf("expected OK, got %s (note: %s)", res.Status, res.Note)
	}

	dstBase := filepath.Join(fakeRoot, "euvduatc-oss", "workspaces", "ws-974361-guwnb")
	// workspace/code.py should exist
	if _, err := os.Stat(filepath.Join(dstBase, "workspace", "code.py")); err != nil {
		t.Errorf("expected workspace/code.py to be copied")
	}
	// bocomwork/server/custom/mytool.go should exist
	if _, err := os.Stat(filepath.Join(dstBase, "bocomwork", "server", "custom", "mytool.go")); err != nil {
		t.Errorf("expected custom tool to be copied")
	}
	// bocomwork/server/skills/builtin should NOT exist (excluded!)
	if _, err := os.Stat(filepath.Join(dstBase, "bocomwork", "server", "skills", "builtin", "tool.go")); !os.IsNotExist(err) {
		t.Errorf("builtin tool should have been excluded from migration")
	}
}

func TestListOSSWorkspaces(t *testing.T) {
	bin, fakeRoot := makeFakeOSSUtil(t)
	t.Setenv("FAKE_OSS_ROOT", fakeRoot)

	// Create multiple workspaces in fake OSS
	ws1 := filepath.Join(fakeRoot, "euvduatc-oss", "workspaces", "ws-9743616", "workspace")
	ws2 := filepath.Join(fakeRoot, "euvduatc-oss", "workspaces", "ws-974361-guwnb", "workspace")
	_ = os.MkdirAll(ws1, 0o755)
	_ = os.MkdirAll(ws2, 0o755)
	_ = os.WriteFile(filepath.Join(ws1, "a.txt"), []byte("1"), 0o644)
	_ = os.WriteFile(filepath.Join(ws2, "b.txt"), []byte("2"), 0o644)

	cfg := MigrateConfig{
		OSSUtilPath: bin,
		LogFile:     filepath.Join(t.TempDir(), "test.log"),
	}
	mig, err := NewMigrator(cfg)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := mig.ListOSSWorkspaces(context.Background(), "oss://euvduatc-oss", "workspaces", "", 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 entries, got %d", len(entries))
	}
	found9743616 := false
	for _, e := range entries {
		if e.Name == "ws-9743616" {
			found9743616 = true
			if e.SourceURI != "oss://euvduatc-oss/workspaces/ws-9743616" {
				t.Errorf("unexpected source URI: %s", e.SourceURI)
			}
		}
	}
	if !found9743616 {
		t.Errorf("ws-9743616 not found in entries")
	}
}

func TestReadLogTSV(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "audit.log")
	content := "2026-11-23T16:00:00Z\tOK\toss://src\toss://dst\tworkspace\t10KB\t5\t1.2s\tsynced successfully\n"
	_ = os.WriteFile(logFile, []byte(content), 0o644)

	rows, err := readLogTSV(logFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].Status != "OK" || rows[0].Source != "oss://src" || rows[0].Destination != "oss://dst" {
		t.Errorf("row mismatch: %+v", rows[0])
	}
}

func TestScopeRestriction(t *testing.T) {
	bin, fakeRoot := makeFakeOSSUtil(t)
	t.Setenv("FAKE_OSS_ROOT", fakeRoot)

	cfg := MigrateConfig{
		SourceBucket: "oss://euvduatc-oss",
		SourcePrefix: "workspaces",
		TargetBucket: "oss://euvduatc-oss",
		TargetPrefix: "workspaces",
		OSSUtilPath:  bin,
		LogFile:      filepath.Join(t.TempDir(), "test.log"),
	}
	mig, err := NewMigrator(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Valid scope
	if err := mig.ValidateScope("oss://euvduatc-oss/workspaces/ws-123", false); err != nil {
		t.Errorf("expected valid scope to pass, got %v", err)
	}

	// 2. Other directory outside workspaces
	if err := mig.ValidateScope("oss://euvduatc-oss/system/secrets", false); err == nil {
		t.Errorf("expected error for directory outside workspaces")
	}

	// 3. Other bucket
	if err := mig.ValidateScope("oss://other-bucket/workspaces/ws-123", true); err == nil {
		t.Errorf("expected error for unauthorized bucket")
	}

	// 4. Path traversal
	if err := mig.ValidateScope("oss://euvduatc-oss/workspaces/../system", false); err == nil {
		t.Errorf("expected error for path traversal ..")
	}

	// 5. Operating on root workspaces dir itself
	if err := mig.ValidateScope("oss://euvduatc-oss/workspaces", false); err == nil {
		t.Errorf("expected error when operating on root workspaces directory directly")
	}

	// 6. ListOSSWorkspaces outside workspaces
	if _, err := mig.ListOSSWorkspaces(context.Background(), "oss://euvduatc-oss", "system", "", 50); err == nil {
		t.Errorf("expected error when listing non-workspaces prefix")
	}

	// 7. ProbeRemote outside workspaces
	if _, err := mig.ProbeRemote(context.Background(), "oss://euvduatc-oss/finance/data"); err == nil {
		t.Errorf("expected error when probing outside workspaces")
	}

	// 8. MigrateOne fails when out of scope
	res := mig.MigrateOne(context.Background(), MigrationItem{
		Source:      "oss://euvduatc-oss/system/config",
		Destination: "oss://euvduatc-oss/workspaces/ws-target",
	})
	if res.Status != "FAIL" {
		t.Errorf("expected FAIL for out-of-scope source, got %s", res.Status)
	}
}
