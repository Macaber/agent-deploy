package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := aiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// buildLocalPathFixture creates a local-path PVC+PV+Workspace for ws-9660946.
func buildLocalPathFixture(t *testing.T, root string) (pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume, ws *aiv1alpha1.Workspace) {
	t.Helper()
	sc := localPathStorageClass
	pv = &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-9660946-pv"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: localPathProvisioner},
			},
		},
	}
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-9660946-pvc", Namespace: "bocomwork"},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &sc,
			VolumeName:       "ws-9660946-pv",
		},
	}
	ws = &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-9660946", Namespace: "bocomwork"},
		Status:     aiv1alpha1.WorkspaceStatus{PVCName: "ws-9660946-pvc"},
	}
	return
}

func localWs(t *testing.T, root string) Workspace {
	t.Helper()
	dir := filepath.Join(root, "pvc-abc_bocomwork_ws-9660946-pvc")
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workspace", "data.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Workspace{ID: "9660946", Name: "ws-9660946", Dir: dir, Subdirs: []string{"workspace"}}
}

// TestCleanupRefusesWithoutEvidence is the regression test for the data-loss
// hole: with no migration log and no OSS objects, cleanup MUST refuse and MUST
// NOT delete the Workspace/PVC/PV, and MUST NOT remove the local dir.
func TestCleanupRefusesWithoutEvidence(t *testing.T) {
	root := t.TempDir()
	pvc, pv, wsCR := buildLocalPathFixture(t, root)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pvc, pv, wsCR).Build()
	ws := localWs(t, root)

	// mig = nil, empty log path -> no evidence at all.
	cleaner := NewCleaner(cl, root, false, nil, "")
	res := cleaner.Clean(context.Background(), ws)

	if res.Status != "FAIL" {
		t.Fatalf("expected FAIL, got %s", res.Status)
	}
	if res.Verified {
		t.Error("must not be marked verified")
	}
	// Nothing deleted.
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "ws-9660946", Namespace: "bocomwork"}, &aiv1alpha1.Workspace{}); err != nil {
		t.Errorf("workspace CR was deleted: %v", err)
	}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "ws-9660946-pvc", Namespace: "bocomwork"}, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Errorf("PVC was deleted: %v", err)
	}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "ws-9660946-pv"}, &corev1.PersistentVolume{}); err != nil {
		t.Errorf("PV was deleted: %v", err)
	}
	// Local dir kept.
	if _, err := os.Stat(filepath.Join(ws.Dir, "workspace", "data.txt")); err != nil {
		t.Errorf("local data removed: %v", err)
	}
}

// TestCleanupWithLogEvidenceDeletesK8sKeepsDir: log has status=OK -> verified;
// CR/PVC/PV deleted, local dir kept.
func TestCleanupWithLogEvidenceDeletesK8sKeepsDir(t *testing.T) {
	root := t.TempDir()
	pvc, pv, wsCR := buildLocalPathFixture(t, root)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pvc, pv, wsCR).Build()
	ws := localWs(t, root)

	logPath := filepath.Join(t.TempDir(), "oss-migrate.log")
	os.WriteFile(logPath, []byte("# time\tstatus\tworkspace\tlocal_dir\tremote_prefix\tdirs\tsize\tfiles\tnote\n"+
		"t\tOK\tws-9660946\t/dir\toss://b/workspaces/ws-9660946\tworkspace\t1K\t1\tremote_objects=1\n"), 0o644)

	cleaner := NewCleaner(cl, root, false, nil, logPath)
	res := cleaner.Clean(context.Background(), ws)

	if res.Status != "OK" || !res.Verified {
		t.Fatalf("expected OK/verified, got %s verified=%v err=%s", res.Status, res.Verified, res.Error)
	}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "ws-9660946", Namespace: "bocomwork"}, &aiv1alpha1.Workspace{}); !apierrors.IsNotFound(err) {
		t.Errorf("workspace CR should be deleted, got err=%v", err)
	}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "ws-9660946-pvc", Namespace: "bocomwork"}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Errorf("PVC should be deleted, got err=%v", err)
	}
	// local dir MUST remain (we never RemoveAll).
	if _, err := os.Stat(filepath.Join(ws.Dir, "workspace", "data.txt")); err != nil {
		t.Errorf("local data must be kept, but was removed: %v", err)
	}
}

// TestCleanupRefusesNonLocalPathPVC: even with log evidence, a non-local-path
// PVC must be refused (untouched).
func TestCleanupRefusesNonLocalPathPVC(t *testing.T) {
	root := t.TempDir()
	pvc, pv, wsCR := buildLocalPathFixture(t, root)
	other := "alicloud-oss"
	pvc.Spec.StorageClassName = &other
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pvc, pv, wsCR).Build()
	ws := localWs(t, root)

	logPath := filepath.Join(t.TempDir(), "log")
	os.WriteFile(logPath, []byte("t\tOK\tws-9660946\t/d\t/r\tworkspace\t1K\t1\tx\n"), 0o644)

	res := NewCleaner(cl, root, false, nil, logPath).Clean(context.Background(), ws)
	if res.Status != "FAIL" {
		t.Fatalf("non-local-path PVC must be refused, got %s", res.Status)
	}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "ws-9660946-pvc", Namespace: "bocomwork"}, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Errorf("PVC was deleted despite refusal: %v", err)
	}
}

// TestCleanupRemoteProbeEvidence: no log, but OSS prefix has objects -> verified.
func TestCleanupRemoteProbeEvidence(t *testing.T) {
	root := t.TempDir()
	pvc, pv, wsCR := buildLocalPathFixture(t, root)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pvc, pv, wsCR).Build()
	ws := localWs(t, root)

	bin, remoteRoot := makeFakeOSSUTIL(t)
	t.Setenv("FAKE_REMOTE_ROOT", remoteRoot)
	// seed remote objects for ws-9660946
	os.MkdirAll(filepath.Join(remoteRoot, "workspaces/ws-9660946/workspace"), 0o755)
	os.WriteFile(filepath.Join(remoteRoot, "workspaces/ws-9660946/workspace/x.txt"), []byte("x"), 0o644)

	mig, _ := NewMigrator(MigrateConfig{
		Root: root, OSSBucket: "oss://bucket", OSSPrefix: "workspaces",
		Dirs: splitList(DefaultDirs), Excludes: ResolveExcludes(""), OSSUtilPath: bin, Retries: 1,
	})
	res := NewCleaner(cl, root, false, mig, "").Clean(context.Background(), ws)
	if res.Status != "OK" || !res.Verified {
		t.Fatalf("remote evidence should verify, got %s verified=%v err=%s", res.Status, res.Verified, res.Error)
	}
}

// TestCleanupDryRunChangesNothing: dry-run with evidence still deletes nothing.
func TestCleanupDryRunChangesNothing(t *testing.T) {
	root := t.TempDir()
	pvc, pv, wsCR := buildLocalPathFixture(t, root)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pvc, pv, wsCR).Build()
	ws := localWs(t, root)

	logPath := filepath.Join(t.TempDir(), "log")
	os.WriteFile(logPath, []byte("t\tOK\tws-9660946\t/d\t/r\tworkspace\t1K\t1\tx\n"), 0o644)

	res := NewCleaner(cl, root, true, nil, logPath).Clean(context.Background(), ws)
	if res.Status != "OK" {
		t.Fatalf("dry-run status = %s", res.Status)
	}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "ws-9660946", Namespace: "bocomwork"}, &aiv1alpha1.Workspace{}); err != nil {
		t.Errorf("dry-run must not delete workspace: %v", err)
	}
}

// TestInspectResourcesFindsLocalPath: a fully present ws-9660946 returns the
// CR, PVC (local-path) and PV details.
func TestInspectResourcesFindsLocalPath(t *testing.T) {
	root := t.TempDir()
	pvc, pv, wsCR := buildLocalPathFixture(t, root)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pvc, pv, wsCR).Build()
	ws := localWs(t, root)

	info := NewCleaner(cl, root, true, nil, "").InspectResources(context.Background(), ws)
	if !info.CRFound || !info.PVCFound || !info.PVFound {
		t.Fatalf("expected CR/PVC/PV all found, got %+v", info)
	}
	if !info.PVCLocalPathOK || !info.PVLocalPath {
		t.Errorf("expected local-path flags true, got pvcOK=%v pvLP=%v", info.PVCLocalPathOK, info.PVLocalPath)
	}
	if info.PVName != "ws-9660946-pv" || info.PVCVolumeName != "ws-9660946-pv" {
		t.Errorf("unexpected names: pv=%q vol=%q", info.PVName, info.PVCVolumeName)
	}
	if info.Note != "" {
		t.Errorf("expected no note when resources present, got %q", info.Note)
	}
}

// TestInspectResourcesEmptyNoError: with no CR/PVC/PV at all, InspectResources
// must still return a row (never panic / error) explaining "无".
func TestInspectResourcesEmptyNoError(t *testing.T) {
	root := t.TempDir()
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).Build() // empty cluster
	ws := localWs(t, root)

	info := NewCleaner(cl, root, true, nil, "").InspectResources(context.Background(), ws)
	if info.CRFound || info.PVCFound || info.PVFound {
		t.Fatalf("expected all not-found, got %+v", info)
	}
	if info.Note == "" {
		t.Errorf("expected a human note for the empty case, got empty")
	}
	if info.Workspace != "ws-9660946" {
		t.Errorf("workspace name should be preserved, got %q", info.Workspace)
	}
}

// TestCleanupWritesAuditLog: a successful cleanup appends a CLEANED row to the
// migrate log (so cleanup has the same audit trail as migration), and that row
// must NOT make LogHasOK report success for the workspace (cleanup is not
// migration proof).
func TestCleanupWritesAuditLog(t *testing.T) {
	root := t.TempDir()
	pvc, pv, wsCR := buildLocalPathFixture(t, root)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pvc, pv, wsCR).Build()
	ws := localWs(t, root)

	logPath := filepath.Join(t.TempDir(), "oss-migrate.log")
	// Seed migration proof so cleanup proceeds.
	os.WriteFile(logPath, []byte("# time\tstatus\tworkspace\tlocal_dir\tremote_prefix\tdirs\tsize\tfiles\tnote\n"+
		"t\tOK\tws-9660946\t/dir\toss://b/workspaces/ws-9660946\tworkspace\t1K\t1\tx\n"), 0o644)

	res := NewCleaner(cl, root, false, nil, logPath).Clean(context.Background(), ws)
	if res.Status != "OK" {
		t.Fatalf("expected cleanup OK, got %s (%s)", res.Status, res.Error)
	}

	rows, err := readLogTSV(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var cleaned int
	for _, r := range rows {
		if r.Workspace == "ws-9660946" && r.Status == "CLEANED" {
			cleaned++
		}
	}
	if cleaned != 1 {
		t.Fatalf("expected exactly one CLEANED row, got %d (rows=%+v)", cleaned, rows)
	}
	// The CLEANED row must still count as OK evidence only because of the
	// pre-seeded migration row — a CLEANED-only log must not verify.
	onlyCleanup := filepath.Join(t.TempDir(), "only.log")
	os.WriteFile(onlyCleanup, []byte("t\tCLEANED\tws-9660946\t\t\t\t\t\tcleanup ok\n"), 0o644)
	if LogHasOK(onlyCleanup, "ws-9660946") {
		t.Error("a CLEANED row must NOT satisfy LogHasOK (cleanup is not migration proof)")
	}
}

// TestCleanupRefusalLogged: a refused cleanup still writes a CLEAN-FAIL row.
func TestCleanupRefusalLogged(t *testing.T) {
	root := t.TempDir()
	pvc, pv, wsCR := buildLocalPathFixture(t, root)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pvc, pv, wsCR).Build()
	ws := localWs(t, root)

	logPath := filepath.Join(t.TempDir(), "log")
	res := NewCleaner(cl, root, false, nil, logPath).Clean(context.Background(), ws)
	if res.Status != "FAIL" {
		t.Fatalf("expected refusal, got %s", res.Status)
	}
	rows, _ := readLogTSV(logPath)
	found := false
	for _, r := range rows {
		if r.Workspace == "ws-9660946" && r.Status == "CLEAN-FAIL" && r.Note != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a CLEAN-FAIL row with a note, rows=%+v", rows)
	}
}
