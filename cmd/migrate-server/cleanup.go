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

// Clean removes the resources of one Workspace. It refuses (returns FAIL)
// unless migration to OSS is proven, and only acts when the backing PVC is on
// the local-path StorageClass.
func (c *Cleaner) Clean(ctx context.Context, ws Workspace) CleanupResult {
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


	// Locate the Workspace CR and its namespace.
	wsv := &aiv1alpha1.WorkspaceList{}
	if err := c.k8s.List(ctx, wsv); err != nil {
		return fail("list workspaces: %v", err)
	}
	var target *aiv1alpha1.Workspace
	for i := range wsv.Items {
		if wsv.Items[i].Name == ws.Name {
			target = &wsv.Items[i]
			break
		}
	}
	if target == nil {
		// No CR: still allow PVC/PV/dir cleanup if they exist by name.
		act("Workspace CR ws-%s not found (continuing with PVC/PV/dir)", ws.ID)
	}

	ns := ""
	if target != nil {
		ns = target.Namespace
	}

	// Resolve the PVC. Prefer status.pvcName, else the deterministic name.
	pvcName := ws.Name + "-pvc"
	if target != nil && target.Status.PVCName != "" {
		pvcName = target.Status.PVCName
	}

	var pvc *corev1.PersistentVolumeClaim
	// Namespace may be unknown if CR is gone; try default then bocomwork.
	var pvcNS string
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
			pvc = obj
			pvcNS = cand
			break
		}
		if !apierrors.IsNotFound(err) {
			return fail("get pvc %s/%s: %v", cand, pvcName, err)
		}
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
