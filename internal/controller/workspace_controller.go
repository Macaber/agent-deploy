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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

const (
	// workspaceFinalizer 旧版本遗留 finalizer，仅用于清理升级兼容，不再向新资源添加
	workspaceFinalizer = "ai.example.com/workspace-cleanup"

	// podWorkspaceIndexKey is the field index key for looking up workspace pods in O(1)
	podWorkspaceIndexKey = "metadata.workspace"
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

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch Workspace
	ws := &aiv1alpha1.Workspace{}
	if err := r.Get(ctx, req.NamespacedName, ws); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 删除处理：不自动删除 PV 和 PVC（包括 OSS 方案），保留持久化存储与底层数据。
	// 若存在旧版本遗留的 finalizer，清理移除以便 Workspace 正常删除，但不执行 PV/PVC/OSS 数据清理。
	if !ws.DeletionTimestamp.IsZero() {
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
							Labels: labels,
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
	if !maps.Equal(deploy.Spec.Template.Labels, labels) {
		deploy.Spec.Template.Labels = labels
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
	if podSpec.AutomountServiceAccountToken == nil || *podSpec.AutomountServiceAccountToken != false {
		podSpec.AutomountServiceAccountToken = boolPtr(false)
		needsUpdate = true
	}
	if podSpec.EnableServiceLinks == nil || *podSpec.EnableServiceLinks != false {
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

func getBlockedEgressCIDRs(ws *aiv1alpha1.Workspace) []string {
	// Exclude private, carrier-grade NAT, and link-local networks from the
	// otherwise public Internet egress rule. These ranges cover the common Pod,
	// Service, node, VPC, and cloud metadata address spaces.
	cidrs := []string{
		"10.0.0.0/8",
		"100.64.0.0/10",
		"169.254.0.0/16",
		"172.16.0.0/12",
		"192.168.0.0/16",
	}
	seen := make(map[string]struct{}, len(cidrs))
	for _, cidr := range cidrs {
		seen[cidr] = struct{}{}
	}

	if ws.Spec.NetworkPolicy != nil {
		for _, cidr := range ws.Spec.NetworkPolicy.BlockedCIDRs {
			trimmed := strings.TrimSpace(cidr)
			if trimmed == "" {
				continue
			}
			if _, exists := seen[trimmed]; exists {
				continue
			}
			cidrs = append(cidrs, trimmed)
			seen[trimmed] = struct{}{}
		}
	}
	return cidrs
}

func (r *WorkspaceReconciler) reconcileNetworkPolicy(ctx context.Context, ws *aiv1alpha1.Workspace) error {
	log := logf.FromContext(ctx)
	netpolName := ws.Name + "-netpol"
	netpol := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: netpolName}, netpol)

	if isNetworkPolicyDisabled(ws) {
		if err == nil {
			log.Info("Deleting NetworkPolicy because NetworkPolicy is disabled", "netpolName", netpolName)
			return r.Delete(ctx, netpol)
		}
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	selectorLabels := buildSelectorLabels(ws)
	labels := buildWorkspaceLabels(ws)

	tcpProtocol := corev1.ProtocolTCP
	udpProtocol := corev1.ProtocolUDP
	containerPort := intstr.FromInt32(getWorkspaceContainerPort(ws))

	blockedCIDRs := getBlockedEgressCIDRs(ws)

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
	}

	// 2. Allow public Internet egress while excluding Pod/Service/VPC networks.
	egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			{
				IPBlock: &networkingv1.IPBlock{
					CIDR:   "0.0.0.0/0",
					Except: blockedCIDRs,
				},
			},
		},
	})

	// 3. Append explicit AllowedCIDRs if specified in ws.Spec.NetworkPolicy (e.g. internal LLM proxy)
	if ws.Spec.NetworkPolicy != nil {
		for _, cidr := range ws.Spec.NetworkPolicy.AllowedCIDRs {
			if trimmed := strings.TrimSpace(cidr); trimmed != "" {
				egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
					To: []networkingv1.NetworkPolicyPeer{
						{
							IPBlock: &networkingv1.IPBlock{
								CIDR: trimmed,
							},
						},
					},
				})
			}
		}
	}

	// 4. Append namespace-scoped egress exceptions (e.g. the shared OTel collector in the
	// "monitoring" namespace), so workspaces can reach in-cluster services inside the
	// private ranges excluded above. Controller-wide defaults apply to every Workspace;
	// per-workspace rules from ws.Spec.NetworkPolicy are merged on top (deduplicated).
	// Rules are matched by namespace/pod labels and stay valid when target pods move.
	seenNsRules := make(map[string]struct{}, len(r.GlobalAllowedNamespaces))
	for _, nsRule := range mergeNamespaceEgressRules(r.GlobalAllowedNamespaces, ws.Spec.NetworkPolicy) {
		key := netpolNamespaceRuleKey(nsRule)
		if _, dup := seenNsRules[key]; dup {
			continue
		}
		seenNsRules[key] = struct{}{}
		if egressRule, ok := buildNamespaceEgressRule(nsRule); ok {
			egressRules = append(egressRules, egressRule)
		}
	}

	desiredNetpolSpec := networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{
			MatchLabels: selectorLabels,
		},
		PolicyTypes: []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress,
			networkingv1.PolicyTypeEgress,
		},
		// Allow the application port from any source so this works with Ingress
		// implementations that SNAT or use host networking. Other ports and
		// non-TCP protocols such as ICMP remain blocked by ingress isolation.
		Ingress: []networkingv1.NetworkPolicyIngressRule{
			{
				Ports: []networkingv1.NetworkPolicyPort{
					{
						Protocol: &tcpProtocol,
						Port:     &containerPort,
					},
				},
			},
		},
		Egress: egressRules,
	}

	if err != nil {
		if apierrors.IsNotFound(err) {
			netpol = &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      netpolName,
					Namespace: ws.Namespace,
					Labels:    labels,
				},
				Spec: desiredNetpolSpec,
			}
			if err := ctrl.SetControllerReference(ws, netpol, r.Scheme); err != nil {
				return err
			}
			log.Info("Creating NetworkPolicy for workspace", "netpolName", netpolName, "blockedCIDRs", blockedCIDRs)
			return r.Create(ctx, netpol)
		}
		return err
	}

	needsUpdate := false
	if !maps.Equal(netpol.Labels, labels) {
		netpol.Labels = labels
		needsUpdate = true
	}
	if !equality.Semantic.DeepEqual(netpol.Spec, desiredNetpolSpec) {
		netpol.Spec = desiredNetpolSpec
		needsUpdate = true
	}
	if needsUpdate {
		log.Info("Updating NetworkPolicy spec due to changes", "netpol", netpolName)
		return r.Update(ctx, netpol)
	}
	return nil
}

// mergeNamespaceEgressRules combines controller-wide egress exceptions with the
// per-workspace rules declared in ws.Spec.NetworkPolicy (controller defaults first).
func mergeNamespaceEgressRules(
	global []aiv1alpha1.WorkspaceNetworkPolicyNamespaceRule,
	wsPolicy *aiv1alpha1.WorkspaceNetworkPolicySpec,
) []aiv1alpha1.WorkspaceNetworkPolicyNamespaceRule {
	var wsRules []aiv1alpha1.WorkspaceNetworkPolicyNamespaceRule
	if wsPolicy != nil {
		wsRules = wsPolicy.AllowedNamespaces
	}
	if len(global) == 0 {
		return wsRules
	}
	if len(wsRules) == 0 {
		return global
	}
	rules := make([]aiv1alpha1.WorkspaceNetworkPolicyNamespaceRule, 0, len(global)+len(wsRules))
	rules = append(rules, global...)
	rules = append(rules, wsRules...)
	return rules
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

	maxConcurrent := r.MaxConcurrentReconciles
	if maxConcurrent <= 0 {
		maxConcurrent = 30
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&aiv1alpha1.Workspace{}, builder.WithPredicates(workspacePredicate())).
		Owns(&appsv1.Deployment{}, builder.WithPredicates(deploymentPredicate())).
		Owns(&corev1.Service{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&networkingv1.Ingress{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&networkingv1.NetworkPolicy{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Named("workspace").
		Complete(r)
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

// buildWorkspaceLabels returns the labels for child resources (Deployment, Pods, Service, Ingress, NetworkPolicy).
// User-provided labels from the Workspace CR are copied first, and system/built-in labels ("app", "workspace", "oa")
// are applied afterwards to ensure selector consistency and prevent user labels from breaking Deployment/Service selectors.
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
