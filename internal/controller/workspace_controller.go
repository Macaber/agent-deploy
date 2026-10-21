/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiresources "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

const (
	// workspaceFinalizer 旧版本遗留 finalizer，仅用于清理升级兼容，不再向新资源添加
	workspaceFinalizer = "ai.example.com/workspace-cleanup"

	// podWorkspaceIndexKey is the field index key for looking up workspace pods in O(1)
	podWorkspaceIndexKey = "metadata.workspace"

	// sharedNetpolName is the name of the single namespace-level NetworkPolicy template
	// (Task 2.1). One static policy per tenant namespace replaces the legacy per-workspace
	// "ws-xxx-netpol" objects, cutting the cluster-wide NetworkPolicy count from
	// 5,000+ down to single digits and shrinking node iptables/ipset rules accordingly.
	sharedNetpolName = "workspace-shared-netpol"
	// legacyNetpolSuffix is appended to the Workspace name to form the legacy
	// per-workspace NetworkPolicy name cleaned up during reconciliation.
	legacyNetpolSuffix = "-netpol"
	// managedByLabelKey/Value select (and mark) every Pod created for a Workspace so the
	// single shared namespace-level NetworkPolicy template manages them all.
	managedByLabelKey   = "app.kubernetes.io/managed-by"
	managedByLabelValue = "workspace-operator"
)

// WorkspaceReconciler reconciles a Workspace object
type WorkspaceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// GlobalAllowedNamespaces holds controller-wide NetworkPolicy egress exceptions that
	// are applied to EVERY Workspace on top of spec.networkPolicy.allowedNamespaces
	// (e.g. letting all workspaces export OTLP to a shared collector). Populated from
	// the --netpol-allowed-namespaces flag / NETPOL_ALLOWED_NAMESPACES env (JSON array).
	GlobalAllowedNamespaces []aiv1alpha1.WorkspaceNetworkPolicyNamespaceRule
	// MaxConcurrentReconciles is the maximum number of concurrent Reconciles which can be run.
	// Defaults to 30 if unset or <= 0.
	MaxConcurrentReconciles int
	// ActiveInstancesSyncInterval is how often the workspace_active_instances gauge
	// is rebuilt from a cluster-wide Workspace list (Task 5.2). Defaults to 1 minute
	// if unset or <= 0.
	ActiveInstancesSyncInterval time.Duration
}

// +kubebuilder:rbac:groups=ai.example.com,resources=workspaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ai.example.com,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ai.example.com,resources=workspaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch;update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch;update

// errWorkspaceGone marks a reconcile request whose Workspace no longer exists.
// The metrics wrapper in Reconcile drops it so delete-event reconciles do not
// pollute the duration histogram.
var errWorkspaceGone = errors.New("workspace no longer exists")

// Reconcile instruments the reconciliation loop with the Workspace Prometheus
// metrics (duration histogram, error counter) and delegates the actual work to
// reconcile.
func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	registerWorkspaceMetrics()

	start := time.Now()
	result, err := r.reconcile(ctx, req)
	if errors.Is(err, errWorkspaceGone) {
		// A delete-event reconcile that found nothing to do is not a
		// reconciliation: keeping it out of the histogram protects the
		// P50/P99 resolution. Reconcile errors are already surfaced by
		// controller-runtime's controller_runtime_reconcile_errors_total.
		return ctrl.Result{}, nil
	}
	workspaceReconcileDuration.Observe(time.Since(start).Seconds())
	return result, err
}

// reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *WorkspaceReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch Workspace
	ws := &aiv1alpha1.Workspace{}
	if err := r.Get(ctx, req.NamespacedName, ws); err != nil {
		if apierrors.IsNotFound(err) {
			// Workspace is gone: drop it from the active instances gauge.
			workspacePhases.unset(req.NamespacedName)
			// Task 2.1: the controller no longer adds a finalizer, so a Workspace
			// deletion is immediate and the delete-event reconcile always lands
			// here — the DeletionTimestamp branch below is never reached for it.
			// The shared namespace-level NetworkPolicy template must therefore be
			// garbage-collected from this branch, or it leaks in every tenant
			// namespace forever.
			if err := r.maybeDeleteSharedNetworkPolicy(ctx, req.Namespace, req.NamespacedName); err != nil {
				log.Error(err, "Failed to clean up shared NetworkPolicy template", "namespace", req.Namespace)
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, errWorkspaceGone
		}
		return ctrl.Result{}, err
	}

	// 删除处理：不自动删除 PV 和 PVC（包括 OSS 方案），保留持久化存储与底层数据。
	// 若存在旧版本遗留的 finalizer，清理移除以便 Workspace 正常删除，但不执行 PV/PVC/OSS 数据清理。
	if !ws.DeletionTimestamp.IsZero() {
		// Workspace is being deleted: stop counting it as an active instance.
		workspacePhases.unset(req.NamespacedName)
		if controllerutil.ContainsFinalizer(ws, workspaceFinalizer) {
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latestWs := &aiv1alpha1.Workspace{}
				if err := r.Get(ctx, req.NamespacedName, latestWs); err != nil {
					if apierrors.IsNotFound(err) {
						return nil
					}
					return err
				}
				if !controllerutil.ContainsFinalizer(latestWs, workspaceFinalizer) {
					return nil
				}
				controllerutil.RemoveFinalizer(latestWs, workspaceFinalizer)
				return r.Update(ctx, latestWs)
			})
			if err != nil {
				log.Error(err, "Failed to remove legacy finalizer from Workspace", "workspace", ws.Name)
				return ctrl.Result{}, err
			}
			log.Info("Successfully removed legacy finalizer for Workspace", "workspace", ws.Name)
		}

		// Task 2.1: garbage-collect the shared namespace-level NetworkPolicy template
		// when the last Workspace in the namespace is going away. This branch only
		// runs while a legacy finalizer still holds the object; the normal (no
		// finalizer) deletion path is handled in the NotFound branch above.
		if err := r.maybeDeleteSharedNetworkPolicy(ctx, ws.Namespace, req.NamespacedName); err != nil {
			log.Error(err, "Failed to clean up shared NetworkPolicy template", "namespace", ws.Namespace)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// 若旧版本已注入 finalizer，在正常调谐中主动清理移除
	if controllerutil.ContainsFinalizer(ws, workspaceFinalizer) {
		_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latestWs := &aiv1alpha1.Workspace{}
			if err := r.Get(ctx, req.NamespacedName, latestWs); err != nil {
				return err
			}
			if !latestWs.DeletionTimestamp.IsZero() {
				return nil
			}
			if !controllerutil.ContainsFinalizer(latestWs, workspaceFinalizer) {
				return nil
			}
			controllerutil.RemoveFinalizer(latestWs, workspaceFinalizer)
			return r.Update(ctx, latestWs)
		})
	}

	// Initialize status Phase and LastActiveTime if not set
	needsStatusUpdate := false
	if ws.Status.Phase == "" {
		ws.Status.Phase = aiv1alpha1.WorkspacePending
		needsStatusUpdate = true
	}
	if ws.Status.LastActiveTime == nil {
		ws.Status.LastActiveTime = &metav1.Time{Time: time.Now()}
		needsStatusUpdate = true
	}
	if needsStatusUpdate {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latestWs := &aiv1alpha1.Workspace{}
			if err := r.Get(ctx, req.NamespacedName, latestWs); err != nil {
				return err
			}
			changed := false
			if latestWs.Status.Phase == "" {
				latestWs.Status.Phase = aiv1alpha1.WorkspacePending
				changed = true
			}
			if latestWs.Status.LastActiveTime == nil {
				latestWs.Status.LastActiveTime = &metav1.Time{Time: time.Now()}
				changed = true
			}
			if !changed {
				return nil
			}
			return r.Status().Update(ctx, latestWs)
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch to get latest resourceVersion
		if err := r.Get(ctx, req.NamespacedName, ws); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Auto-populate 'oa' label on Workspace CR if namespace is bocomwork and OA exists in Env
	if ws.Namespace == "bocomwork" {
		oaVal := getOAValueFromEnv(ws.Spec.Runtime.Env)
		if oaVal != "" && (ws.Labels == nil || ws.Labels["oa"] != oaVal) {
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latestWs := &aiv1alpha1.Workspace{}
				if err := r.Get(ctx, req.NamespacedName, latestWs); err != nil {
					return err
				}
				if latestWs.Labels == nil {
					latestWs.Labels = make(map[string]string)
				}
				latestWs.Labels["oa"] = oaVal
				return r.Update(ctx, latestWs)
			})
			if err != nil {
				return ctrl.Result{}, err
			}
			if err := r.Get(ctx, req.NamespacedName, ws); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// 2. Reconcile children (PVC, Deployment, Service, Ingress)
	pvcName, err := r.reconcilePVC(ctx, ws)
	if err != nil {
		log.Error(err, "Failed to reconcile PVC")
		return ctrl.Result{}, err
	}

	deployName, reconciledReplicas, err := r.reconcileDeployment(ctx, ws, pvcName)
	if err != nil {
		log.Error(err, "Failed to reconcile Deployment")
		return ctrl.Result{}, err
	}

	svcName, err := r.reconcileService(ctx, ws)
	if err != nil {
		log.Error(err, "Failed to reconcile Service")
		return ctrl.Result{}, err
	}

	endpoint, err := r.reconcileIngress(ctx, ws, svcName)
	if err != nil {
		log.Error(err, "Failed to reconcile Ingress")
		return ctrl.Result{}, err
	}

	if err := r.reconcileNetworkPolicy(ctx, ws); err != nil {
		log.Error(err, "Failed to reconcile NetworkPolicy")
		return ctrl.Result{}, err
	}

	// 3. Status Phase logic & Idle Timeout logic
	var nextRequeue time.Duration

	oldPhase := ws.Status.Phase
	oldPodName := ws.Status.PodName
	oldPVCName := ws.Status.PVCName
	oldEndpoint := ws.Status.Endpoint

	ws.Status.PVCName = pvcName
	ws.Status.Endpoint = endpoint

	// Check underlying Deployment/Pod status to update phase
	deploy := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: deployName}, deploy); err == nil {
		if ws.Spec.Stopped {
			ws.Status.Phase = aiv1alpha1.WorkspaceStopped
			ws.Status.PodName = ""
		} else {
			if reconciledReplicas == 0 {
				ws.Status.Phase = aiv1alpha1.WorkspaceSleeping
				ws.Status.PodName = ""
			} else if deploy.Status.ReadyReplicas > 0 {
				ws.Status.Phase = aiv1alpha1.WorkspaceRunning

				// Find pod name to update status using field indexer (O(1)) with fallback
				podList := &corev1.PodList{}
				opts := []client.ListOption{
					client.InNamespace(ws.Namespace),
					client.MatchingFields{podWorkspaceIndexKey: ws.Name},
				}
				err := r.List(ctx, podList, opts...)
				if err != nil {
					// Fallback to MatchingLabels if indexer is unavailable (e.g. in test envs)
					log.V(1).Info("Pod field-index lookup failed, falling back to label selector", "error", err)
					opts = []client.ListOption{
						client.InNamespace(ws.Namespace),
						client.MatchingLabels(deploy.Spec.Selector.MatchLabels),
					}
					err = r.List(ctx, podList, opts...)
				}
				if err == nil && len(podList.Items) > 0 {
					selectedPod := podList.Items[0].Name
					for _, p := range podList.Items {
						if p.DeletionTimestamp.IsZero() && p.Status.Phase == corev1.PodRunning {
							selectedPod = p.Name
							break
						}
					}
					ws.Status.PodName = selectedPod
				}
			} else {
				ws.Status.Phase = aiv1alpha1.WorkspaceStarting
				ws.Status.PodName = ""
			}
		}
	}

	// Handle transitions and Idle timeout calculation
	if ws.Status.Phase == aiv1alpha1.WorkspaceRunning {
		// Reset last active time only when transitioning from a non-active state to Running,
		// avoiding accidental reset during scale-down when ReadyReplicas hasn't reached 0 yet.
		if oldPhase == aiv1alpha1.WorkspaceSleeping || oldPhase == aiv1alpha1.WorkspaceStopped ||
			oldPhase == aiv1alpha1.WorkspacePending || oldPhase == aiv1alpha1.WorkspaceFailed {
			ws.Status.LastActiveTime = &metav1.Time{Time: time.Now()}
		}

		// Calculate next requeue for idle timeout
		if ws.Spec.IdleTimeout != "" {
			idleDuration, err := time.ParseDuration(ws.Spec.IdleTimeout)
			if err == nil {
				idleExpiry := ws.Status.LastActiveTime.Add(idleDuration)
				remainingIdle := time.Until(idleExpiry)
				if remainingIdle > 0 {
					nextRequeue = remainingIdle
				} else {
					// Idle expired, trigger immediate reconcile to scale down
					nextRequeue = 1 * time.Second
				}
			} else {
				log.Error(err, "Failed to parse idleTimeout in requeue calculation", "value", ws.Spec.IdleTimeout)
			}
		}
	}

	// Update status if anything changed
	if oldPhase != ws.Status.Phase || oldPodName != ws.Status.PodName || oldPVCName != ws.Status.PVCName || oldEndpoint != ws.Status.Endpoint {
		log.Info("Updating Workspace status", "phase", ws.Status.Phase, "podName", ws.Status.PodName)
		desiredPhase := ws.Status.Phase
		desiredPodName := ws.Status.PodName
		desiredPVCName := ws.Status.PVCName
		desiredEndpoint := ws.Status.Endpoint
		desiredLastActiveTime := ws.Status.LastActiveTime

		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latestWs := &aiv1alpha1.Workspace{}
			if err := r.Get(ctx, req.NamespacedName, latestWs); err != nil {
				return err
			}
			latestWs.Status.Phase = desiredPhase
			latestWs.Status.PodName = desiredPodName
			latestWs.Status.PVCName = desiredPVCName
			latestWs.Status.Endpoint = desiredEndpoint
			if desiredLastActiveTime != nil && (latestWs.Status.LastActiveTime == nil || desiredLastActiveTime.After(latestWs.Status.LastActiveTime.Time)) {
				latestWs.Status.LastActiveTime = desiredLastActiveTime
			}
			return r.Status().Update(ctx, latestWs)
		})
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Track the Workspace in the active instances gauge with its final phase.
	workspacePhases.set(req.NamespacedName, ws.Status.Phase)

	return ctrl.Result{RequeueAfter: nextRequeue}, nil
}

// getDefaultStorageClass returns the default StorageClass and its name.
// It first checks the DEFAULT_STORAGE_CLASS environment variable.
// If not set, it inspects the cluster for a StorageClass annotated with
// storageclass.kubernetes.io/is-default-class="true" (or legacy storageclass.beta.kubernetes.io/is-default-class="true").
func (r *WorkspaceReconciler) getDefaultStorageClass(ctx context.Context) (*storagev1.StorageClass, string) {
	if envDefault := strings.TrimSpace(os.Getenv("DEFAULT_STORAGE_CLASS")); envDefault != "" {
		scObj := &storagev1.StorageClass{}
		if err := r.Get(ctx, client.ObjectKey{Name: envDefault}, scObj); err == nil {
			return scObj, envDefault
		}
		return nil, envDefault
	}

	scList := &storagev1.StorageClassList{}
	if err := r.List(ctx, scList); err == nil {
		for i := range scList.Items {
			item := &scList.Items[i]
			if item.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" ||
				item.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true" {
				return item, item.Name
			}
		}
	}
	return nil, ""
}

func (r *WorkspaceReconciler) reconcilePVC(ctx context.Context, ws *aiv1alpha1.Workspace) (string, error) {
	log := logf.FromContext(ctx)
	pvcName := ws.Name + "-pvc"

	var storageClass *string
	if ws.Spec.Storage.StorageClass != "" {
		storageClass = &ws.Spec.Storage.StorageClass
	}

	// Check if the requested StorageClass is OSS or uses ossplugin.csi.alibabacloud.com
	isOSS := false
	var sc *storagev1.StorageClass
	if storageClass != nil && *storageClass != "" {
		scObj := &storagev1.StorageClass{}
		if err := r.Get(ctx, client.ObjectKey{Name: *storageClass}, scObj); err == nil {
			if scObj.Provisioner == "ossplugin.csi.alibabacloud.com" || strings.Contains(strings.ToLower(*storageClass), "oss") {
				isOSS = true
				sc = scObj
			}
		} else if strings.Contains(strings.ToLower(*storageClass), "oss") {
			isOSS = true
		}
	} else {
		// When storageClass is not explicitly specified in the Workspace spec,
		// resolve the default StorageClass:
		// 1. DEFAULT_STORAGE_CLASS environment variable (if configured)
		// 2. Cluster default StorageClass with annotation storageclass.kubernetes.io/is-default-class="true"
		defaultSC, defaultSCName := r.getDefaultStorageClass(ctx)
		if defaultSCName != "" {
			storageClass = &defaultSCName
			sc = defaultSC
			if (sc != nil && sc.Provisioner == "ossplugin.csi.alibabacloud.com") || strings.Contains(strings.ToLower(defaultSCName), "oss") {
				isOSS = true
			}
			log.Info("Resolved default StorageClass for workspace", "workspace", ws.Name, "storageClass", defaultSCName, "isOSS", isOSS)
		}
	}

	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: pvcName}, pvc)
	if err != nil {
		if apierrors.IsNotFound(err) {
			storageSize, err := apiresources.ParseQuantity(ws.Spec.Storage.Size)
			if err != nil {
				return "", err
			}

			if isOSS {
				pvName := ws.Name + "-pv"
				pv := &corev1.PersistentVolume{}
				pvErr := r.Get(ctx, client.ObjectKey{Name: pvName}, pv)
				if pvErr != nil && apierrors.IsNotFound(pvErr) {
					bucket := "your-user-workspaces-bucket"
					url := "oss-cn-hangzhou-internal.aliyuncs.com"
					otherOpts := "-o max_stat_cache_size=100000 -o allow_other"
					fuseType := "ossfs"
					secretName := "oss-secret"
					secretNamespace := "default"
					// OSS V4 签名参数：2025-09-01 起阿里云新建 Bucket 强制 V4 签名，
					// 由 StorageClass 透传到 PV，未配置时保持 V1（兼容存量专有云 Bucket）
					sigVersion := ""
					region := ""

					if sc != nil {
						if b, ok := sc.Parameters["bucket"]; ok && b != "" {
							bucket = b
						}
						if u, ok := sc.Parameters["url"]; ok && u != "" {
							url = u
						}
						if o, ok := sc.Parameters["otherOpts"]; ok && o != "" {
							otherOpts = o
						}
						if f, ok := sc.Parameters["fuseType"]; ok && f != "" {
							fuseType = f
						}
						if s, ok := sc.Parameters["sigVersion"]; ok {
							sigVersion = s
						}
						if rg, ok := sc.Parameters["region"]; ok {
							region = rg
						}
						if sName, ok := sc.Parameters["csi.storage.k8s.io/provisioner-secret-name"]; ok && sName != "" {
							secretName = sName
						} else if sName, ok := sc.Parameters["akIdSecret"]; ok && sName != "" {
							secretName = sName
						}
						if sNs, ok := sc.Parameters["csi.storage.k8s.io/provisioner-secret-namespace"]; ok && sNs != "" {
							secretNamespace = sNs
						}
					}

					subPath := "/workspaces/" + ws.Name
					scNameStr := ""
					if storageClass != nil {
						scNameStr = *storageClass
					}
					volumeAttributes := map[string]string{
						"bucket":    bucket,
						"url":       url,
						"path":      subPath,
						"otherOpts": otherOpts,
						"fuseType":  fuseType,
					}
					if sigVersion != "" {
						volumeAttributes["sigVersion"] = sigVersion
					}
					if region != "" {
						volumeAttributes["region"] = region
					}
					pv = &corev1.PersistentVolume{
						ObjectMeta: metav1.ObjectMeta{
							Name: pvName,
						},
						Spec: corev1.PersistentVolumeSpec{
							Capacity: corev1.ResourceList{
								corev1.ResourceStorage: storageSize,
							},
							AccessModes: []corev1.PersistentVolumeAccessMode{
								corev1.ReadWriteMany,
							},
							PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
							StorageClassName:              scNameStr,
							PersistentVolumeSource: corev1.PersistentVolumeSource{
								CSI: &corev1.CSIPersistentVolumeSource{
									Driver:       "ossplugin.csi.alibabacloud.com",
									VolumeHandle: pvName + "-handle",
									NodePublishSecretRef: &corev1.SecretReference{
										Name:      secretName,
										Namespace: secretNamespace,
									},
									// v1.30.4+ 新架构：FUSE Pod 在 ControllerPublishVolume 阶段创建，
									// 凭证必须同时提供给 controller 端，否则 FUSE Pod 无法访问 OSS
									ControllerPublishSecretRef: &corev1.SecretReference{
										Name:      secretName,
										Namespace: secretNamespace,
									},
									VolumeAttributes: volumeAttributes,
								},
							},
						},
					}
					log.Info("Auto-provisioning OSS PersistentVolume for workspace", "pvName", pvName, "subPath", subPath)
					if err := r.Create(ctx, pv); err != nil && !apierrors.IsAlreadyExists(err) {
						return "", err
					}
				}

				// For auto-provisioned OSS PVs, PVC binds explicitly to pvName with matching StorageClassName
				pvc = &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      pvcName,
						Namespace: ws.Namespace,
						Labels:    buildWorkspaceLabels(ws),
						Annotations: map[string]string{
							"volume.kubernetes.io/storage-provisioner": "ossplugin.csi.alibabacloud.com",
						},
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteMany,
						},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: storageSize,
							},
						},
						VolumeName:       pvName,
						StorageClassName: storageClass,
					},
				}
			} else {
				pvc = &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      pvcName,
						Namespace: ws.Namespace,
						Labels:    buildWorkspaceLabels(ws),
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: storageSize,
							},
						},
						StorageClassName: storageClass,
					},
				}
			}

			// 注意：不设置 ControllerReference / OwnerReference，以确保删除 Workspace 时不会由 K8s 垃圾回收（GC）级联删除 PVC
			if err := r.Create(ctx, pvc); err != nil {
				return "", err
			}
			return pvcName, nil
		}
		return "", err
	}

	// PVC already exists: 若存在旧版本设置的 Workspace OwnerReference，移除它以防止删除 Workspace 时 PVC 被级联删除
	if len(pvc.OwnerReferences) > 0 {
		hasWsOwner := false
		newOwnerRefs := make([]metav1.OwnerReference, 0, len(pvc.OwnerReferences))
		for _, ref := range pvc.OwnerReferences {
			if ref.Kind == "Workspace" && ref.Name == ws.Name {
				hasWsOwner = true
				continue
			}
			newOwnerRefs = append(newOwnerRefs, ref)
		}
		if hasWsOwner {
			pvc.OwnerReferences = newOwnerRefs
			if err := r.Update(ctx, pvc); err != nil {
				log.Error(err, "Failed to remove workspace owner reference from existing PVC", "pvcName", pvcName)
			} else {
				log.Info("Removed workspace owner reference from existing PVC to prevent auto-deletion", "pvcName", pvcName)
			}
		}
	}

	// If PVC already exists, also ensure isOSS is recognized if the PVC was created with an OSS storage class
	if !isOSS && pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" {
		scName := *pvc.Spec.StorageClassName
		if strings.Contains(strings.ToLower(scName), "oss") {
			isOSS = true
		} else {
			scObj := &storagev1.StorageClass{}
			if err := r.Get(ctx, client.ObjectKey{Name: scName}, scObj); err == nil {
				if scObj.Provisioner == "ossplugin.csi.alibabacloud.com" {
					isOSS = true
				}
			}
		}
	}

	// handle volume expansion / size reconciliation
	// Ensure OSS PVC has the storage-provisioner annotation so K8s admission allows resizing
	if isOSS {
		if pvc.Annotations == nil {
			pvc.Annotations = make(map[string]string)
		}
		if _, ok := pvc.Annotations["volume.kubernetes.io/storage-provisioner"]; !ok {
			pvc.Annotations["volume.kubernetes.io/storage-provisioner"] = "ossplugin.csi.alibabacloud.com"
			if err := r.Update(ctx, pvc); err != nil {
				log.Error(err, "Failed to update existing PVC annotations with storage-provisioner", "pvcName", pvcName)
			}
		}
	}

	desiredSize, err := apiresources.ParseQuantity(ws.Spec.Storage.Size)
	if err != nil {
		log.Error(err, "Invalid storage size in workspace spec", "size", ws.Spec.Storage.Size)
		if r.Recorder != nil {
			r.Recorder.Eventf(ws, corev1.EventTypeWarning, "InvalidStorageSize", "Failed to parse storage size %q: %v", ws.Spec.Storage.Size, err)
		}
		return pvc.Name, nil
	}

	currentReq, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok {
		if pvc.Spec.Resources.Requests == nil {
			pvc.Spec.Resources.Requests = make(corev1.ResourceList)
		}
		pvc.Spec.Resources.Requests[corev1.ResourceStorage] = desiredSize
		log.Info("Setting storage request on existing PVC", "pvcName", pvcName, "size", desiredSize.String())
		if err := r.Update(ctx, pvc); err != nil {
			return "", err
		}
	} else {
		cmp := desiredSize.Cmp(currentReq)
		if cmp > 0 {
			log.Info("Expanding PVC storage size", "pvcName", pvcName, "oldSize", currentReq.String(), "newSize", desiredSize.String())
			if isOSS {
				if pvc.Annotations == nil {
					pvc.Annotations = make(map[string]string)
				}
				pvc.Annotations["volume.kubernetes.io/storage-provisioner"] = "ossplugin.csi.alibabacloud.com"
			}
			pvc.Spec.Resources.Requests[corev1.ResourceStorage] = desiredSize
			if err := r.Update(ctx, pvc); err != nil {
				log.Error(err, "Failed to update PVC for expansion", "pvcName", pvcName)
				if r.Recorder != nil {
					r.Recorder.Eventf(ws, corev1.EventTypeWarning, "VolumeExpansionFailed", "Failed to expand PVC %s from %s to %s: %v", pvcName, currentReq.String(), desiredSize.String(), err)
				}
				return pvc.Name, nil
			}
			if r.Recorder != nil {
				r.Recorder.Eventf(ws, corev1.EventTypeNormal, "VolumeExpanded", "Expanded PVC %s from %s to %s", pvcName, currentReq.String(), desiredSize.String())
			}

			// For auto-provisioned OSS PVs, update PV capacity to match
			pvName := ws.Name + "-pv"
			pv := &corev1.PersistentVolume{}
			if pvErr := r.Get(ctx, client.ObjectKey{Name: pvName}, pv); pvErr == nil {
				if curCap, ok := pv.Spec.Capacity[corev1.ResourceStorage]; !ok || desiredSize.Cmp(curCap) > 0 {
					if pv.Spec.Capacity == nil {
						pv.Spec.Capacity = make(corev1.ResourceList)
					}
					pv.Spec.Capacity[corev1.ResourceStorage] = desiredSize
					log.Info("Expanding OSS PV capacity", "pvName", pvName, "newSize", desiredSize.String())
					_ = r.Update(ctx, pv)
				}
			}
		} else if cmp < 0 {
			log.Info("Storage size reduction requested but not supported by Kubernetes", "pvcName", pvcName, "currentSize", currentReq.String(), "requestedSize", desiredSize.String())
			if r.Recorder != nil {
				r.Recorder.Eventf(ws, corev1.EventTypeWarning, "VolumeShrinkNotSupported", "Shrinking storage from %s to %s is not supported by Kubernetes; ignoring size update", currentReq.String(), desiredSize.String())
			}
		}
	}

	return pvc.Name, nil
}

// buildResourceRequirements parses CPU and memory specifications and constructs
// corev1.ResourceRequirements with limits equal to the specified/default resources
// and requests set to 50% (half) of limits.
//
// Examples:
//   - cpu: "1", memory: "2" or "2Gi" -> cpu: limit 1, request 500m (0.5); memory: limit 2Gi, request 1Gi
//   - cpu: "2", memory: "4Gi"        -> cpu: limit 2, request 1; memory: limit 4Gi, request 2Gi
//   - cpu: "500m", memory: "1Gi"     -> cpu: limit 500m, request 250m; memory: limit 1Gi, request 512Mi
//   - empty (default)                -> cpu: limit 500m, request 250m; memory: limit 1Gi, request 512Mi
func buildResourceRequirements(cpuStr, memStr string) corev1.ResourceRequirements {
	cpuStr = strings.TrimSpace(cpuStr)
	if cpuStr == "" {
		cpuStr = "500m" // 0.5 CPU default limit
	}

	memStr = strings.TrimSpace(memStr)
	if memStr == "" {
		memStr = "1Gi" // 1G Memory default limit
	} else if _, err := strconv.ParseInt(memStr, 10, 64); err == nil {
		// If memStr is a plain integer without a unit (e.g. "2" for 2Gi, "1" for 1Gi), append "Gi"
		memStr = memStr + "Gi"
	}

	resources := corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{},
		Requests: corev1.ResourceList{},
	}

	if qtyCPU, err := apiresources.ParseQuantity(cpuStr); err == nil {
		resources.Limits[corev1.ResourceCPU] = qtyCPU

		// Request is 50% of Limit
		reqMilli := qtyCPU.MilliValue() / 2
		if reqMilli < 1 && qtyCPU.MilliValue() > 0 {
			reqMilli = 1
		}
		resources.Requests[corev1.ResourceCPU] = *apiresources.NewMilliQuantity(reqMilli, apiresources.DecimalSI)
	}

	if qtyMem, err := apiresources.ParseQuantity(memStr); err == nil {
		resources.Limits[corev1.ResourceMemory] = qtyMem

		// Request is 50% of Limit
		reqBytes := qtyMem.Value() / 2
		if reqBytes < 1 && qtyMem.Value() > 0 {
			reqBytes = 1
		}
		resources.Requests[corev1.ResourceMemory] = *apiresources.NewQuantity(reqBytes, qtyMem.Format)
	}

	return resources
}

func (r *WorkspaceReconciler) reconcileDeployment(ctx context.Context, ws *aiv1alpha1.Workspace, pvcName string) (string, int32, error) {
	log := logf.FromContext(ctx)
	deployName := ws.Name + "-deploy"
	deploy := &appsv1.Deployment{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: deployName}, deploy)

	// Operator always owns Deployment.spec.replicas (Stopped / IdleTimeout / default 1).
	desiredReplicas := computeDesiredReplicas(ws, log)

	// Build runtime container environment
	envVars := []corev1.EnvVar{
		{Name: "WORKSPACE_OWNER", Value: ws.Spec.Owner},
		{Name: "WORKSPACE_NAME", Value: ws.Name},
	}
	for _, env := range ws.Spec.Runtime.Env {
		envVars = append(envVars, corev1.EnvVar{Name: env.Name, Value: env.Value})
	}

	// Parse resource requirements: limit = configured/default, request = 50% limit
	resources := buildResourceRequirements(ws.Spec.Runtime.CPU, ws.Spec.Runtime.Memory)

	// Ports
	containerPort := getWorkspaceContainerPort(ws)

	// Protocol must be set explicitly: the API server defaults it to "TCP" on write and
	// persists it, so an unset Protocol here would make the Task 1.2 diff never converge
	// and trigger a spurious r.Update on every reconcile.
	ports := []corev1.ContainerPort{
		{Name: "http", ContainerPort: containerPort, Protocol: corev1.ProtocolTCP},
	}

	// Volumes and Mounts: Detect if running inside K8s (production PVC mode) or locally (developer HostPath mode)
	var volumeSource corev1.VolumeSource
	useHostPath := os.Getenv("USE_HOSTPATH") == "true" || os.Getenv("WORKSPACE_DATA_DIR") != "" || os.Getenv("KUBERNETES_SERVICE_HOST") == ""

	if useHostPath {
		dataDir := os.Getenv("WORKSPACE_DATA_DIR")
		if dataDir == "" {
			cwd, err := os.Getwd()
			if err == nil {
				dataDir = fmt.Sprintf("%s/data", cwd)
			} else {
				dataDir = "/tmp/workspace-data"
			}
		}
		hostPath := fmt.Sprintf("%s/%s", dataDir, ws.Name)

		// Pre-create host directories to prevent data loss in Docker Desktop VM ephemeral storage
		for _, sub := range []string{"workspace", "config", "share", "state", "cache"} {
			_ = os.MkdirAll(fmt.Sprintf("%s/%s", hostPath, sub), 0755)
		}

		hostPathType := corev1.HostPathDirectoryOrCreate
		volumeSource = corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: hostPath,
				Type: &hostPathType,
			},
		}
	} else {
		// Production PVC mode (NFS/NAS/Ceph dynamic provisioning)
		volumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvcName,
			},
		}
	}

	volumes := []corev1.Volume{
		{
			Name:         "workspace-data",
			VolumeSource: volumeSource,
		},
	}
	var volumeMounts []corev1.VolumeMount

	// Append pre-existing shared volume mounts if specified
	// Group by PVCName to avoid declaring multiple Volumes for the same PVC in PodSpec
	sharedVolumeMap := make(map[string]string) // maps PVCName to generated volumeName
	sharedVolCount := 0

	for _, svm := range ws.Spec.SharedVolumeMounts {
		volumeName, exists := sharedVolumeMap[svm.PVCName]
		if !exists {
			volumeName = fmt.Sprintf("shared-vol-%d", sharedVolCount)
			sharedVolumeMap[svm.PVCName] = volumeName
			sharedVolCount++

			volumes = append(volumes, corev1.Volume{
				Name: volumeName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: svm.PVCName,
					},
				},
			})
		}

		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      volumeName,
			MountPath: svm.MountPath,
			SubPath:   svm.SubPath,
			ReadOnly:  svm.ReadOnly,
		})
	}

	// Append ConfigMap volume mounts if specified
	configMapVolumeMap := make(map[string]string) // maps ConfigMapName to generated volumeName
	cmVolCount := 0

	for _, cm := range ws.Spec.ConfigMapVolumeMounts {
		volumeName, exists := configMapVolumeMap[cm.ConfigMapName]
		if !exists {
			volumeName = fmt.Sprintf("cm-vol-%d", cmVolCount)
			configMapVolumeMap[cm.ConfigMapName] = volumeName
			cmVolCount++

			defaultMode := int32(0755)
			volumes = append(volumes, corev1.Volume{
				Name: volumeName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: cm.ConfigMapName,
						},
						DefaultMode: &defaultMode,
					},
				},
			})
		}

		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      volumeName,
			MountPath: cm.MountPath,
			SubPath:   cm.SubPath,
			ReadOnly:  cm.ReadOnly,
		})
	}

	// Append workspace-data mounts without replacing SharedVolumeMounts already in volumeMounts.
	if len(ws.Spec.Runtime.VolumeMounts) > 0 {
		for _, vm := range ws.Spec.Runtime.VolumeMounts {
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      "workspace-data",
				MountPath: vm.MountPath,
				SubPath:   vm.SubPath,
			})
		}
	} else {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "workspace-data",
			MountPath: "/workspace",
			SubPath:   "workspace",
		})

		if strings.Contains(ws.Spec.Runtime.Image, "smanx/opencode") {
			volumeMounts = append(volumeMounts,
				corev1.VolumeMount{
					Name:      "workspace-data",
					MountPath: "/root/.config/opencode",
					SubPath:   "config",
				},
				corev1.VolumeMount{
					Name:      "workspace-data",
					MountPath: "/root/.local/share/opencode",
					SubPath:   "share",
				},
				corev1.VolumeMount{
					Name:      "workspace-data",
					MountPath: "/root/.local/state/opencode",
					SubPath:   "state",
				},
				corev1.VolumeMount{
					Name:      "workspace-data",
					MountPath: "/root/.cache/opencode",
					SubPath:   "cache",
				},
			)
		}
	}

	// Build custom initContainers if specified in ws.Spec.Runtime.InitContainers
	var initContainers []corev1.Container
	for _, icSpec := range ws.Spec.Runtime.InitContainers {
		var icEnvVars []corev1.EnvVar
		for _, env := range icSpec.Env {
			icEnvVars = append(icEnvVars, corev1.EnvVar{Name: env.Name, Value: env.Value})
		}

		var icVolumeMounts []corev1.VolumeMount
		for _, svm := range icSpec.SharedVolumeMounts {
			volumeName, exists := sharedVolumeMap[svm.PVCName]
			if !exists {
				volumeName = fmt.Sprintf("shared-vol-%d", sharedVolCount)
				sharedVolumeMap[svm.PVCName] = volumeName
				sharedVolCount++

				volumes = append(volumes, corev1.Volume{
					Name: volumeName,
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: svm.PVCName,
						},
					},
				})
			}

			icVolumeMounts = append(icVolumeMounts, corev1.VolumeMount{
				Name:      volumeName,
				MountPath: svm.MountPath,
				SubPath:   svm.SubPath,
				ReadOnly:  svm.ReadOnly,
			})
		}

		for _, cm := range icSpec.ConfigMapVolumeMounts {
			volumeName, exists := configMapVolumeMap[cm.ConfigMapName]
			if !exists {
				volumeName = fmt.Sprintf("cm-vol-%d", cmVolCount)
				configMapVolumeMap[cm.ConfigMapName] = volumeName
				cmVolCount++

				defaultMode := int32(0755)
				volumes = append(volumes, corev1.Volume{
					Name: volumeName,
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: cm.ConfigMapName,
							},
							DefaultMode: &defaultMode,
						},
					},
				})
			}

			icVolumeMounts = append(icVolumeMounts, corev1.VolumeMount{
				Name:      volumeName,
				MountPath: cm.MountPath,
				SubPath:   cm.SubPath,
				ReadOnly:  cm.ReadOnly,
			})
		}

		for _, vm := range icSpec.VolumeMounts {
			icVolumeMounts = append(icVolumeMounts, corev1.VolumeMount{
				Name:      "workspace-data",
				MountPath: vm.MountPath,
				SubPath:   vm.SubPath,
			})
		}

		initContainers = append(initContainers, corev1.Container{
			Name:            icSpec.Name,
			Image:           icSpec.Image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         icSpec.Command,
			Args:            icSpec.Args,
			Env:             icEnvVars,
			VolumeMounts:    icVolumeMounts,
		})
	}

	selectorLabels := buildSelectorLabels(ws)
	labels := buildWorkspaceLabels(ws)
	// Pods are the only resources that carry the managed-by marker (the shared
	// NetworkPolicy selects on it); PVCs/Services/Ingresses must stay clean.
	podLabels := buildPodLabels(ws)

	var command []string
	var containerArgs []string
	if len(ws.Spec.Runtime.Command) > 0 || len(ws.Spec.Runtime.Args) > 0 {
		command = ws.Spec.Runtime.Command
		containerArgs = ws.Spec.Runtime.Args
	} else if strings.Contains(ws.Spec.Runtime.Image, "smanx/opencode") {
		command = []string{"/bin/bash", "-c"}
		containerArgs = []string{"cd /workspace && exec /entrypoint.sh"}
	}

	var lifecycle *corev1.Lifecycle
	if ws.Spec.Runtime.PostStartScript != "" {
		lifecycle = &corev1.Lifecycle{
			PostStart: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"/bin/bash", "-c", ws.Spec.Runtime.PostStartScript},
				},
			},
		}
	}

	if err != nil {
		if apierrors.IsNotFound(err) {
			deploy = &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deployName,
					Namespace: ws.Namespace,
					Labels:    labels,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &desiredReplicas,
					Strategy: appsv1.DeploymentStrategy{
						Type: appsv1.RecreateDeploymentStrategyType,
					},
					Selector: &metav1.LabelSelector{
						MatchLabels: selectorLabels,
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: podLabels,
						},
						Spec: corev1.PodSpec{
							RuntimeClassName:              ws.Spec.Runtime.RuntimeClassName,
							InitContainers:                initContainers,
							TerminationGracePeriodSeconds: int64Ptr(2),
							AutomountServiceAccountToken:  boolPtr(false),
							EnableServiceLinks:            boolPtr(false),
							Containers: []corev1.Container{
								{
									Name:            "runtime",
									Image:           ws.Spec.Runtime.Image,
									ImagePullPolicy: corev1.PullIfNotPresent,
									Command:         command,
									Args:            containerArgs,
									Ports:           ports,
									Env:             envVars,
									Resources:       resources,
									VolumeMounts:    volumeMounts,
									Lifecycle:       lifecycle,
									StartupProbe:    getStartupProbe(ws),
									ReadinessProbe:  getReadinessProbe(ws),
								},
							},
							Volumes: volumes,
						},
					},
				},
			}
			if err := ctrl.SetControllerReference(ws, deploy, r.Scheme); err != nil {
				return "", 0, err
			}
			if err := r.Create(ctx, deploy); err != nil {
				return "", 0, err
			}
			return deployName, desiredReplicas, nil
		}
		return "", 0, err
	}

	// Update replica count and other spec settings if changed
	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		return "", 0, fmt.Errorf("deployment %s has no containers", deployName)
	}

	needsUpdate := false
	if deploy.Spec.Replicas == nil || *deploy.Spec.Replicas != desiredReplicas {
		deploy.Spec.Replicas = &desiredReplicas
		needsUpdate = true
	}
	if deploy.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		deploy.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RecreateDeploymentStrategyType,
		}
		needsUpdate = true
	}
	if !maps.Equal(deploy.Labels, labels) {
		deploy.Labels = labels
		needsUpdate = true
	}
	if !maps.Equal(deploy.Spec.Template.Labels, podLabels) {
		deploy.Spec.Template.Labels = podLabels
		needsUpdate = true
	}

	podSpec := &deploy.Spec.Template.Spec
	if !reflect.DeepEqual(podSpec.RuntimeClassName, ws.Spec.Runtime.RuntimeClassName) {
		podSpec.RuntimeClassName = ws.Spec.Runtime.RuntimeClassName
		needsUpdate = true
	}
	if podSpec.TerminationGracePeriodSeconds == nil || *podSpec.TerminationGracePeriodSeconds != 2 {
		podSpec.TerminationGracePeriodSeconds = int64Ptr(2)
		needsUpdate = true
	}
	if podSpec.AutomountServiceAccountToken == nil || *podSpec.AutomountServiceAccountToken {
		podSpec.AutomountServiceAccountToken = boolPtr(false)
		needsUpdate = true
	}
	if podSpec.EnableServiceLinks == nil || *podSpec.EnableServiceLinks {
		podSpec.EnableServiceLinks = boolPtr(false)
		needsUpdate = true
	}

	targetContainer := &podSpec.Containers[0]
	if targetContainer.Image != ws.Spec.Runtime.Image {
		targetContainer.Image = ws.Spec.Runtime.Image
		needsUpdate = true
	}
	if targetContainer.ImagePullPolicy != corev1.PullIfNotPresent {
		targetContainer.ImagePullPolicy = corev1.PullIfNotPresent
		needsUpdate = true
	}
	if !slices.Equal(targetContainer.Command, command) {
		targetContainer.Command = command
		needsUpdate = true
	}
	if !slices.Equal(targetContainer.Args, containerArgs) {
		targetContainer.Args = containerArgs
		needsUpdate = true
	}
	if !equality.Semantic.DeepEqual(targetContainer.Env, envVars) {
		targetContainer.Env = envVars
		needsUpdate = true
	}
	if !equality.Semantic.DeepEqual(targetContainer.Resources, resources) {
		targetContainer.Resources = resources
		needsUpdate = true
	}
	if !equality.Semantic.DeepEqual(targetContainer.Ports, ports) {
		targetContainer.Ports = ports
		needsUpdate = true
	}
	if !equality.Semantic.DeepEqual(targetContainer.VolumeMounts, volumeMounts) {
		targetContainer.VolumeMounts = volumeMounts
		needsUpdate = true
	}
	if !equality.Semantic.DeepEqual(targetContainer.Lifecycle, lifecycle) {
		targetContainer.Lifecycle = lifecycle
		needsUpdate = true
	}
	startupProbe := getStartupProbe(ws)
	if !equality.Semantic.DeepEqual(targetContainer.StartupProbe, startupProbe) {
		targetContainer.StartupProbe = startupProbe
		needsUpdate = true
	}
	readinessProbe := getReadinessProbe(ws)
	if !equality.Semantic.DeepEqual(targetContainer.ReadinessProbe, readinessProbe) {
		targetContainer.ReadinessProbe = readinessProbe
		needsUpdate = true
	}
	if !equality.Semantic.DeepEqual(podSpec.InitContainers, initContainers) {
		podSpec.InitContainers = initContainers
		needsUpdate = true
	}
	if !equality.Semantic.DeepEqual(podSpec.Volumes, volumes) {
		podSpec.Volumes = volumes
		needsUpdate = true
	}

	if needsUpdate {
		log.Info("Updating Deployment spec due to changes", "deployment", deployName)
		if err := r.Update(ctx, deploy); err != nil {
			return "", 0, err
		}
	}

	return deploy.Name, desiredReplicas, nil
}

// computeDesiredReplicas returns the desired Deployment replica count.
//
//   - Stopped=true → 0
//   - IdleTimeout set and lastActiveTime + idleTimeout elapsed → 0
//   - otherwise → 1
func computeDesiredReplicas(ws *aiv1alpha1.Workspace, log interface {
	Error(err error, msg string, keysAndValues ...any)
}) int32 {
	if ws.Spec.Stopped {
		return 0
	}
	if ws.Spec.IdleTimeout != "" && ws.Status.LastActiveTime != nil {
		idleDuration, err := time.ParseDuration(ws.Spec.IdleTimeout)
		if err != nil {
			log.Error(err, "Failed to parse idleTimeout in replica calculation", "value", ws.Spec.IdleTimeout)
			return 1
		}
		if time.Now().After(ws.Status.LastActiveTime.Add(idleDuration)) {
			return 0
		}
	}
	return 1
}

func (r *WorkspaceReconciler) reconcileService(ctx context.Context, ws *aiv1alpha1.Workspace) (string, error) {
	svcName := ws.Name + "-svc"
	svc := &corev1.Service{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: svcName}, svc)

	// Protocol must match the API-server default ("TCP") so the Task 1.2 diff converges.
	ports := []corev1.ServicePort{
		{
			Name:       "http",
			Port:       80,
			TargetPort: intstr.FromString("http"),
			Protocol:   corev1.ProtocolTCP,
		},
	}

	selectorLabels := buildSelectorLabels(ws)
	labels := buildWorkspaceLabels(ws)

	if err != nil {
		if apierrors.IsNotFound(err) {
			svc = &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      svcName,
					Namespace: ws.Namespace,
					Labels:    labels,
				},
				Spec: corev1.ServiceSpec{
					Selector: selectorLabels,
					Ports:    ports,
					Type:     corev1.ServiceTypeClusterIP,
				},
			}
			if err := ctrl.SetControllerReference(ws, svc, r.Scheme); err != nil {
				return "", err
			}
			if err := r.Create(ctx, svc); err != nil {
				return "", err
			}
			return svcName, nil
		}
		return "", err
	}

	needsUpdate := false
	if !maps.Equal(svc.Labels, labels) {
		svc.Labels = labels
		needsUpdate = true
	}
	if !maps.Equal(svc.Spec.Selector, selectorLabels) {
		svc.Spec.Selector = selectorLabels
		needsUpdate = true
	}
	if !equality.Semantic.DeepEqual(svc.Spec.Ports, ports) {
		svc.Spec.Ports = ports
		needsUpdate = true
	}
	if needsUpdate {
		log := logf.FromContext(ctx)
		log.Info("Updating Service spec due to changes", "service", svcName)
		if err := r.Update(ctx, svc); err != nil {
			return "", err
		}
	}
	return svc.Name, nil
}

func (r *WorkspaceReconciler) reconcileIngress(ctx context.Context, ws *aiv1alpha1.Workspace, svcName string) (string, error) {
	ingressName := ws.Name + "-ingress"
	ingress := &networkingv1.Ingress{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: ingressName}, ingress)

	host := getWorkspaceHost(ws.Name)
	pathType := networkingv1.PathTypePrefix

	ingressClassName := "nginx"
	ingressSpec := networkingv1.IngressSpec{
		IngressClassName: &ingressClassName,
		Rules: []networkingv1.IngressRule{
			{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{
							{
								Path:     "/",
								PathType: &pathType,
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: svcName,
										Port: networkingv1.ServiceBackendPort{
											Number: 80,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	labels := buildWorkspaceLabels(ws)

	if err != nil {
		if apierrors.IsNotFound(err) {
			ingress = &networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ingressName,
					Namespace: ws.Namespace,
					Labels:    labels,
					Annotations: map[string]string{
						"nginx.ingress.kubernetes.io/ssl-redirect":              "false",
						"nginx.ingress.kubernetes.io/proxy-connect-timeout":     "10",
						"nginx.ingress.kubernetes.io/proxy-body-size":           "100m",
						"nginx.ingress.kubernetes.io/proxy-read-timeout":        "86400",
						"nginx.ingress.kubernetes.io/proxy-send-timeout":        "86400",
						"nginx.ingress.kubernetes.io/proxy-buffering":           "off",
						"nginx.ingress.kubernetes.io/proxy-http-version":        "1.1",
						"nginx.ingress.kubernetes.io/limit-connections":         "200",
						"nginx.ingress.kubernetes.io/proxy-next-upstream":       "error timeout invalid_header http_502 http_503",
						"nginx.ingress.kubernetes.io/proxy-next-upstream-tries": "3",
					},
				},
				Spec: ingressSpec,
			}
			if err := ctrl.SetControllerReference(ws, ingress, r.Scheme); err != nil {
				return "", err
			}
			if err := r.Create(ctx, ingress); err != nil {
				return "", err
			}
			return host, nil
		}
		return "", err
	}

	needsUpdate := false
	if !maps.Equal(ingress.Labels, labels) {
		ingress.Labels = labels
		needsUpdate = true
	}
	if ingress.Annotations == nil {
		ingress.Annotations = make(map[string]string)
		needsUpdate = true
	}
	expectedAnnotations := map[string]string{
		"nginx.ingress.kubernetes.io/ssl-redirect":              "false",
		"nginx.ingress.kubernetes.io/proxy-connect-timeout":     "10",
		"nginx.ingress.kubernetes.io/proxy-body-size":           "100m",
		"nginx.ingress.kubernetes.io/proxy-read-timeout":        "86400",
		"nginx.ingress.kubernetes.io/proxy-send-timeout":        "86400",
		"nginx.ingress.kubernetes.io/proxy-buffering":           "off",
		"nginx.ingress.kubernetes.io/proxy-http-version":        "1.1",
		"nginx.ingress.kubernetes.io/limit-connections":         "200",
		"nginx.ingress.kubernetes.io/proxy-next-upstream":       "error timeout invalid_header http_502 http_503",
		"nginx.ingress.kubernetes.io/proxy-next-upstream-tries": "3",
	}
	for k, v := range expectedAnnotations {
		if ingress.Annotations[k] != v {
			ingress.Annotations[k] = v
			needsUpdate = true
		}
	}
	if !equality.Semantic.DeepEqual(ingress.Spec, ingressSpec) {
		ingress.Spec = ingressSpec
		needsUpdate = true
	}
	if needsUpdate {
		log := logf.FromContext(ctx)
		log.Info("Updating Ingress spec due to changes", "ingress", ingressName)
		if err := r.Update(ctx, ingress); err != nil {
			return "", err
		}
	}
	return host, nil
}

func isNetworkPolicyDisabled(ws *aiv1alpha1.Workspace) bool {
	if ws.Spec.DisableNetworkPolicy {
		return true
	}
	if ws.Spec.NetworkPolicy == nil {
		return false
	}
	return ws.Spec.NetworkPolicy.Disabled
}

func getWorkspaceContainerPort(ws *aiv1alpha1.Workspace) int32 {
	if ws.Spec.Runtime.Port != 0 {
		return ws.Spec.Runtime.Port
	}
	if strings.Contains(ws.Spec.Runtime.Image, "nginx") {
		return 80
	}
	return 8080
}

func (r *WorkspaceReconciler) reconcileNetworkPolicy(ctx context.Context, ws *aiv1alpha1.Workspace) error {
	log := logf.FromContext(ctx)

	// Remove the legacy per-workspace NetworkPolicy from older operator versions.
	// The object carries an owner reference to the Workspace, so garbage collection
	// handles it on Workspace deletion; this Get/Delete pair cleans up leftovers in
	// namespaces where the Workspace still exists.
	legacyName := ws.Name + legacyNetpolSuffix
	legacyNetpol := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: legacyName}, legacyNetpol)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		log.Info("Deleting legacy per-workspace NetworkPolicy", "netpolName", legacyName)
		if err := r.Delete(ctx, legacyNetpol); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	// The shared template is static per namespace. Per-workspace egress customization
	// can no longer be projected into it: merging one Workspace's exceptions into the
	// shared policy would widen access for EVERY Workspace in the namespace. The
	// controller-wide exception list (--netpol-allowed-namespaces) still applies.
	if ws.Spec.NetworkPolicy != nil &&
		(len(ws.Spec.NetworkPolicy.BlockedCIDRs) > 0 ||
			len(ws.Spec.NetworkPolicy.AllowedCIDRs) > 0 ||
			len(ws.Spec.NetworkPolicy.AllowedNamespaces) > 0) {
		log.Info("Ignoring per-workspace NetworkPolicy customization because a single shared namespace-level NetworkPolicy template is used; configure controller-wide egress exceptions with --netpol-allowed-namespaces instead",
			"workspace", ws.Name, "namespace", ws.Namespace)
		if r.Recorder != nil {
			// Static message on purpose: the apiserver aggregates repeats with the
			// same reason and message into a single Event, so continuous
			// reconciliation does not flood the event stream.
			r.Recorder.Eventf(ws, corev1.EventTypeWarning, "NetworkPolicyOverridden",
				"spec.networkPolicy.{blockedCIDRs,allowedCIDRs,allowedNamespaces} is ignored: a single shared namespace-level NetworkPolicy template is used; configure controller-wide egress exceptions with --netpol-allowed-namespaces")
		}
	}

	return r.ensureSharedNetworkPolicy(ctx, ws.Namespace)
}

// ensureSharedNetworkPolicy creates or converges the single static namespace-level
// NetworkPolicy template. The object is deliberately NOT owned by any Workspace so it
// survives individual Workspace deletions; it is garbage collected with the namespace
// or removed by the Workspace deletion path when the last Workspace goes away.
func (r *WorkspaceReconciler) ensureSharedNetworkPolicy(ctx context.Context, namespace string) error {
	log := logf.FromContext(ctx)

	desiredSpec := networkingv1.NetworkPolicySpec{
		// Select every Pod the operator manages in this namespace (Task 2.1).
		PodSelector: metav1.LabelSelector{
			MatchLabels: map[string]string{managedByLabelKey: managedByLabelValue},
		},
		PolicyTypes: []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress,
			networkingv1.PolicyTypeEgress,
		},
		// Ingress stays open for the selected Pods: the application port differs per
		// Workspace and the Ingress controller may SNAT or use host networking, so a
		// static template cannot pin a port without breaking traffic. The security
		// boundary of the template is egress isolation.
		Ingress: []networkingv1.NetworkPolicyIngressRule{{}},
		Egress:  buildSharedEgressRules(r.GlobalAllowedNamespaces),
	}

	expectedLabels := map[string]string{
		"app":             "workspace",
		managedByLabelKey: managedByLabelValue,
	}

	netpol := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: sharedNetpolName}, netpol)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		netpol = &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sharedNetpolName,
				Namespace: namespace,
				Labels:    expectedLabels,
			},
			Spec: desiredSpec,
		}
		log.Info("Creating shared namespace-level NetworkPolicy template", "netpol", sharedNetpolName, "namespace", namespace)
		if err := r.Create(ctx, netpol); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		return nil
	}

	needsUpdate := false
	if !maps.Equal(netpol.Labels, expectedLabels) {
		netpol.Labels = expectedLabels
		needsUpdate = true
	}
	if !equality.Semantic.DeepEqual(netpol.Spec, desiredSpec) {
		netpol.Spec = desiredSpec
		needsUpdate = true
	}
	if needsUpdate {
		log.Info("Updating shared NetworkPolicy template due to changes", "netpol", sharedNetpolName, "namespace", namespace)
		if err := r.Update(ctx, netpol); err != nil {
			if apierrors.IsConflict(err) {
				// Another worker converged the same template concurrently (the
				// template is namespace-scoped). The NetworkPolicy watch will
				// enqueue a Workspace again and the next attempt re-converges, so
				// a conflict here must not become a reconcile error / backoff.
				log.Info("Shared NetworkPolicy template update conflicted; deferring convergence to the next reconcile", "netpol", sharedNetpolName, "namespace", namespace)
				return nil
			}
			return err
		}
		return nil
	}
	return nil
}

// buildSharedEgressRules builds the static egress section of the shared NetworkPolicy
// template: DNS, the public Internet with private ranges excluded, and the
// controller-wide namespace egress exceptions (--netpol-allowed-namespaces).
func buildSharedEgressRules(globalAllowedNamespaces []aiv1alpha1.WorkspaceNetworkPolicyNamespaceRule) []networkingv1.NetworkPolicyEgressRule {
	tcpProtocol := corev1.ProtocolTCP
	udpProtocol := corev1.ProtocolUDP

	egressRules := []networkingv1.NetworkPolicyEgressRule{
		// 1. Allow DNS lookups (UDP and TCP on port 53)
		{
			Ports: []networkingv1.NetworkPolicyPort{
				{
					Protocol: &udpProtocol,
					Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: 53},
				},
				{
					Protocol: &tcpProtocol,
					Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: 53},
				},
			},
		},
		// 2. Allow public Internet egress while excluding the private, carrier-grade
		//    NAT, and link-local ranges that cover the common Pod, Service, node, VPC,
		//    and cloud metadata address spaces.
		{
			To: []networkingv1.NetworkPolicyPeer{
				{
					IPBlock: &networkingv1.IPBlock{
						CIDR: "0.0.0.0/0",
						Except: []string{
							"10.0.0.0/8",
							"100.64.0.0/10",
							"169.254.0.0/16",
							"172.16.0.0/12",
							"192.168.0.0/16",
						},
					},
				},
			},
		},
	}

	// 3. Controller-wide namespace-scoped egress exceptions (e.g. the shared OTel
	//    collector in the "monitoring" namespace), so every workspace can reach shared
	//    in-cluster services inside the excluded private ranges. Rules are matched by
	//    namespace/pod labels and stay valid when target pods move.
	seenNsRules := make(map[string]struct{}, len(globalAllowedNamespaces))
	for _, nsRule := range globalAllowedNamespaces {
		key := netpolNamespaceRuleKey(nsRule)
		if _, dup := seenNsRules[key]; dup {
			continue
		}
		seenNsRules[key] = struct{}{}
		if egressRule, ok := buildNamespaceEgressRule(nsRule); ok {
			egressRules = append(egressRules, egressRule)
		}
	}
	return egressRules
}

// maybeDeleteSharedNetworkPolicy garbage-collects the shared namespace-level
// NetworkPolicy template when no live (non-deleting) Workspace remains in the
// namespace, keeping the cluster-wide policy count tied to active tenant namespaces.
//
// gone is the Workspace being reconciled after it disappeared. It is skipped
// explicitly because the cached List below can still contain the object for a
// short while after the API server deleted it; without this the template would
// never be considered empty on the reconcile that observes the deletion.
func (r *WorkspaceReconciler) maybeDeleteSharedNetworkPolicy(ctx context.Context, namespace string, gone types.NamespacedName) error {
	wsList := &aiv1alpha1.WorkspaceList{}
	if err := r.List(ctx, wsList, client.InNamespace(namespace)); err != nil {
		return err
	}
	for i := range wsList.Items {
		item := &wsList.Items[i]
		if item.DeletionTimestamp.IsZero() && item.Name != gone.Name {
			return nil
		}
	}
	sharedNetpol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: sharedNetpolName, Namespace: namespace},
	}
	if err := r.Delete(ctx, sharedNetpol); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// netpolNamespaceRuleKey builds a deterministic dedupe key for a namespace egress rule.
func netpolNamespaceRuleKey(rule aiv1alpha1.WorkspaceNetworkPolicyNamespaceRule) string {
	selectorLabels := make([]string, 0, len(rule.PodSelector))
	for k, v := range rule.PodSelector {
		selectorLabels = append(selectorLabels, k+"="+v)
	}
	slices.Sort(selectorLabels)
	ports := make([]string, 0, len(rule.Ports))
	for _, port := range rule.Ports {
		ports = append(ports, strconv.Itoa(int(port)))
	}
	slices.Sort(ports)
	return strings.TrimSpace(rule.Namespace) + "|" + strings.Join(selectorLabels, ",") + "|" + strings.Join(ports, ",")
}

// buildNamespaceEgressRule translates a namespace egress exception into a NetworkPolicy
// egress rule. It returns false when the rule has no usable destination (empty namespace).
func buildNamespaceEgressRule(nsRule aiv1alpha1.WorkspaceNetworkPolicyNamespaceRule) (networkingv1.NetworkPolicyEgressRule, bool) {
	ns := strings.TrimSpace(nsRule.Namespace)
	if ns == "" {
		return networkingv1.NetworkPolicyEgressRule{}, false
	}
	tcpProtocol := corev1.ProtocolTCP
	peer := networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns},
		},
	}
	if len(nsRule.PodSelector) > 0 {
		peer.PodSelector = &metav1.LabelSelector{MatchLabels: nsRule.PodSelector}
	}
	egressRule := networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{peer},
	}
	for _, port := range nsRule.Ports {
		if port < 1 || port > 65535 {
			continue
		}
		portVal := intstr.FromInt32(port)
		egressRule.Ports = append(egressRule.Ports, networkingv1.NetworkPolicyPort{
			Protocol: &tcpProtocol,
			Port:     &portVal,
		})
	}
	return egressRule, true
}

func workspacePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}
			oldWs, ok1 := e.ObjectOld.(*aiv1alpha1.Workspace)
			newWs, ok2 := e.ObjectNew.(*aiv1alpha1.Workspace)
			if !ok1 || !ok2 {
				return false
			}
			// 1. Generation changed (Spec change)
			if oldWs.GetGeneration() != newWs.GetGeneration() {
				return true
			}
			// 2. DeletionTimestamp changed
			if !oldWs.DeletionTimestamp.Equal(newWs.DeletionTimestamp) {
				return true
			}
			// 3. Finalizers changed
			if !slices.Equal(oldWs.Finalizers, newWs.Finalizers) {
				return true
			}
			// 4. Labels changed (e.g. "oa" label or custom user labels)
			if !maps.Equal(oldWs.Labels, newWs.Labels) {
				return true
			}
			// 5. LastActiveTime changed (e.g. resumed/woken up via API)
			oldActive := oldWs.Status.LastActiveTime
			newActive := newWs.Status.LastActiveTime
			if (oldActive == nil) != (newActive == nil) {
				return true
			}
			if oldActive != nil && newActive != nil && !oldActive.Time.Equal(newActive.Time) {
				return true
			}
			// Ignore other status-only updates from the controller itself (Phase, PodName, Endpoint, PVCName)
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return true
		},
	}
}

func deploymentPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return false
			}
			oldDeploy, ok1 := e.ObjectOld.(*appsv1.Deployment)
			newDeploy, ok2 := e.ObjectNew.(*appsv1.Deployment)
			if !ok1 || !ok2 {
				return false
			}
			// Reconcile when ReadyReplicas changes (Pod becomes ready or drops to 0)
			if oldDeploy.Status.ReadyReplicas != newDeploy.Status.ReadyReplicas {
				return true
			}
			// Reconcile if Deployment Generation changed
			if oldDeploy.GetGeneration() != newDeploy.GetGeneration() {
				return true
			}
			// Reconcile if DeletionTimestamp changed
			if !oldDeploy.DeletionTimestamp.Equal(newDeploy.DeletionTimestamp) {
				return true
			}
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return true
		},
	}
}

func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Register field indexer for pod lookup by workspace label (Task 1.5, O(1) pod lookup)
	if mgr.GetFieldIndexer() != nil {
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, podWorkspaceIndexKey, func(rawObj client.Object) []string {
			pod, ok := rawObj.(*corev1.Pod)
			if !ok {
				return nil
			}
			if wsName, exists := pod.Labels["workspace"]; exists && wsName != "" {
				return []string{wsName}
			}
			return nil
		}); err != nil {
			logf.Log.Error(err, "Failed to register pod workspace field index; falling back to label-selector pod lists")
		}
	}

	// Register the Workspace observability metrics on the manager metrics
	// endpoint (Task 5.2).
	registerWorkspaceMetrics()

	maxConcurrent := r.MaxConcurrentReconciles
	if maxConcurrent <= 0 {
		maxConcurrent = 30
	}

	controllerOptions := controller.Options{MaxConcurrentReconciles: maxConcurrent}

	// Run the periodic workspace_active_instances resync alongside the controller.
	// It runs under leader election like the controller itself.
	if err := mgr.Add(r); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&aiv1alpha1.Workspace{}, builder.WithPredicates(workspacePredicate())).
		Owns(&appsv1.Deployment{}, builder.WithPredicates(deploymentPredicate())).
		Owns(&corev1.Service{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&networkingv1.Ingress{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// The shared namespace-level NetworkPolicy template is not owned by any single
		// Workspace, so a mapped watch repairs it across all Workspaces of the
		// namespace whenever it drifts or is deleted (Task 2.1).
		Watches(&networkingv1.NetworkPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.mapNetworkPolicyToWorkspaces),
			builder.WithPredicates(sharedNetpolPredicate())).
		WithOptions(controllerOptions).
		Named("workspace").
		Complete(r)
}

// Start implements manager.Runnable: it periodically rebuilds the
// workspace_active_instances gauge from a cluster-wide Workspace list so the
// metric stays correct even when reconciles fail or objects are modified out of
// band. Per-reconcile updates keep the gauge fresh in between syncs.
func (r *WorkspaceReconciler) Start(ctx context.Context) error {
	interval := r.ActiveInstancesSyncInterval
	if interval <= 0 {
		interval = defaultActiveInstancesSyncInterval
	}

	log := logf.FromContext(ctx).WithValues("runnable", "workspace-metrics-sync")
	log.Info("Starting Workspace active instances metric syncer", "interval", interval.String())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		r.syncActiveInstances(ctx)
		select {
		case <-ctx.Done():
			log.Info("Stopping Workspace active instances metric syncer")
			return nil
		case <-ticker.C:
		}
	}
}

// syncActiveInstances lists all Workspaces and resets the gauge from the snapshot.
func (r *WorkspaceReconciler) syncActiveInstances(ctx context.Context) {
	log := logf.FromContext(ctx)
	wsList := &aiv1alpha1.WorkspaceList{}
	if err := r.List(ctx, wsList); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Error(err, "Failed to list Workspaces for active instances metric sync")
		return
	}
	phases := make(map[types.NamespacedName]aiv1alpha1.WorkspacePhase, len(wsList.Items))
	for i := range wsList.Items {
		phases[types.NamespacedName{Namespace: wsList.Items[i].Namespace, Name: wsList.Items[i].Name}] = wsList.Items[i].Status.Phase
	}
	workspacePhases.resetAll(phases)
}

// sharedNetpolPredicate filters NetworkPolicy events down to the shared
// namespace-level template so legacy per-workspace policy churn does not fan out
// into Workspace reconciles.
func sharedNetpolPredicate() predicate.Predicate {
	isShared := func(obj client.Object) bool {
		return obj != nil && obj.GetName() == sharedNetpolName
	}
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return isShared(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return isShared(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !isShared(e.ObjectOld) || !isShared(e.ObjectNew) {
				return false
			}
			oldNetpol, ok1 := e.ObjectOld.(*networkingv1.NetworkPolicy)
			newNetpol, ok2 := e.ObjectNew.(*networkingv1.NetworkPolicy)
			if !ok1 || !ok2 {
				return false
			}
			// Only react to content changes of the shared template.
			return !equality.Semantic.DeepEqual(oldNetpol.Labels, newNetpol.Labels) ||
				!equality.Semantic.DeepEqual(oldNetpol.Spec, newNetpol.Spec)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return isShared(e.Object)
		},
	}
}

// mapNetworkPolicyToWorkspaces enqueues a single representative Workspace so the
// shared template is re-converged (and recreated when deleted). The template is
// namespace-scoped, so one reconcile is enough; enqueuing every Workspace turned
// one template change into an N-request burst that also made concurrent workers
// fight over the same Update (conflict storms).
func (r *WorkspaceReconciler) mapNetworkPolicyToWorkspaces(ctx context.Context, obj client.Object) []reconcile.Request {
	wsList := &aiv1alpha1.WorkspaceList{}
	if err := r.List(ctx, wsList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	names := make([]string, 0, len(wsList.Items))
	keys := make(map[string]types.NamespacedName, len(wsList.Items))
	for i := range wsList.Items {
		item := &wsList.Items[i]
		if !item.DeletionTimestamp.IsZero() {
			continue
		}
		names = append(names, item.Name)
		keys[item.Name] = client.ObjectKeyFromObject(item)
	}
	if len(names) == 0 {
		return nil
	}
	// Deterministic pick (smallest name) so every replica maps the same event to
	// the same request.
	return []reconcile.Request{{NamespacedName: keys[slices.Min(names)]}}
}

func boolPtr(b bool) *bool {
	return &b
}

func int64Ptr(i int64) *int64 {
	return &i
}

func getWorkspaceHost(wsName string) string {
	domain := os.Getenv("WORKSPACE_DOMAIN")
	if domain == "" {
		domain = "localhost"
	}
	return fmt.Sprintf("%s.%s", wsName, domain)
}

func getProbeHandler(ws *aiv1alpha1.Workspace) corev1.ProbeHandler {
	if ws.Spec.Runtime.HealthPath != "" {
		return corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: ws.Spec.Runtime.HealthPath,
				Port: intstr.FromString("http"),
				// Scheme must match the API-server default ("HTTP") so the
				// Task 1.2 diff converges instead of updating on every reconcile.
				Scheme: corev1.URISchemeHTTP,
			},
		}
	}
	return corev1.ProbeHandler{
		TCPSocket: &corev1.TCPSocketAction{
			Port: intstr.FromString("http"),
		},
	}
}

func getStartupProbe(ws *aiv1alpha1.Workspace) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        getProbeHandler(ws),
		InitialDelaySeconds: 1,
		TimeoutSeconds:      1,
		PeriodSeconds:       2,
		SuccessThreshold:    1,
		FailureThreshold:    30,
	}
}

func getReadinessProbe(ws *aiv1alpha1.Workspace) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        getProbeHandler(ws),
		InitialDelaySeconds: 1,
		TimeoutSeconds:      1,
		PeriodSeconds:       2,
		SuccessThreshold:    1,
		FailureThreshold:    3,
	}
}

// getOAValueFromEnv extracts the OA value from environment variables if present.
func getOAValueFromEnv(envs []aiv1alpha1.EnvVar) string {
	for _, env := range envs {
		if strings.EqualFold(env.Name, "OA") && strings.TrimSpace(env.Value) != "" {
			return strings.TrimSpace(env.Value)
		}
	}
	return ""
}

// buildSelectorLabels returns the immutable selector labels used to identify workspace pods.
func buildSelectorLabels(ws *aiv1alpha1.Workspace) map[string]string {
	return map[string]string{
		"app":       "workspace",
		"workspace": ws.Name,
	}
}

// buildWorkspaceLabels returns the labels for child resources (Deployment,
// Service, Ingress, PVC). It deliberately does NOT carry the managed-by marker:
// that marker is only meaningful on Pods (it is the shared NetworkPolicy
// selector), so it is added by buildPodLabels.
func buildWorkspaceLabels(ws *aiv1alpha1.Workspace) map[string]string {
	labels := make(map[string]string, len(ws.Labels)+3)
	maps.Copy(labels, ws.Labels)
	// Enforce built-in reserved keys
	labels["app"] = "workspace"
	labels["workspace"] = ws.Name
	if _, ok := labels["oa"]; !ok && ws.Namespace == "bocomwork" {
		if oa := getOAValueFromEnv(ws.Spec.Runtime.Env); oa != "" {
			labels["oa"] = oa
		}
	}
	return labels
}

// buildPodLabels returns the labels for Workspace Pods: the shared child labels
// plus the managed-by marker selected by the namespace-level NetworkPolicy.
// A Workspace that opted out of network isolation must never carry the marker,
// even when the user supplied the same key in the CR labels — hence the explicit
// delete instead of "just don't add".
func buildPodLabels(ws *aiv1alpha1.Workspace) map[string]string {
	labels := buildWorkspaceLabels(ws)
	if isNetworkPolicyDisabled(ws) {
		delete(labels, managedByLabelKey)
		return labels
	}
	labels[managedByLabelKey] = managedByLabelValue
	return labels
}
