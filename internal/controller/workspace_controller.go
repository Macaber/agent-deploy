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
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiresources "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

// workspaceFinalizer 保证 workspace 删除时有机会清理同名 PV
const workspaceFinalizer = "ai.example.com/workspace-cleanup"

// WorkspaceReconciler reconciles a Workspace object
type WorkspaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=ai.example.com,resources=workspaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ai.example.com,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ai.example.com,resources=workspaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

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

	// 删除处理：清理 PV 与存储介质数据后移除 finalizer（PVC 由 ownerRef 自动级联删除）。
	// 语义与 NAS/Local 方案一致：删除 workspace 时 PV、PVC、存储介质数据一并删除。
	// Local/NFS 的数据由各自 provisioner 在 PVC Delete 回收时清理；
	// OSS 卷（Retain + CSI DeleteVolume 为空操作）由 cleanupPV 显式清空 OSS 数据。
	if !ws.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(ws, workspaceFinalizer) {
			if err := r.cleanupPV(ctx, ws); err != nil {
				log.Error(err, "Failed to cleanup PV for deleted workspace")
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(ws, workspaceFinalizer)
			if err := r.Update(ctx, ws); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// 首次创建时添加 finalizer，保证后续删除时有机会清理 PV
	if !controllerutil.ContainsFinalizer(ws, workspaceFinalizer) {
		controllerutil.AddFinalizer(ws, workspaceFinalizer)
		if err := r.Update(ctx, ws); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
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
		if err := r.Status().Update(ctx, ws); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch to get latest resourceVersion
		if err := r.Get(ctx, req.NamespacedName, ws); err != nil {
			return ctrl.Result{}, err
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

				// Find pod name to update status
				podList := &corev1.PodList{}
				opts := []client.ListOption{
					client.InNamespace(ws.Namespace),
					client.MatchingLabels(deploy.Spec.Selector.MatchLabels),
				}
				if err := r.List(ctx, podList, opts...); err == nil && len(podList.Items) > 0 {
					ws.Status.PodName = podList.Items[0].Name
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
		if err := r.Status().Update(ctx, ws); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: nextRequeue}, nil
}

func (r *WorkspaceReconciler) reconcilePVC(ctx context.Context, ws *aiv1alpha1.Workspace) (string, error) {
	log := logf.FromContext(ctx)
	pvcName := ws.Name + "-pvc"
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: pvcName}, pvc)
	if err != nil {
		if apierrors.IsNotFound(err) {
			storageSize, err := apiresources.ParseQuantity(ws.Spec.Storage.Size)
			if err != nil {
				return "", err
			}
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
									VolumeAttributes: map[string]string{
										"bucket":    bucket,
										"url":       url,
										"path":      subPath,
										"otherOpts": otherOpts,
										"fuseType":  fuseType,
									},
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

			if err := ctrl.SetControllerReference(ws, pvc, r.Scheme); err != nil {
				return "", err
			}
			if err := r.Create(ctx, pvc); err != nil {
				return "", err
			}
			return pvcName, nil
		}
		return "", err
	}
	return pvc.Name, nil
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

	// Parse resource requirements
	resources := corev1.ResourceRequirements{}

	// Default resource specs if empty (0.5c CPU / 1G Memory)
	cpuStr := ws.Spec.Runtime.CPU
	if cpuStr == "" {
		cpuStr = "500m" // 0.5 CPU
	}
	memStr := ws.Spec.Runtime.Memory
	if memStr == "" {
		memStr = "1Gi" // 1G Memory
	}

	resources.Limits = corev1.ResourceList{}
	resources.Requests = corev1.ResourceList{}

	if qtyCPU, err := apiresources.ParseQuantity(cpuStr); err == nil {
		resources.Limits[corev1.ResourceCPU] = qtyCPU
		resources.Requests[corev1.ResourceCPU] = qtyCPU
	}
	if qtyMem, err := apiresources.ParseQuantity(memStr); err == nil {
		resources.Limits[corev1.ResourceMemory] = qtyMem
		resources.Requests[corev1.ResourceMemory] = qtyMem
	}

	// Ports
	containerPort := int32(8080)
	if ws.Spec.Runtime.Port != 0 {
		containerPort = ws.Spec.Runtime.Port
	} else if strings.Contains(ws.Spec.Runtime.Image, "nginx") {
		containerPort = 80
	}

	ports := []corev1.ContainerPort{
		{Name: "http", ContainerPort: containerPort},
	}
	if ws.Spec.ExposeSSH {
		ports = append(ports, corev1.ContainerPort{Name: "ssh", ContainerPort: 22})
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

	labels := map[string]string{
		"app":       "workspace",
		"workspace": ws.Name,
	}

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
						MatchLabels: labels,
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

	// Update replica count and other spec settings
	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		return "", 0, fmt.Errorf("deployment %s has no containers", deployName)
	}
	deploy.Spec.Replicas = &desiredReplicas
	deploy.Spec.Strategy = appsv1.DeploymentStrategy{
		Type: appsv1.RecreateDeploymentStrategyType,
	}
	deploy.Spec.Template.Spec.RuntimeClassName = ws.Spec.Runtime.RuntimeClassName
	deploy.Spec.Template.Spec.Containers[0].Image = ws.Spec.Runtime.Image
	deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
	deploy.Spec.Template.Spec.Containers[0].Command = command
	deploy.Spec.Template.Spec.Containers[0].Args = containerArgs
	deploy.Spec.Template.Spec.Containers[0].Env = envVars
	deploy.Spec.Template.Spec.Containers[0].Resources = resources
	deploy.Spec.Template.Spec.Containers[0].Ports = ports
	deploy.Spec.Template.Spec.InitContainers = initContainers
	deploy.Spec.Template.Spec.Containers[0].VolumeMounts = volumeMounts
	deploy.Spec.Template.Spec.Containers[0].Lifecycle = lifecycle
	deploy.Spec.Template.Spec.Containers[0].StartupProbe = getStartupProbe(ws)
	deploy.Spec.Template.Spec.Containers[0].ReadinessProbe = getReadinessProbe(ws)
	deploy.Spec.Template.Spec.Volumes = volumes
	deploy.Spec.Template.Spec.TerminationGracePeriodSeconds = int64Ptr(2)
	deploy.Spec.Template.Spec.AutomountServiceAccountToken = boolPtr(false)
	deploy.Spec.Template.Spec.EnableServiceLinks = boolPtr(false)

	if err := r.Update(ctx, deploy); err != nil {
		return "", 0, err
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

	ports := []corev1.ServicePort{
		{
			Name:       "http",
			Port:       80,
			TargetPort: intstr.FromString("http"),
		},
	}
	if ws.Spec.ExposeSSH {
		ports = append(ports, corev1.ServicePort{
			Name:       "ssh",
			Port:       22,
			TargetPort: intstr.FromString("ssh"),
		})
	}

	labels := map[string]string{
		"app":       "workspace",
		"workspace": ws.Name,
	}

	if err != nil {
		if apierrors.IsNotFound(err) {
			svc = &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      svcName,
					Namespace: ws.Namespace,
					Labels:    labels,
				},
				Spec: corev1.ServiceSpec{
					Selector: labels,
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

	svc.Spec.Ports = ports
	if err := r.Update(ctx, svc); err != nil {
		return "", err
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

	labels := map[string]string{
		"app":       "workspace",
		"workspace": ws.Name,
	}

	if err != nil {
		if apierrors.IsNotFound(err) {
			ingress = &networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ingressName,
					Namespace: ws.Namespace,
					Labels:    labels,
					Annotations: map[string]string{
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

	if ingress.Annotations == nil {
		ingress.Annotations = make(map[string]string)
	}
	ingress.Annotations["nginx.ingress.kubernetes.io/proxy-connect-timeout"] = "10"
	ingress.Annotations["nginx.ingress.kubernetes.io/proxy-body-size"] = "100m"
	ingress.Annotations["nginx.ingress.kubernetes.io/proxy-read-timeout"] = "86400"
	ingress.Annotations["nginx.ingress.kubernetes.io/proxy-send-timeout"] = "86400"
	ingress.Annotations["nginx.ingress.kubernetes.io/proxy-buffering"] = "off"
	ingress.Annotations["nginx.ingress.kubernetes.io/proxy-http-version"] = "1.1"
	ingress.Annotations["nginx.ingress.kubernetes.io/limit-connections"] = "200"
	ingress.Annotations["nginx.ingress.kubernetes.io/proxy-next-upstream"] = "error timeout invalid_header http_502 http_503"
	ingress.Annotations["nginx.ingress.kubernetes.io/proxy-next-upstream-tries"] = "3"
	ingress.Spec = ingressSpec
	if err := r.Update(ctx, ingress); err != nil {
		return "", err
	}
	return host, nil
}

func isNetworkPolicyDisabled(ws *aiv1alpha1.Workspace) bool {
	if ws.Spec.DisableNetworkPolicy {
		return true
	}
	if ws.Spec.NetworkPolicy != nil && ws.Spec.NetworkPolicy.Disabled {
		return true
	}
	return false
}

func getBlockedEgressCIDRs(ws *aiv1alpha1.Workspace) []string {
	if ws.Spec.NetworkPolicy != nil && len(ws.Spec.NetworkPolicy.BlockedCIDRs) > 0 {
		return ws.Spec.NetworkPolicy.BlockedCIDRs
	}

	envCIDRs := os.Getenv("DEFAULT_BLOCKED_EGRESS_CIDRS")
	if envCIDRs == "" {
		envCIDRs = os.Getenv("BLOCKED_EGRESS_CIDRS")
	}
	if envCIDRs != "" {
		parts := strings.Split(envCIDRs, ",")
		var list []string
		for _, p := range parts {
			trimmed := strings.TrimSpace(p)
			if trimmed != "" {
				list = append(list, trimmed)
			}
		}
		if len(list) > 0 {
			return list
		}
	}

	// Default fallback: RFC1918 private subnets and cloud metadata endpoint
	return []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.169.254/32",
	}
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

	labels := map[string]string{
		"app":       "workspace",
		"workspace": ws.Name,
	}

	containerPort := int32(8080)
	if ws.Spec.Runtime.Port != 0 {
		containerPort = ws.Spec.Runtime.Port
	} else if strings.Contains(ws.Spec.Runtime.Image, "nginx") {
		containerPort = 80
	}

	tcpProtocol := corev1.ProtocolTCP
	udpProtocol := corev1.ProtocolUDP

	ingressPorts := []networkingv1.NetworkPolicyPort{
		{
			Protocol: &tcpProtocol,
			Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: containerPort},
		},
	}
	if ws.Spec.ExposeSSH {
		ingressPorts = append(ingressPorts, networkingv1.NetworkPolicyPort{
			Protocol: &tcpProtocol,
			Port:     &intstr.IntOrString{Type: intstr.Int, IntVal: 22},
		})
	}

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
		// 2. Allow egress to external Internet (0.0.0.0/0), while blocking the configured/custom CIDRs in Except
		{
			To: []networkingv1.NetworkPolicyPeer{
				{
					IPBlock: &networkingv1.IPBlock{
						CIDR:   "0.0.0.0/0",
						Except: blockedCIDRs,
					},
				},
			},
		},
	}

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

	desiredNetpolSpec := networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{
			MatchLabels: labels,
		},
		PolicyTypes: []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress,
			networkingv1.PolicyTypeEgress,
		},
		Ingress: []networkingv1.NetworkPolicyIngressRule{
			{
				Ports: ingressPorts,
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

	netpol.Spec = desiredNetpolSpec
	return r.Update(ctx, netpol)
}

func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&aiv1alpha1.Workspace{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&networkingv1.NetworkPolicy{}).
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
		PeriodSeconds:       2,
		SuccessThreshold:    1,
		FailureThreshold:    30,
	}
}

func getReadinessProbe(ws *aiv1alpha1.Workspace) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        getProbeHandler(ws),
		InitialDelaySeconds: 1,
		PeriodSeconds:       2,
		SuccessThreshold:    1,
		FailureThreshold:    3,
	}
}
