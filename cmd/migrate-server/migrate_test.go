package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeFakeOSSUTIL writes a fake ossutil binary that records invocations and
// simulates a remote object store backed by a directory tree controlled by
// FAKE_REMOTE_ROOT. Behavior:
//   - `ls <prefix>`: prints object lines if remote tree has files, else
//     "Object Number is: 0"
//   - `cp ... src dst`: creates marker files under FAKE_REMOTE_ROOT
func makeFakeOSSUTIL(t *testing.T) (bin string, remoteRoot string) {
	t.Helper()
	dir := t.TempDir()
	remoteRoot = filepath.Join(dir, "remote")
	_ = os.MkdirAll(remoteRoot, 0o755)
	bin = filepath.Join(dir, "ossutil")
	script := `#!/usr/bin/env bash
set -u
REMOTE="${FAKE_REMOTE_ROOT:?}"
sub=$1; shift
# strip options we care about
args=()
for a in "$@"; do args+=("$a"); done
if [ "$sub" = "ls" ]; then
  prefix="${args[0]}"
  # map oss://bucket/prefix/ to REMOTE/prefix
  key=$(printf '%s' "$prefix" | sed -E 's#^oss://[^/]+/##; s#/$##')
  target="$REMOTE/$key"
  found=0
  if [ -d "$target" ]; then
    while IFS= read -r f; do
      rel=${f#$REMOTE/}
      echo "  Standard  ETAG=abc  oss://bucket/$rel"
      found=1
    done < <(find "$target" -type f 2>/dev/null)
  fi
  if [ "$found" = 0 ]; then echo "Object Number is: 0"; fi
  exit 0
fi
if [ "$sub" = "cp" ]; then
  # last two non-flag args are src dst
  src=""; dst=""
  i=0
  pos=()
  for a in "${args[@]}"; do
    case "$a" in
      -r|-f) ;;
      *) pos+=("$a") ;;
    esac
  done
  src="${pos[0]}"; dst="${pos[1]}"
  key=$(printf '%s' "$dst" | sed -E 's#^oss://[^/]+/##; s#/$##')
  tgt="$REMOTE/$key"
  if [ -d "$src" ] || [ -d "${src%/}" ]; then
    mkdir -p "$tgt"
    # copy: for each file under src, write a marker preserving its relative path
    while IFS= read -r f; do
      rel=${f#"$src"/}
      mkdir -p "$tgt/$(dirname "$rel")" 2>/dev/null || true
      : > "$tgt/$rel"
    done < <(find "$src" -type f 2>/dev/null)
  else
    mkdir -p "$(dirname "$tgt")"
    echo x > "$tgt"
  fi
  exit 0
fi
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, remoteRoot
}

func setupLocalWorkspace(t *testing.T) (root, wsID string) {
	t.Helper()
	root = t.TempDir()
	wsID = "9660946"
	pvc := filepath.Join(root, "pvc-abc_bocomwork_ws-"+wsID+"-pvc")
	for _, d := range []string{"workspace", "bocomwork", "bocomcode"} {
		_ = os.MkdirAll(filepath.Join(pvc, d), 0o755)
	}
	_ = os.WriteFile(filepath.Join(pvc, "workspace", "a.txt"), []byte("hello"), 0o644)
	_ = os.WriteFile(filepath.Join(pvc, "bocomwork", "b.txt"), []byte("world"), 0o644)
	// excluded builtin dir with a file
	excl := filepath.Join(pvc, "bocomwork", "server", "skills", "builtin", "x.go")
	_ = os.MkdirAll(filepath.Dir(excl), 0o755)
	_ = os.WriteFile(excl, []byte("builtin"), 0o644)
	// sibling of excluded branch that SHOULD be uploaded
	sib := filepath.Join(pvc, "bocomwork", "server", "custom", "y.go")
	_ = os.MkdirAll(filepath.Dir(sib), 0o755)
	_ = os.WriteFile(sib, []byte("custom"), 0o644)
	return root, wsID
}

func TestDirToID(t *testing.T) {
	cases := map[string]string{
		"pvc-abc123_bocomwork_ws-9660946-pvc":                                     "9660946",
		"pvc-uuid_ns_ws-08-pvc":                                                   "08",
		"pvc-uuid_ns_ws-9743616-guwpb-pvc":                                        "9743616-guwpb",
		"pvc-4fcb6a0f-4922-43d6-93a7-df6fa7e25aac_bocomwork_ws-9660946-guwpb-pvc": "9660946-guwpb",
	}
	for in, want := range cases {
		got, ok := DirToID(in)
		if !ok || got != want {
			t.Errorf("DirToID(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	if _, ok := DirToID("not-a-pvc"); ok {
		t.Error("expected non-pvc dir to be rejected")
	}
}

func TestResolveExcludes(t *testing.T) {
	if got := ResolveExcludes(""); len(got) != 1 || got[0] != DefaultExcludes {
		t.Errorf("default excludes = %v", got)
	}
	if got := ResolveExcludes("none"); len(got) != 0 {
		t.Errorf("none excludes = %v", got)
	}
	if got := ResolveExcludes("a/b, c/d"); len(got) != 2 {
		t.Errorf("csv excludes = %v", got)
	}
}

func TestListLocal(t *testing.T) {
	root, wsID := setupLocalWorkspace(t)
	m, err := NewMigrator(MigrateConfig{
		Root: root, OSSBucket: "oss://bucket", OSSPrefix: "workspaces",
		Dirs: splitList(DefaultDirs), Excludes: ResolveExcludes(""),
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := m.ListLocal()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != wsID {
		t.Fatalf("ListLocal = %+v", list)
	}
	if list[0].Files != 3 { // a.txt, b.txt, custom/y.go (builtin excluded)
		t.Errorf("files = %d want 3", list[0].Files)
	}
}

// TestMigrateOneSuffixedWorkspace guards the -guwpb style names: the suffix
// must be kept and the OSS target must be ws-<full-name>, not the bare number.
func TestMigrateOneSuffixedWorkspace(t *testing.T) {
	bin, remoteRoot := makeFakeOSSUTIL(t)
	t.Setenv("FAKE_REMOTE_ROOT", remoteRoot)
	root := t.TempDir()
	pvc := filepath.Join(root, "pvc-4fcb6a0f-4922-43d6-93a7-df6fa7e25aac_bocomwork_ws-9660946-guwpb-pvc")
	for _, d := range []string{"workspace", "bocomwork", "bocomcode"} {
		_ = os.MkdirAll(filepath.Join(pvc, d), 0o755)
	}
	_ = os.WriteFile(filepath.Join(pvc, "workspace", "a.txt"), []byte("hi"), 0o644)

	m, _ := NewMigrator(MigrateConfig{
		Root: root, OSSBucket: "oss://bucket", OSSPrefix: "workspaces",
		Dirs: splitList(DefaultDirs), Excludes: ResolveExcludes(""), OSSUtilPath: bin, Retries: 1,
	})
	list, _ := m.ListLocal()
	if len(list) != 1 || list[0].ID != "9660946-guwpb" || list[0].Name != "ws-9660946-guwpb" {
		t.Fatalf("ListLocal = %+v", list)
	}
	res := m.MigrateOne(context.Background(), list[0])
	if res.Status != "OK" {
		t.Fatalf("migrate status = %s note=%s", res.Status, res.Note)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "workspaces/ws-9660946-guwpb/workspace/a.txt")); err != nil {
		t.Errorf("expected upload to ws-9660946-guwpb (suffix kept): %v", err)
	}
}

func TestMigrateOneFresh(t *testing.T) {
	bin, remoteRoot := makeFakeOSSUTIL(t)
	t.Setenv("FAKE_REMOTE_ROOT", remoteRoot)
	root, _ := setupLocalWorkspace(t)
	m, _ := NewMigrator(MigrateConfig{
		Root: root, OSSBucket: "oss://bucket", OSSPrefix: "workspaces",
		Dirs: splitList(DefaultDirs), Excludes: ResolveExcludes(""),
		OSSUtilPath: bin, Retries: 1,
	})
	ws, err := m.ResolveLocal("9660946")
	if err != nil || ws == nil {
		t.Fatalf("resolve: %v %v", ws, err)
	}
	res := m.MigrateOne(context.Background(), *ws)
	if res.Status != "OK" {
		t.Fatalf("status = %s note=%s", res.Status, res.Note)
	}
	// workspace/a.txt should be uploaded
	if _, err := os.Stat(filepath.Join(remoteRoot, "workspaces/ws-9660946/workspace/a.txt")); err != nil {
		t.Errorf("expected uploaded workspace/a.txt: %v", err)
	}
	// excluded builtin must NOT be uploaded
	if _, err := os.Stat(filepath.Join(remoteRoot, "workspaces/ws-9660946/bocomwork/server/skills/builtin/x.go")); err == nil {
		t.Error("excluded builtin was uploaded")
	}
	// sibling of excluded branch must be uploaded
	if _, err := os.Stat(filepath.Join(remoteRoot, "workspaces/ws-9660946/bocomwork/server/custom/y.go")); err != nil {
		t.Errorf("expected sibling custom/y.go uploaded: %v", err)
	}
}

func TestMigrateOneSkipsWhenRemoteHasData(t *testing.T) {
	bin, remoteRoot := makeFakeOSSUTIL(t)
	t.Setenv("FAKE_REMOTE_ROOT", remoteRoot)
	root, _ := setupLocalWorkspace(t)
	// pre-seed remote with a real object
	_ = os.MkdirAll(filepath.Join(remoteRoot, "workspaces/ws-9660946/workspace"), 0o755)
	_ = os.WriteFile(filepath.Join(remoteRoot, "workspaces/ws-9660946/workspace/existing.txt"), []byte("x"), 0o644)

	m, _ := NewMigrator(MigrateConfig{
		Root: root, OSSBucket: "oss://bucket", OSSPrefix: "workspaces",
		Dirs: splitList(DefaultDirs), Excludes: ResolveExcludes(""), OSSUtilPath: bin, Retries: 1,
	})
	ws, _ := m.ResolveLocal("9660946")
	res := m.MigrateOne(context.Background(), *ws)
	if res.Status != "SKIP" {
		t.Fatalf("expected SKIP, got %s (%s)", res.Status, res.Note)
	}
	// nothing new uploaded
	if _, err := os.Stat(filepath.Join(remoteRoot, "workspaces/ws-9660946/workspace/a.txt")); err == nil {
		t.Error("should not have uploaded on skip")
	}
}

func TestMigrateOneForceOverwrites(t *testing.T) {
	bin, remoteRoot := makeFakeOSSUTIL(t)
	t.Setenv("FAKE_REMOTE_ROOT", remoteRoot)
	root, _ := setupLocalWorkspace(t)
	_ = os.MkdirAll(filepath.Join(remoteRoot, "workspaces/ws-9660946/workspace"), 0o755)
	_ = os.WriteFile(filepath.Join(remoteRoot, "workspaces/ws-9660946/workspace/existing.txt"), []byte("x"), 0o644)

	m, _ := NewMigrator(MigrateConfig{
		Root: root, OSSBucket: "oss://bucket", OSSPrefix: "workspaces",
		Dirs: splitList(DefaultDirs), Excludes: ResolveExcludes(""), OSSUtilPath: bin, Retries: 1, Force: true,
	})
	ws, _ := m.ResolveLocal("9660946")
	res := m.MigrateOne(context.Background(), *ws)
	if res.Status != "OK" {
		t.Fatalf("force expected OK, got %s (%s)", res.Status, res.Note)
	}
	if _, err := os.Stat(filepath.Join(remoteRoot, "workspaces/ws-9660946/workspace/a.txt")); err != nil {
		t.Error("force should have uploaded a.txt")
	}
}

func TestMigrateOneUnknownRemoteFails(t *testing.T) {
	// fake ossutil that always errors on ls
	dir := t.TempDir()
	bin := filepath.Join(dir, "ossutil")
	_ = os.WriteFile(bin, []byte("#!/usr/bin/env bash\necho 'AccessDenied: permission' >&2\nexit 1\n"), 0o755)
	root, _ := setupLocalWorkspace(t)
	m, _ := NewMigrator(MigrateConfig{
		Root: root, OSSBucket: "oss://bucket", OSSPrefix: "workspaces",
		Dirs: splitList(DefaultDirs), Excludes: ResolveExcludes(""), OSSUtilPath: bin, Retries: 1,
	})
	ws, _ := m.ResolveLocal("9660946")
	res := m.MigrateOne(context.Background(), *ws)
	if res.Status != "FAIL" {
		t.Fatalf("unknown remote state must FAIL, got %s", res.Status)
	}
	if !strings.Contains(res.Note, "cannot confirm") {
		t.Errorf("note = %q", res.Note)
	}
	// The ossutil diagnostic (not just "exit status 1") must be surfaced so the
	// failure is debuggable.
	if !strings.Contains(res.Note, "AccessDenied") {
		t.Errorf("note should include ossutil output, got %q", res.Note)
	}
}

func TestNormalizeID(t *testing.T) {
	cases := map[string]string{
		"9660946":                           "9660946",
		"ws-9660946":                        "9660946",
		"ws-9660946-pvc":                    "9660946",
		"pvc-uuid_bocomwork_ws-9660946-pvc": "9660946",
		"/data/local-path-storage/pvc-u_b_ws-08-pvc/": "08",
	}
	for in, want := range cases {
		if got := normalizeID(in); got != want {
			t.Errorf("normalizeID(%q)=%q want %q", in, got, want)
		}
	}
}
