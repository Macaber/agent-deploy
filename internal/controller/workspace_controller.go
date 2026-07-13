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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	apiresources "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

// WorkspaceReconciler reconciles a Workspace object
type WorkspaceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=ai.example.com,resources=workspaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ai.example.com,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ai.example.com,resources=workspaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
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

	// Reconcile KEDA ScaledObject dynamically
	if err := r.reconcileScaledObject(ctx, ws, deployName); err != nil {
		if meta.IsNoMatchError(err) {
			log.Info("KEDA ScaledObject CRD not found in cluster, skipping KEDA integration")
		} else {
			log.Error(err, "Failed to reconcile KEDA ScaledObject")
		}
	}

	// 3. Status Phase logic & Idle Timeout logic
	var nextRequeue time.Duration

	// Compute next Requeue based on TTL if set
	if ws.Status.ExpiryTime != nil {
		remainingTTL := time.Until(ws.Status.ExpiryTime.Time)
		if remainingTTL > 0 {
			nextRequeue = remainingTTL
		}
	}

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
					if nextRequeue == 0 || remainingIdle < nextRequeue {
						nextRequeue = remainingIdle
					}
				} else {
					// Idle expired, trigger immediate reconcile to scale down
					nextRequeue = 1 * time.Second
				}
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
	deployName := ws.Name + "-deploy"
	deploy := &appsv1.Deployment{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: deployName}, deploy)

	// Determine desired replicas
	desiredReplicas := int32(1)
	if ws.Spec.Stopped {
		desiredReplicas = 0
	} else {
		// If not stopped, check if it's sleeping due to idle timeout
		if ws.Spec.IdleTimeout != "" && ws.Status.LastActiveTime != nil {
			idleDuration, err := time.ParseDuration(ws.Spec.IdleTimeout)
			if err == nil {
				idleExpiry := ws.Status.LastActiveTime.Add(idleDuration)
				if time.Now().After(idleExpiry) {
					desiredReplicas = 0
				}
			}
		}
	}

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
	for i, svm := range ws.Spec.SharedVolumeMounts {
		volumeName := fmt.Sprintf("shared-vol-%d", i)
		volumes = append(volumes, corev1.Volume{
			Name: volumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: svm.PVCName,
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      volumeName,
			MountPath: svm.MountPath,
			SubPath:   svm.SubPath,
		})
	}

	if len(ws.Spec.Runtime.VolumeMounts) > 0 {
		for _, vm := range ws.Spec.Runtime.VolumeMounts {
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      "workspace-data",
				MountPath: vm.MountPath,
				SubPath:   vm.SubPath,
			})
		}
	} else {
		volumeMounts = []corev1.VolumeMount{
			{
				Name:      "workspace-data",
				MountPath: "/workspace",
				SubPath:   "workspace",
			},
		}

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

	// Temporarily disabled git clone initContainers
	var initContainers []corev1.Container

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
					Selector: &metav1.LabelSelector{
						MatchLabels: labels,
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: labels,
						},
						Spec: corev1.PodSpec{
							InitContainers:                initContainers,
							TerminationGracePeriodSeconds: int64Ptr(2),
							AutomountServiceAccountToken:  boolPtr(false),
							Containers: []corev1.Container{
								{
									Name:            "runtime",
									Image:           ws.Spec.Runtime.Image,
									ImagePullPolicy: corev1.PullIfNotPresent,
									WorkingDir:      "/workspace",
									Command:         command,
									Args:            containerArgs,
									Ports:           ports,
									Env:             envVars,
									Resources:       resources,
									VolumeMounts:    volumeMounts,
									Lifecycle:       lifecycle,
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

	// Update logic: we update replica count and other spec settings
	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		return "", 0, fmt.Errorf("deployment %s has no containers", deployName)
	}
	deploy.Spec.Replicas = &desiredReplicas
	deploy.Spec.Template.Spec.Containers[0].Image = ws.Spec.Runtime.Image
	deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
	deploy.Spec.Template.Spec.Containers[0].WorkingDir = "/workspace"
	deploy.Spec.Template.Spec.Containers[0].Command = command
	deploy.Spec.Template.Spec.Containers[0].Args = containerArgs
	deploy.Spec.Template.Spec.Containers[0].Env = envVars
	deploy.Spec.Template.Spec.Containers[0].Resources = resources
	deploy.Spec.Template.Spec.Containers[0].Ports = ports
	deploy.Spec.Template.Spec.InitContainers = initContainers
	deploy.Spec.Template.Spec.Containers[0].VolumeMounts = volumeMounts
	deploy.Spec.Template.Spec.Containers[0].Lifecycle = lifecycle
	deploy.Spec.Template.Spec.Containers[0].ReadinessProbe = getReadinessProbe(ws)
	deploy.Spec.Template.Spec.Volumes = volumes
	deploy.Spec.Template.Spec.TerminationGracePeriodSeconds = int64Ptr(2)
	deploy.Spec.Template.Spec.AutomountServiceAccountToken = boolPtr(false)

	if err := r.Update(ctx, deploy); err != nil {
		return "", 0, err
	}

	return deploy.Name, desiredReplicas, nil
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

	ingress.Spec = ingressSpec
	if err := r.Update(ctx, ingress); err != nil {
		return "", err
	}
	return host, nil
}

func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&aiv1alpha1.Workspace{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Named("workspace").
		Complete(r)
}

func (r *WorkspaceReconciler) reconcileScaledObject(ctx context.Context, ws *aiv1alpha1.Workspace, deployName string) error {
	scaledObjectName := ws.Name + "-scaledobject"

	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "keda.sh",
		Version: "v1alpha1",
		Kind:    "ScaledObject",
	})

	err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: scaledObjectName}, so)

	host := getWorkspaceHost(ws.Name)

	// Construct the desired ScaledObject structure
	desiredSO := &unstructured.Unstructured{}
	desiredSO.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "keda.sh",
		Version: "v1alpha1",
		Kind:    "ScaledObject",
	})
	desiredSO.SetName(scaledObjectName)
	desiredSO.SetNamespace(ws.Namespace)

	desiredSpec := map[string]any{
		"scaleTargetRef": map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"name":       deployName,
		},
		"minReplicaCount": int64(0),
		"maxReplicaCount": int64(1),
		"cooldownPeriod":  int64(300), // 5 minutes
		"triggers": []any{
			map[string]any{
				"type": "http",
				"metadata": map[string]any{
					"serverAddress": "http://keda-http-add-on-interceptor.keda:8080",
					"host":          host,
				},
			},
		},
	}

	desiredSO.Object["spec"] = desiredSpec

	if err := ctrl.SetControllerReference(ws, desiredSO, r.Scheme); err != nil {
		return err
	}

	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.Create(ctx, desiredSO)
		}
		return err
	}

	so.Object["spec"] = desiredSpec
	return r.Update(ctx, so)
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

func getReadinessProbe(ws *aiv1alpha1.Workspace) *corev1.Probe {
	var handler corev1.ProbeHandler
	if ws.Spec.Runtime.HealthPath != "" {
		handler = corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: ws.Spec.Runtime.HealthPath,
				Port: intstr.FromString("http"),
			},
		}
	} else {
		handler = corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromString("http"),
			},
		}
	}

	return &corev1.Probe{
		ProbeHandler:        handler,
		InitialDelaySeconds: 0,
		PeriodSeconds:       1,
		SuccessThreshold:    1,
		FailureThreshold:    30,
	}
}
