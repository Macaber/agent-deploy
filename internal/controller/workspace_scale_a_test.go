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
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
	"github.com/example/workspace-operator/internal/egress"
)

type recordingClient struct {
	client.Client
	mu             sync.Mutex
	gets           map[string]int
	lists          map[string]int
	listLimits     []int64
	creates        int
	updates        int
	patches        int
	deletes        int
	statusUpdates  int
	statusPatches  int
	stripDeployGet bool
}

func newRecordingClient(c client.Client) *recordingClient {
	return &recordingClient{
		Client: c,
		gets:   make(map[string]int),
		lists:  make(map[string]int),
	}
}

func kindKey(obj any) string {
	switch obj.(type) {
	case *appsv1.Deployment, *appsv1.DeploymentList:
		return "Deployment"
	case *corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaimList:
		return "PVC"
	case *corev1.PersistentVolume, *corev1.PersistentVolumeList:
		return "PV"
	case *corev1.Service, *corev1.ServiceList:
		return "Service"
	case *networkingv1.Ingress, *networkingv1.IngressList:
		return "Ingress"
	case *networkingv1.NetworkPolicy, *networkingv1.NetworkPolicyList:
		return "NetworkPolicy"
	case *corev1.ConfigMap, *corev1.ConfigMapList:
		return "ConfigMap"
	case *corev1.Pod, *corev1.PodList:
		return "Pod"
	case *aiv1alpha1.Workspace, *aiv1alpha1.WorkspaceList:
		return "Workspace"
	default:
		return "Other"
	}
}

func (r *recordingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.mu.Lock()
	r.gets[kindKey(obj)]++
	stripDeploy := r.stripDeployGet
	r.mu.Unlock()

	err := r.Client.Get(ctx, key, obj, opts...)
	if err == nil && stripDeploy {
		if d, ok := obj.(*appsv1.Deployment); ok {
			if specHash := d.Annotations[DeploymentSpecHashAnnotation]; specHash != "" {
				d.Annotations = map[string]string{DeploymentSpecHashAnnotation: specHash}
			} else {
				d.Annotations = nil
			}
			d.Spec.Template.Spec = corev1.PodSpec{SecurityContext: d.Spec.Template.Spec.SecurityContext}
		}
	}
	return err
}

func (r *recordingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	var listOpts client.ListOptions
	for _, opt := range opts {
		opt.ApplyToList(&listOpts)
	}
	r.mu.Lock()
	switch list.(type) {
	case *aiv1alpha1.WorkspaceList:
		r.lists["Workspace"]++
	case *corev1.PodList:
		r.lists["Pod"]++
	case *appsv1.DeploymentList:
		r.lists["Deployment"]++
	case *networkingv1.NetworkPolicyList:
		r.lists["NetworkPolicy"]++
	default:
		r.lists["Other"]++
	}
	if listOpts.Limit > 0 {
		r.listLimits = append(r.listLimits, listOpts.Limit)
	}
	r.mu.Unlock()
	return r.Client.List(ctx, list, opts...)
}

func (r *recordingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	r.mu.Lock()
	r.creates++
	r.mu.Unlock()
	return r.Client.Create(ctx, obj, opts...)
}

func (r *recordingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	r.mu.Lock()
	r.updates++
	r.mu.Unlock()
	return r.Client.Update(ctx, obj, opts...)
}

func (r *recordingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	r.mu.Lock()
	r.patches++
	r.mu.Unlock()
	return r.Client.Patch(ctx, obj, patch, opts...)
}

func (r *recordingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	r.mu.Lock()
	r.deletes++
	r.mu.Unlock()
	return r.Client.Delete(ctx, obj, opts...)
}

func (r *recordingClient) Status() client.SubResourceWriter {
	return &recordingStatusWriter{parent: r, SubResourceWriter: r.Client.Status()}
}

func (r *recordingClient) resetCounts() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gets = make(map[string]int)
	r.lists = make(map[string]int)
	r.listLimits = nil
	r.creates = 0
	r.updates = 0
	r.patches = 0
	r.deletes = 0
	r.statusUpdates = 0
	r.statusPatches = 0
}

func (r *recordingClient) totalWrites() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.creates + r.updates + r.patches + r.deletes + r.statusUpdates + r.statusPatches
}

func (r *recordingClient) totalReads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, n := range r.gets {
		total += n
	}
	for _, n := range r.lists {
		total += n
	}
	return total
}

type recordingStatusWriter struct {
	parent *recordingClient
	client.SubResourceWriter
}

func (w *recordingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.parent.mu.Lock()
	w.parent.statusUpdates++
	w.parent.mu.Unlock()
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (w *recordingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	w.parent.mu.Lock()
	w.parent.statusPatches++
	w.parent.mu.Unlock()
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func newScaleTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		aiv1alpha1.AddToScheme,
		appsv1.AddToScheme,
		corev1.AddToScheme,
		networkingv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add scheme: %v", err)
		}
	}
	return scheme
}

func TestDeploymentSpecHashDeterministicAndSensitive(t *testing.T) {
	t.Setenv("USE_HOSTPATH", "true")
	t.Setenv("WORKSPACE_DATA_DIR", t.TempDir())
	scheme := newScaleTestScheme(t)

	wsBase := &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-hash", Namespace: "work"},
		Spec: aiv1alpha1.WorkspaceSpec{
			Owner: "alice",
			Runtime: aiv1alpha1.RuntimeSpec{
				Image:      "nginx:alpine",
				HealthPath: "/healthz",
				Env:        []aiv1alpha1.EnvVar{{Name: "FOO", Value: "bar"}},
			},
			Storage: aiv1alpha1.StorageSpec{Size: "10Gi"},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-hash-pvc", Namespace: "work"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "ws-hash-pv"},
	}
	pv := ossPV("ws-hash-pv", "-o uid=10001,gid=10001,umask=022")
	r := &WorkspaceReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, pv).Build(),
		Scheme: scheme,
	}

	ctx := context.Background()
	parts1, err := r.buildDesiredDeploymentSpec(ctx, wsBase, pvc.Name, pvc, false, egress.Config{})
	if err != nil {
		t.Fatalf("buildDesiredDeploymentSpec 1 failed: %v", err)
	}
	parts2, err := r.buildDesiredDeploymentSpec(ctx, wsBase.DeepCopy(), pvc.Name, pvc, false, egress.Config{})
	if err != nil {
		t.Fatalf("buildDesiredDeploymentSpec 2 failed: %v", err)
	}
	if parts1.specHash == "" || parts1.specHash != parts2.specHash {
		t.Fatalf("expected deterministic non-empty specHash, got %q vs %q", parts1.specHash, parts2.specHash)
	}

	// Changing Image, Env, Resources, VolumeMounts, HealthPath, DefaultRunAsUser, or Egress mode must change specHash.
	variants := []struct {
		name   string
		mutate func(ws *aiv1alpha1.Workspace, rec *WorkspaceReconciler, migrate *bool, cfg *egress.Config)
	}{
		{
			name: "image",
			mutate: func(ws *aiv1alpha1.Workspace, _ *WorkspaceReconciler, _ *bool, _ *egress.Config) {
				ws.Spec.Runtime.Image = "nginx:1.27"
			},
		},
		{
			name: "env",
			mutate: func(ws *aiv1alpha1.Workspace, _ *WorkspaceReconciler, _ *bool, _ *egress.Config) {
				ws.Spec.Runtime.Env[0].Value = "baz"
			},
		},
		{
			name: "resources",
			mutate: func(ws *aiv1alpha1.Workspace, _ *WorkspaceReconciler, _ *bool, _ *egress.Config) {
				ws.Spec.Runtime.CPU = "2"
			},
		},
		{
			name: "volumeMounts",
			mutate: func(ws *aiv1alpha1.Workspace, _ *WorkspaceReconciler, _ *bool, _ *egress.Config) {
				ws.Spec.Runtime.VolumeMounts = []aiv1alpha1.VolumeMount{{MountPath: "/data", SubPath: "data"}}
			},
		},
		{
			name: "probe",
			mutate: func(ws *aiv1alpha1.Workspace, _ *WorkspaceReconciler, _ *bool, _ *egress.Config) {
				ws.Spec.Runtime.HealthPath = "/readyz"
			},
		},
		{
			name: "defaultRunAsUser",
			mutate: func(_ *aiv1alpha1.Workspace, rec *WorkspaceReconciler, _ *bool, _ *egress.Config) {
				rec.DefaultRunAsUser = 10001
			},
		},
		{
			name: "egressAllowlist",
			mutate: func(_ *aiv1alpha1.Workspace, _ *WorkspaceReconciler, migrate *bool, cfg *egress.Config) {
				*migrate = true
				*cfg = egress.Config{Mode: egress.ModeAllowlist}
			},
		},
	}

	for _, tc := range variants {
		t.Run(tc.name, func(t *testing.T) {
			wsCopy := wsBase.DeepCopy()
			recCopy := &WorkspaceReconciler{Client: r.Client, Scheme: scheme}
			migrate := false
			cfg := egress.Config{}
			tc.mutate(wsCopy, recCopy, &migrate, &cfg)
			got, err := recCopy.buildDesiredDeploymentSpec(ctx, wsCopy, pvc.Name, pvc, migrate, cfg)
			if err != nil {
				t.Fatalf("buildDesiredDeploymentSpec failed: %v", err)
			}
			if got.specHash == parts1.specHash {
				t.Fatalf("expected specHash to change when %s changed", tc.name)
			}
		})
	}
}

func readyProxyObjects(cfg egress.Config) []client.Object {
	one := int32(1)
	rev := cfg.Revision()
	statusJSON, _ := json.Marshal(egress.ReplicaStatus{
		Pod:             "workspace-egress-0",
		PodUID:          "proxy-uid-0",
		DesiredRevision: rev,
		LoadedRevision:  rev,
		State:           "loaded",
		ObservedAt:      time.Now().UTC().Format(time.RFC3339),
	})
	return []client.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "workspace-egress",
				Namespace:  egress.Namespace(),
				Generation: 1,
			},
			Spec: appsv1.DeploymentSpec{Replicas: &one},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: 1,
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "workspace-egress-0",
				Namespace: egress.Namespace(),
				UID:       "proxy-uid-0",
				Labels:    map[string]string{"app.kubernetes.io/name": "workspace-egress"},
				Annotations: map[string]string{
					egress.StatusAnnotation: string(statusJSON),
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				},
			},
		},
	}
}

func TestReconcileDeploymentSpecHashZeroUncachedGet(t *testing.T) {
	t.Setenv("USE_HOSTPATH", "true")
	t.Setenv("WORKSPACE_DATA_DIR", t.TempDir())
	t.Setenv("EGRESS_CONFIG_NAMESPACE", "system")
	t.Setenv("EGRESS_PROTECTED_CIDRS", "10.0.0.0/8")

	modes := []struct {
		name          string
		egressEnabled string
		cfg           egress.Config
	}{
		{name: "egress-disabled", egressEnabled: "false"},
		{name: "egress-legacy", egressEnabled: "true", cfg: egress.Config{Mode: egress.ModeLegacy}},
		{name: "egress-unrestricted", egressEnabled: "true", cfg: egress.Config{Mode: egress.ModeUnrestricted}},
		{
			name:          "egress-allowlist",
			egressEnabled: "true",
			cfg: egress.Config{
				Mode: egress.ModeAllowlist,
				Services: []egress.Service{
					{Name: "dns", Namespace: "kube-system", PodSelector: map[string]string{"k8s-app": "kube-dns"}, Protocol: "TCP", Port: 53},
					{Name: "dns", Namespace: "kube-system", PodSelector: map[string]string{"k8s-app": "kube-dns"}, Protocol: "UDP", Port: 53},
				},
			},
		},
	}

	for _, tc := range modes {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("EGRESS_ENABLED", tc.egressEnabled)
			scheme := newScaleTestScheme(t)
			objs := []client.Object{}
			if tc.egressEnabled == "true" {
				objs = append(objs, readyProxyObjects(tc.cfg)...)
			}
			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			if tc.egressEnabled == "true" {
				if err := egress.Save(context.Background(), base, "system", "", tc.cfg); err != nil {
					t.Fatalf("save egress config: %v", err)
				}
			}

			cacheReader := newRecordingClient(base)
			cacheReader.stripDeployGet = true
			apiReader := newRecordingClient(base)
			r := &WorkspaceReconciler{
				Client:    cacheReader,
				APIReader: apiReader,
				Scheme:    scheme,
			}
			ws := &aiv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{Name: "ws-steady", Namespace: "work"},
				Spec: aiv1alpha1.WorkspaceSpec{
					Owner:   "alice",
					Runtime: aiv1alpha1.RuntimeSpec{Image: "nginx:alpine"},
					Storage: aiv1alpha1.StorageSpec{Size: "1Gi"},
				},
			}

			ctx := context.Background()
			// First reconcile converges Deployment + policies (slow path).
			if _, replicas, err := r.reconcileDeployment(ctx, ws, "ws-steady-pvc", nil); err != nil || replicas != 1 {
				t.Fatalf("initial reconcileDeployment failed: replicas=%d err=%v", replicas, err)
			}

			created := &appsv1.Deployment{}
			if err := base.Get(ctx, client.ObjectKey{Namespace: "work", Name: "ws-steady-deploy"}, created); err != nil {
				t.Fatalf("get created deployment: %v", err)
			}
			if created.Annotations[DeploymentSpecHashAnnotation] == "" {
				t.Fatal("expected DeploymentSpecHashAnnotation to be populated")
			}

			// Steady-state reconcile: stripped cache matches spec-hash => 0 uncached reads, 0 writes.
			cacheReader.resetCounts()
			apiReader.resetCounts()
			if _, replicas, err := r.reconcileDeployment(ctx, ws, "ws-steady-pvc", nil); err != nil || replicas != 1 {
				t.Fatalf("steady-state reconcileDeployment failed: replicas=%d err=%v", replicas, err)
			}
			if got := apiReader.totalReads(); got != 0 {
				t.Fatalf("expected 0 uncached APIReader reads in steady state, got %d (gets=%v lists=%v)", got, apiReader.gets, apiReader.lists)
			}
			if got := cacheReader.totalWrites(); got != 0 {
				t.Fatalf("expected 0 writes in steady state, got %d", got)
			}

			// Under allowlist mode, adding an IP rule keeps spec-hash steady (0 uncached Deployment Get)
			// while still converging workspace-egress-v1 NetworkPolicy.
			if tc.name == "egress-allowlist" {
				updatedCfg := tc.cfg
				updatedCfg.IPs = []egress.IP{{CIDR: "1.2.3.4/32", Protocol: "TCP", Port: 443}}
				if err := egress.Save(ctx, base, "system", "", updatedCfg); err == nil {
					t.Fatal("expected Save without version to fail AlreadyExists")
				}
				cm := &corev1.ConfigMap{}
				if err := base.Get(ctx, client.ObjectKey{Namespace: "system", Name: egress.ConfigMapName}, cm); err != nil {
					t.Fatalf("get configmap: %v", err)
				}
				if err := egress.Save(ctx, base, "system", cm.ResourceVersion, updatedCfg); err != nil {
					t.Fatalf("update egress config: %v", err)
				}
				cacheReader.resetCounts()
				apiReader.resetCounts()
				if _, _, err := r.reconcileDeployment(ctx, ws, "ws-steady-pvc", nil); err != nil {
					t.Fatalf("same-mode rule update failed: %v", err)
				}
				if got := apiReader.gets["Deployment"]; got != 0 {
					t.Fatalf("expected 0 uncached Deployment Gets on allowlist IP update, got %d", got)
				}
				np := &networkingv1.NetworkPolicy{}
				if err := base.Get(ctx, client.ObjectKey{Namespace: "work", Name: "workspace-egress-v1"}, np); err != nil {
					t.Fatalf("get workspace-egress-v1: %v", err)
				}
				if np.Annotations["workspace.bocom.io/egress-revision"] != updatedCfg.Revision() {
					t.Fatalf("expected workspace-egress-v1 revision %s, got %s", updatedCfg.Revision(), np.Annotations["workspace.bocom.io/egress-revision"])
				}
			}
		})
	}
}

func TestReconcileDeploymentSpecHashMismatchTriggersUncachedUpdate(t *testing.T) {
	t.Setenv("USE_HOSTPATH", "true")
	t.Setenv("WORKSPACE_DATA_DIR", t.TempDir())
	t.Setenv("EGRESS_ENABLED", "false")
	scheme := newScaleTestScheme(t)

	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	cacheReader := newRecordingClient(base)
	cacheReader.stripDeployGet = true
	apiReader := newRecordingClient(base)
	r := &WorkspaceReconciler{
		Client:    cacheReader,
		APIReader: apiReader,
		Scheme:    scheme,
	}
	ws := &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-drift", Namespace: "work"},
		Spec: aiv1alpha1.WorkspaceSpec{
			Owner:   "alice",
			Runtime: aiv1alpha1.RuntimeSpec{Image: "nginx:alpine"},
			Storage: aiv1alpha1.StorageSpec{Size: "1Gi"},
		},
	}

	ctx := context.Background()
	if _, _, err := r.reconcileDeployment(ctx, ws, "ws-drift-pvc", nil); err != nil {
		t.Fatalf("initial reconcileDeployment failed: %v", err)
	}

	// Drift 1: image changes -> spec-hash mismatch triggers uncached Get + Update.
	ws.Spec.Runtime.Image = "nginx:1.27"
	apiReader.resetCounts()
	cacheReader.resetCounts()
	if _, _, err := r.reconcileDeployment(ctx, ws, "ws-drift-pvc", nil); err != nil {
		t.Fatalf("image update reconcileDeployment failed: %v", err)
	}
	if apiReader.gets["Deployment"] != 1 || cacheReader.updates != 1 {
		t.Fatalf("expected 1 uncached Deployment Get and 1 Update on hash mismatch, got gets=%d updates=%d",
			apiReader.gets["Deployment"], cacheReader.updates)
	}

	// Drift 2: RevisionHistoryLimit drifted -> triggers uncached Get + Update back to 0.
	deploy := &appsv1.Deployment{}
	if err := base.Get(ctx, client.ObjectKey{Namespace: "work", Name: "ws-drift-deploy"}, deploy); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	legacyRev := int32(10)
	deploy.Spec.RevisionHistoryLimit = &legacyRev
	if err := base.Update(ctx, deploy); err != nil {
		t.Fatalf("corrupt revisionHistoryLimit: %v", err)
	}
	apiReader.resetCounts()
	cacheReader.resetCounts()
	if _, _, err := r.reconcileDeployment(ctx, ws, "ws-drift-pvc", nil); err != nil {
		t.Fatalf("revisionHistoryLimit drift reconcile failed: %v", err)
	}
	if apiReader.gets["Deployment"] != 1 || cacheReader.updates != 1 {
		t.Fatalf("expected 1 uncached Deployment Get and 1 Update on revisionHistoryLimit drift, got gets=%d updates=%d",
			apiReader.gets["Deployment"], cacheReader.updates)
	}

	// Allowlist unmigrated slow path must still fail closed when proxy is missing.
	t.Setenv("EGRESS_ENABLED", "true")
	t.Setenv("EGRESS_CONFIG_NAMESPACE", "system")
	t.Setenv("EGRESS_PROTECTED_CIDRS", "10.0.0.0/8")
	allowCfg := egress.Config{
		Mode: egress.ModeAllowlist,
		Services: []egress.Service{
			{Name: "dns", Namespace: "kube-system", PodSelector: map[string]string{"k8s-app": "kube-dns"}, Protocol: "TCP", Port: 53},
			{Name: "dns", Namespace: "kube-system", PodSelector: map[string]string{"k8s-app": "kube-dns"}, Protocol: "UDP", Port: 53},
		},
	}
	if err := egress.Save(ctx, base, "system", "", allowCfg); err != nil {
		t.Fatalf("save allowlist config: %v", err)
	}
	if _, _, err := r.reconcileDeployment(ctx, ws, "ws-drift-pvc", nil); err == nil || !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("expected fail-closed proxy error on unmigrated allowlist rollout, got %v", err)
	}
}

func TestReconcileIdleWorkspaceShortCircuitZeroDirectReads(t *testing.T) {
	resetPhaseTracker(t)
	resetDerivedGauges(t)
	t.Setenv("EGRESS_ENABLED", "false")

	scheme := newScaleTestScheme(t)
	zero := int32(0)
	uid := int64(10001)
	lastActive := metav1.NewTime(time.Now().Add(-2 * time.Hour))

	cases := []struct {
		name       string
		stopped    bool
		phase      aiv1alpha1.WorkspacePhase
		withDeploy bool
	}{
		{name: "sleeping-with-zero-deploy", stopped: false, phase: aiv1alpha1.WorkspaceSleeping, withDeploy: true},
		{name: "sleeping-without-deploy", stopped: false, phase: aiv1alpha1.WorkspaceSleeping, withDeploy: false},
		{name: "stopped-with-zero-deploy", stopped: true, phase: aiv1alpha1.WorkspaceStopped, withDeploy: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := &aiv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{Name: "ws-idle", Namespace: "work"},
				Spec: aiv1alpha1.WorkspaceSpec{
					Owner:       "alice",
					Stopped:     tc.stopped,
					IdleTimeout: "30m",
					Runtime:     aiv1alpha1.RuntimeSpec{Image: "nginx:alpine"},
					Storage:     aiv1alpha1.StorageSpec{Size: "1Gi"},
				},
				Status: aiv1alpha1.WorkspaceStatus{
					Phase:          tc.phase,
					PVCName:        "ws-idle-pvc",
					Endpoint:       "ws-idle.localhost",
					LastActiveTime: &lastActive,
					RunAsUser:      &uid,
				},
			}
			sharedNetpol := &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sharedNetpolName,
					Namespace: "work",
					Labels:    map[string]string{"app": "workspace", managedByLabelKey: managedByLabelValue},
				},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{managedByLabelKey: managedByLabelValue}},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
					Ingress:     []networkingv1.NetworkPolicyIngressRule{{}},
					Egress:      buildSharedEgressRules(nil),
				},
			}
			objs := []client.Object{ws, sharedNetpol}
			if tc.withDeploy {
				objs = append(objs, &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "ws-idle-deploy", Namespace: "work"},
					Spec: appsv1.DeploymentSpec{
						Replicas: &zero,
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								SecurityContext: &corev1.PodSecurityContext{RunAsUser: &uid},
							},
						},
					},
				})
			}

			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&aiv1alpha1.Workspace{}).Build()
			cacheClient := newRecordingClient(base)
			apiReader := newRecordingClient(base)
			r := &WorkspaceReconciler{
				Client:    cacheClient,
				APIReader: apiReader,
				Scheme:    scheme,
			}

			res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "work", Name: "ws-idle"}})
			if err != nil {
				t.Fatalf("short-circuit Reconcile failed: %v", err)
			}
			if res.RequeueAfter != 0 {
				t.Fatalf("expected RequeueAfter=0 for idle workspace, got %v", res.RequeueAfter)
			}
			if apiReader.totalReads() != 0 {
				t.Fatalf("expected 0 APIReader calls on short-circuit, got gets=%v lists=%v", apiReader.gets, apiReader.lists)
			}
			for _, kind := range []string{"PVC", "PV", "Service", "Ingress"} {
				if cacheClient.gets[kind] != 0 {
					t.Fatalf("expected 0 %s Gets on short-circuit, got %d", kind, cacheClient.gets[kind])
				}
			}
			if cacheClient.totalWrites() != 0 {
				t.Fatalf("expected 0 writes on steady short-circuit, got %d", cacheClient.totalWrites())
			}

			// If the shared NetworkPolicy is deleted, the short-circuit path still heals it.
			if err := base.Delete(context.Background(), sharedNetpol); err != nil {
				t.Fatalf("delete shared netpol: %v", err)
			}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "work", Name: "ws-idle"}}); err != nil {
				t.Fatalf("short-circuit netpol self-heal failed: %v", err)
			}
			if err := base.Get(context.Background(), client.ObjectKey{Namespace: "work", Name: sharedNetpolName}, &networkingv1.NetworkPolicy{}); err != nil {
				t.Fatalf("expected shared NetworkPolicy to be recreated during short-circuit: %v", err)
			}
		})
	}
}

func TestResolveRunAsUserReusesFetchedPVC(t *testing.T) {
	scheme := newScaleTestScheme(t)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pvc-a"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-a"},
	}
	pv := ossPV("pv-a", "-o uid=10001,gid=10001,umask=022")
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc, pv).Build()
	rec := newRecordingClient(base)
	r := &WorkspaceReconciler{
		Client:           rec,
		DefaultRunAsUser: 10001,
	}
	ws := &aiv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "w"}}

	uid, skip, err := r.resolveRunAsUser(context.Background(), ws, pvc.Name, pvc)
	if err != nil || skip != "" || uid != 10001 {
		t.Fatalf("unexpected resolveRunAsUser result: uid=%d skip=%q err=%v", uid, skip, err)
	}
	if rec.gets["PVC"] != 0 {
		t.Fatalf("expected 0 PVC Gets when fetched PVC is passed in, got %d", rec.gets["PVC"])
	}
	if rec.gets["PV"] != 1 {
		t.Fatalf("expected 1 PV Get, got %d", rec.gets["PV"])
	}
}

func TestMaybeDeleteSharedNetworkPolicyAvoidsFullList(t *testing.T) {
	resetPhaseTracker(t)
	resetDerivedGauges(t)
	scheme := newScaleTestScheme(t)

	sharedNetpol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: sharedNetpolName, Namespace: "work"},
	}
	wsA := &aiv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "ws-a", Namespace: "work"}}
	wsB := &aiv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "ws-b", Namespace: "work"}}

	t.Run("tracker reports sibling alive => 0 Lists and netpol kept", func(t *testing.T) {
		resetPhaseTracker(t)
		base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sharedNetpol, wsB).Build()
		rec := newRecordingClient(base)
		r := &WorkspaceReconciler{Client: rec, APIReader: rec, Scheme: scheme}

		workspacePhases.set(types.NamespacedName{Namespace: "work", Name: "ws-b"}, aiv1alpha1.WorkspaceRunning)
		if err := r.maybeDeleteSharedNetworkPolicy(context.Background(), "work", types.NamespacedName{Namespace: "work", Name: "ws-a"}); err != nil {
			t.Fatalf("maybeDeleteSharedNetworkPolicy failed: %v", err)
		}
		if rec.lists["Workspace"] != 0 {
			t.Fatalf("expected 0 Workspace List calls when tracker knows sibling exists, got %d", rec.lists["Workspace"])
		}
		if err := base.Get(context.Background(), client.ObjectKeyFromObject(sharedNetpol), &networkingv1.NetworkPolicy{}); err != nil {
			t.Fatalf("shared NetworkPolicy was unexpectedly deleted: %v", err)
		}
	})

	t.Run("cold-start empty tracker with live workspace => Limit(2) and netpol kept", func(t *testing.T) {
		resetPhaseTracker(t)
		base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sharedNetpol, wsB).Build()
		rec := newRecordingClient(base)
		r := &WorkspaceReconciler{Client: rec, APIReader: rec, Scheme: scheme}

		if err := r.maybeDeleteSharedNetworkPolicy(context.Background(), "work", types.NamespacedName{Namespace: "work", Name: "ws-a"}); err != nil {
			t.Fatalf("maybeDeleteSharedNetworkPolicy failed: %v", err)
		}
		if rec.lists["Workspace"] != 1 || len(rec.listLimits) != 1 || rec.listLimits[0] != 2 {
			t.Fatalf("expected 1 Workspace List with Limit(2), got lists=%d limits=%v", rec.lists["Workspace"], rec.listLimits)
		}
		if err := base.Get(context.Background(), client.ObjectKeyFromObject(sharedNetpol), &networkingv1.NetworkPolicy{}); err != nil {
			t.Fatalf("shared NetworkPolicy was unexpectedly deleted: %v", err)
		}
	})

	t.Run("last workspace gone => Limit(2) confirms empty and deletes netpol", func(t *testing.T) {
		resetPhaseTracker(t)
		base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sharedNetpol, wsA).Build()
		rec := newRecordingClient(base)
		r := &WorkspaceReconciler{Client: rec, APIReader: rec, Scheme: scheme}

		if err := r.maybeDeleteSharedNetworkPolicy(context.Background(), "work", types.NamespacedName{Namespace: "work", Name: "ws-a"}); err != nil {
			t.Fatalf("maybeDeleteSharedNetworkPolicy failed: %v", err)
		}
		if err := base.Get(context.Background(), client.ObjectKeyFromObject(sharedNetpol), &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
			t.Fatalf("expected shared NetworkPolicy to be deleted, got %v", err)
		}
	})
}

func TestIdleTimeoutJitterAndRequeue(t *testing.T) {
	seen := make(map[time.Duration]struct{})
	for _, name := range []string{"ws-alpha", "ws-beta", "ws-gamma", "ws-delta", "ws-epsilon"} {
		j1 := idleTimeoutJitter(name)
		j2 := idleTimeoutJitter(name)
		if j1 != j2 {
			t.Fatalf("jitter must be deterministic for %s: %v != %v", name, j1, j2)
		}
		if j1 < 0 || j1 >= maxIdleTimeoutJitter {
			t.Fatalf("jitter for %s out of range [0, %v): %v", name, maxIdleTimeoutJitter, j1)
		}
		seen[j1] = struct{}{}
	}
	if len(seen) < 3 {
		t.Fatalf("expected distinct jitter values across names, got %d", len(seen))
	}

	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	jitter := idleTimeoutJitter("ws-alpha")

	// Before expiry: remaining + jitter.
	requeueFuture := computeIdleRequeue("ws-alpha", now.Add(-10*time.Minute), 30*time.Minute, now)
	if want := 20*time.Minute + jitter; requeueFuture != want {
		t.Fatalf("future requeue=%v, want %v", requeueFuture, want)
	}

	// After expiry: 1s + jitter.
	requeueExpired := computeIdleRequeue("ws-alpha", now.Add(-40*time.Minute), 30*time.Minute, now)
	if want := time.Second + jitter; requeueExpired != want {
		t.Fatalf("expired requeue=%v, want %v", requeueExpired, want)
	}
}

func TestProbeAndRevisionHistoryTuning(t *testing.T) {
	if workspaceRevisionHistoryLimit != 0 {
		t.Fatalf("expected workspaceRevisionHistoryLimit=0, got %d", workspaceRevisionHistoryLimit)
	}

	for _, healthPath := range []string{"", "/healthz"} {
		ws := &aiv1alpha1.Workspace{
			Spec: aiv1alpha1.WorkspaceSpec{
				Runtime: aiv1alpha1.RuntimeSpec{HealthPath: healthPath},
			},
		}
		p := getReadinessProbe(ws)
		if p.PeriodSeconds != 10 || p.TimeoutSeconds != 5 || p.FailureThreshold != 3 {
			t.Fatalf("unexpected readiness probe for healthPath=%q: %+v", healthPath, p)
		}
		sp := getStartupProbe(ws)
		if sp.PeriodSeconds != 2 {
			t.Fatalf("expected startup probe PeriodSeconds=2 preserved, got %d", sp.PeriodSeconds)
		}
	}
}
