package main

// cleanup.go deletes the resources behind an already-migrated Workspace.
//
// This is intentionally NOT part of the shell script. It is only ever invoked
// for Workspaces whose migration has been PROVEN, and it refuses to delete
// anything when that proof is missing. Concretely, Clean() will not touch any
// Kubernetes object unless at least one of these holds:
//
//   - the migration log records status=OK for this Workspace (LogHasOK), or
//   - the target OSS prefix already holds real objects (remote probe).
//
// If neither can be confirmed, Clean() returns FAIL and deletes nothing. This
// is the guard against physically destroying the only copy of unmigrated data.
//
// Additional guards:
//   - Workspace CR / PVC / PV are touched only when the PVC is local-path.
//   - The PV is deleted only when it is a local-path volume.
//   - The node-local directory is NEVER removed by this tool. It is kept as a
//     last-resort fallback; removing it is left to a human. (The PV/pvc deletion
//     may still cause local-path provisioner teardown of the volume dir, which
//     is exactly why we require migration proof before proceeding.)
//
// Deletion order: Workspace CR -> PVC -> PV.
// DryRun prints what would be deleted and changes nothing.

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

// localPathProvisioner identifies PVs that belong to local-path storage.
const localPathProvisioner = "rancher.io/local-path"

// localPathStorageClass is the StorageClass name we allow cleanup for.
const localPathStorageClass = "local-path"

// CleanupResult reports the per-resource outcome for one Workspace.
type CleanupResult struct {
	Workspace string   `json:"workspace"`
	DryRun    bool     `json:"dryRun"`
	Verified  bool     `json:"verified"` // migration evidence confirmed before deleting
	Actions   []string `json:"actions"`  // human readable steps performed/skipped
	Status    string   `json:"status"`   // OK | FAIL
	Error     string   `json:"error,omitempty"`
}

// ResourceInfo is the read-only, per-workspace view of the local-path storage
// objects the cleanup step would touch. It is intentionally all-optional: a
// missing CR / PVC / PV is reported with Found=false and a human Note, never as
// an error, so an empty result renders as "无" instead of failing the request.
type ResourceInfo struct {
	Workspace string `json:"workspace"`

	// Workspace CR
	CRFound     bool   `json:"crFound"`
	CRNamespace string `json:"crNamespace,omitempty"`
	CRName      string `json:"crName,omitempty"`

	// PVC
	PVCFound       bool   `json:"pvcFound"`
	PVCNamespace   string `json:"pvcNamespace,omitempty"`
	PVCName        string `json:"pvcName,omitempty"`
	PVCStorageCls  string `json:"pvcStorageClass,omitempty"`
	PVCPhase       string `json:"pvcPhase,omitempty"`
	PVCVolumeName  string `json:"pvcVolumeName,omitempty"`
	PVCLocalPathOK bool   `json:"pvcLocalPathOK"` // true when storageClass is local-path

	// PV
	PVFound      bool   `json:"pvFound"`
	PVName       string `json:"pvName,omitempty"`
	PVDriver     string `json:"pvDriver,omitempty"`
	PVLocalPath  bool   `json:"pvLocalPath"` // true when provisioned by local-path
	PVReclaim    string `json:"pvReclaim,omitempty"`
	PVLocalPathA string `json:"pvLocalPathA,omitempty"` // spec.local.path, if present

	Note string `json:"note,omitempty"` // e.g. "未找到 PVC/PV"
}

// Cleaner deletes Workspace / PVC / PV for one node, after proving migration.
type Cleaner struct {
	k8s    client.Client
	root   string // local-path root, e.g. /data/local-path-storage
	dryRun bool
	// mig is used to probe the OSS target; may be nil (then log evidence only).
	mig     *Migrator
	logPath string // migration TSV log used for LogHasOK evidence
}

func NewCleaner(k8s client.Client, root string, dryRun bool, mig *Migrator, logPath string) *Cleaner {
	return &Cleaner{k8s: k8s, root: root, dryRun: dryRun, mig: mig, logPath: logPath}
}

// verifyMigrated returns (verified, evidence, error). verified is true when the
// Workspace is proven to exist in OSS, either via the migration log (status=OK)
// or via a remote probe that finds real objects. An unreachable remote is not
// treated as "migrated": it returns verified=false so the caller refuses.
func (c *Cleaner) verifyMigrated(ctx context.Context, ws Workspace) (bool, string, error) {
	// 1) migration log evidence
	if c.logPath != "" && LogHasOK(c.logPath, ws.Name) {
		return true, "migration log records status=OK", nil
	}
	// 2) remote OSS evidence
	if c.mig != nil {
		remote := fmt.Sprintf("%s/%s/ws-%s/", c.mig.cfg.OSSBucket, c.mig.cfg.OSSPrefix, ws.ID)
		st, err := c.mig.remoteState(ctx, remote)
		if err == nil && st == remoteHasData {
			return true, "OSS prefix already holds objects", nil
		}
		if err != nil {
			return false, "", fmt.Errorf("cannot confirm OSS state: %v", err)
		}
	}
	return false, "", nil
}

// Clean removes the resources of one Workspace and records the outcome in the
// audit log. It refuses (returns FAIL) unless migration to OSS is proven, and
// only acts when the backing PVC is on the local-path StorageClass.
//
// Every invocation (success or refusal) appends one row to the migrate log so
// cleanup has the same audit trail as migration. Cleanup rows use a DISTINCT
// status (CLEANED / CLEAN-FAIL) and never "OK", so LogHasOK() keeps meaning
// "data was migrated" and a later cleanup can never mistake a prior deletion
// for migration proof.
func (c *Cleaner) Clean(ctx context.Context, ws Workspace) CleanupResult {
	res := c.cleanInner(ctx, ws)
	c.logCleanup(res)
	return res
}

// cleanInner performs the actual cleanup; Clean() wraps it with logging.
func (c *Cleaner) cleanInner(ctx context.Context, ws Workspace) CleanupResult {
	res := CleanupResult{Workspace: ws.Name, DryRun: c.dryRun, Status: "OK"}
	act := func(format string, a ...interface{}) {
		res.Actions = append(res.Actions, fmt.Sprintf(format, a...))
	}
	fail := func(format string, a ...interface{}) CleanupResult {
		res.Status = "FAIL"
		res.Error = fmt.Sprintf(format, a...)
		return res
	}

	// STEP 0 (mandatory): prove the data exists in OSS before deleting anything.
	verified, evidence, verr := c.verifyMigrated(ctx, ws)
	if verr != nil {
		return fail("refuse cleanup: %v (migration not proven)", verr)
	}
	if !verified {
		return fail("refuse cleanup: ws-%s has no migration evidence (no status=OK log and no OSS objects); refusing to delete to avoid data loss", ws.ID)
	}
	res.Verified = true
	act("migration verified: %s", evidence)

	// Locate the Workspace CR, PVC and PV using the shared resolver so the
	// read-only view and the delete path never drift apart.
	target, pvc, pvcNS, lookupErr := c.resolveResources(ctx, ws)
	if lookupErr != nil {
		return fail("resolve resources: %v", lookupErr)
	}

	if target == nil {
		// No CR: still allow PVC/PV/dir cleanup if they exist by name.
		act("Workspace CR ws-%s not found (continuing with PVC/PV/dir)", ws.ID)
	}

	pvcName := ws.Name + "-pvc"
	if target != nil && target.Status.PVCName != "" {
		pvcName = target.Status.PVCName
	}

	// Guard: refuse if the PVC exists but is not local-path.
	if pvc != nil {
		sc := ""
		if pvc.Spec.StorageClassName != nil {
			sc = *pvc.Spec.StorageClassName
		}
		if sc != "" && sc != localPathStorageClass {
			return fail("refuse: PVC %s/%s uses storageClass=%q, not %q", pvcNS, pvcName, sc, localPathStorageClass)
		}
	}

	// 1) delete Workspace CR
	if target != nil {
		if c.dryRun {
			act("would delete Workspace %s/%s", target.Namespace, target.Name)
		} else {
			if err := c.k8s.Delete(ctx, target); err != nil && !apierrors.IsNotFound(err) {
				return fail("delete workspace %s/%s: %v", target.Namespace, target.Name, err)
			}
			act("deleted Workspace %s/%s", target.Namespace, target.Name)
		}
	}

	// 2) delete PVC
	if pvc != nil {
		if c.dryRun {
			act("would delete PVC %s/%s", pvcNS, pvcName)
		} else {
			if err := c.k8s.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
				return fail("delete pvc %s/%s: %v", pvcNS, pvcName, err)
			}
			act("deleted PVC %s/%s", pvcNS, pvcName)
		}
	} else {
		act("PVC ws-%s-pvc not found", ws.ID)
	}

	// 3) delete PV (only local-path driver)
	pvName := ""
	if pvc != nil {
		pvName = pvc.Spec.VolumeName
	}
	if pvName != "" {
		pv := &corev1.PersistentVolume{}
		err := c.k8s.Get(ctx, types.NamespacedName{Name: pvName}, pv)
		if err == nil {
			if drivesLocalPath(pv) {
				if c.dryRun {
					act("would delete PV %s", pvName)
				} else {
					if err := c.k8s.Delete(ctx, pv); err != nil && !apierrors.IsNotFound(err) {
						return fail("delete pv %s: %v", pvName, err)
					}
					act("deleted PV %s", pvName)
				}
			} else {
				act("refuse PV %s: not a local-path volume", pvName)
			}
		} else if !apierrors.IsNotFound(err) {
			return fail("get pv %s: %v", pvName, err)
		}
	}

	// 4) local directory: NEVER removed by this tool. Kept as a last-resort
	//    fallback copy. (Note: deleting the PV above may still cause the
	//    local-path provisioner to tear down the volume dir; that is why step 0
	//    requires migration proof first.)
	if ws.Dir != "" {
		act("local dir kept (not removed by migrate-server): %s", ws.Dir)
	}

	return res
}

// logCleanup appends one audit row per cleanup to the migrate log, mirroring
// Migrator.logWrite. It skips dry-run and empty log paths. Status is CLEANED on
// success and CLEAN-FAIL on refusal/failure (never OK, so LogHasOK stays about
// migration). The note carries the failure reason or a short summary.
func (c *Cleaner) logCleanup(res CleanupResult) {
	if c.dryRun || c.logPath == "" {
		return
	}
	status := "CLEANED"
	if res.Status != "OK" {
		status = "CLEAN-FAIL"
	}
	note := res.Error
	if note == "" {
		note = "cleanup ok"
	}
	f, err := os.OpenFile(c.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	if info != nil && info.Size() == 0 {
		fmt.Fprintln(f, "# time\tstatus\tworkspace\tlocal_dir\tremote_prefix\tdirs\tsize\tfiles\tnote")
	}
	// Reuse the migrate TSV shape: time, status, workspace, then empty columns
	// for local_dir/remote/dirs/size/files and the note last.
	fmt.Fprintf(f, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
		time.Now().Format("2006-01-02T15:04:05-0700"),
		status, res.Workspace, "", "", "", "", "", note)
}

// resolveResources locates the Workspace CR, its PVC, and the PVC's namespace
// for one workspace. A missing CR or PVC is NOT an error (nil is returned);
// only a real API failure is. Shared by Clean() and InspectResources() so the
// read-only view and the delete path agree on which objects are in scope.
func (c *Cleaner) resolveResources(ctx context.Context, ws Workspace) (*aiv1alpha1.Workspace, *corev1.PersistentVolumeClaim, string, error) {
	wsv := &aiv1alpha1.WorkspaceList{}
	if err := c.k8s.List(ctx, wsv); err != nil {
		return nil, nil, "", fmt.Errorf("list workspaces: %w", err)
	}
	var target *aiv1alpha1.Workspace
	for i := range wsv.Items {
		if wsv.Items[i].Name == ws.Name {
			target = &wsv.Items[i]
			break
		}
	}

	ns := ""
	if target != nil {
		ns = target.Namespace
	}

	pvcName := ws.Name + "-pvc"
	if target != nil && target.Status.PVCName != "" {
		pvcName = target.Status.PVCName
	}

	// Namespace may be unknown if CR is gone; try those, skipping empties.
	candidateNS := []string{ns, "default", "bocomwork"}
	seen := map[string]bool{}
	for _, cand := range candidateNS {
		if cand == "" || seen[cand] {
			continue
		}
		seen[cand] = true
		obj := &corev1.PersistentVolumeClaim{}
		err := c.k8s.Get(ctx, types.NamespacedName{Namespace: cand, Name: pvcName}, obj)
		if err == nil {
			return target, obj, cand, nil
		}
		if !apierrors.IsNotFound(err) {
			return nil, nil, "", fmt.Errorf("get pvc %s/%s: %w", cand, pvcName, err)
		}
	}
	return target, nil, "", nil
}

// InspectResources returns the read-only local-path CR/PVC/PV view for one
// workspace. It NEVER returns an error for "nothing found": a workspace with no
// CR/PVC/PV yields an entry with Found=false and a Note, so the UI can render
// "无" instead of surfacing a spurious failure. Only genuine API errors are
// reported (in the Note), and even then the row is returned rather than aborting.
func (c *Cleaner) InspectResources(ctx context.Context, ws Workspace) ResourceInfo {
	info := ResourceInfo{Workspace: ws.Name}

	target, pvc, pvcNS, err := c.resolveResources(ctx, ws)
	if err != nil {
		info.Note = "查询失败: " + err.Error()
		return info
	}

	if target != nil {
		info.CRFound = true
		info.CRNamespace = target.Namespace
		info.CRName = target.Name
	}

	if pvc != nil {
		info.PVCFound = true
		info.PVCNamespace = pvcNS
		info.PVCName = pvc.Name
		if pvc.Spec.StorageClassName != nil {
			info.PVCStorageCls = *pvc.Spec.StorageClassName
		}
		info.PVCPhase = string(pvc.Status.Phase)
		info.PVCVolumeName = pvc.Spec.VolumeName
		info.PVCLocalPathOK = info.PVCStorageCls == localPathStorageClass || info.PVCStorageCls == ""

		if pvc.Spec.VolumeName != "" {
			pv := &corev1.PersistentVolume{}
			if gerr := c.k8s.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); gerr == nil {
				info.PVFound = true
				info.PVName = pv.Name
				if pv.Spec.CSI != nil {
					info.PVDriver = pv.Spec.CSI.Driver
				}
				info.PVLocalPath = drivesLocalPath(pv)
				info.PVReclaim = string(pv.Spec.PersistentVolumeReclaimPolicy)
				if pv.Spec.Local != nil {
					info.PVLocalPathA = pv.Spec.Local.Path
				}
			} else if !apierrors.IsNotFound(gerr) {
				info.Note = "查询 PV 失败: " + gerr.Error()
			}
		}
	}

	if !info.CRFound && !info.PVCFound {
		info.Note = "未找到 Workspace CR / PVC（该 workspace 可能已清理）"
	} else if !info.PVCFound {
		info.Note = "未找到 PVC"
	} else if info.PVCVolumeName == "" {
		info.Note = "PVC 未绑定 PV"
	} else if !info.PVFound {
		info.Note = "未找到 PV（可能已删除）"
	}
	return info
}

// drivesLocalPath reports whether pv was provisioned by local-path.
func drivesLocalPath(pv *corev1.PersistentVolume) bool {
	if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == localPathProvisioner {
		return true
	}
	if pv.Spec.Local != nil {
		return true
	}
	if pv.Annotations != nil {
		if v := pv.Annotations["pv.kubernetes.io/provisioned-by"]; v == localPathProvisioner {
			return true
		}
	}
	return false
}

var _ = metav1.Now
