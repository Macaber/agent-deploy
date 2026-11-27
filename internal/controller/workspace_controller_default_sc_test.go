package controller

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

func TestReconcilePVC_DefaultStorageClass_OSS(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = storagev1.AddToScheme(scheme)
	_ = aiv1alpha1.AddToScheme(scheme)

	defaultSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "alicloud-oss",
			Annotations: map[string]string{
				"storageclass.kubernetes.io/is-default-class": "true",
			},
		},
		Provisioner: "ossplugin.csi.alibabacloud.com",
		Parameters: map[string]string{
			"bucket": "test-bucket",
			"url":    "oss-cn-hangzhou-internal.aliyuncs.com",
			"region": "cn-hangzhou",
		},
	}

	ws := &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-user1",
			Namespace: "default",
		},
		Spec: aiv1alpha1.WorkspaceSpec{
			Owner: "user1",
			Storage: aiv1alpha1.StorageSpec{
				Size: "10Gi",
				// StorageClass is empty, expecting to fall back to default SC
			},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(defaultSC, ws).
		Build()

	r := &WorkspaceReconciler{
		Client: k8sClient,
		Scheme: scheme,
	}

	ctx := context.Background()
	pvc, err := r.reconcilePVC(ctx, ws)
	if err != nil {
		t.Fatalf("unexpected error from reconcilePVC: %v", err)
	}
	if pvc == nil || pvc.Name != "ws-user1-pvc" {
		t.Fatalf("expected pvcName ws-user1-pvc, got %v", pvc)
	}

	// Verify PVC was created with OSS settings (ReadWriteMany, bound to static PV)
	fetchedPVC := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ws-user1-pvc"}, fetchedPVC); err != nil {
		t.Fatalf("failed to get created PVC: %v", err)
	}

	if fetchedPVC.Spec.StorageClassName == nil || *fetchedPVC.Spec.StorageClassName != "alicloud-oss" {
		t.Errorf("expected StorageClassName 'alicloud-oss', got %v", fetchedPVC.Spec.StorageClassName)
	}
	if fetchedPVC.Spec.VolumeName != "ws-user1-pv" {
		t.Errorf("expected VolumeName 'ws-user1-pv', got %s", fetchedPVC.Spec.VolumeName)
	}
	if len(fetchedPVC.Spec.AccessModes) == 0 || fetchedPVC.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("expected accessMode ReadWriteMany, got %v", fetchedPVC.Spec.AccessModes)
	}

	// Verify PV was created with matching parameters from default SC
	pv := &corev1.PersistentVolume{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "ws-user1-pv"}, pv); err != nil {
		t.Fatalf("failed to get created PV: %v", err)
	}

	if pv.Spec.StorageClassName != "alicloud-oss" {
		t.Errorf("expected PV StorageClassName 'alicloud-oss', got %s", pv.Spec.StorageClassName)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != "ossplugin.csi.alibabacloud.com" {
		t.Errorf("expected CSI driver ossplugin.csi.alibabacloud.com, got %v", pv.Spec.CSI)
	}
	if pv.Spec.CSI.VolumeAttributes["bucket"] != "test-bucket" {
		t.Errorf("expected bucket 'test-bucket', got %s", pv.Spec.CSI.VolumeAttributes["bucket"])
	}
}

func TestReconcilePVC_ExplicitStorageClass_OverridesDefault(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = storagev1.AddToScheme(scheme)
	_ = aiv1alpha1.AddToScheme(scheme)

	defaultSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "alicloud-oss",
			Annotations: map[string]string{
				"storageclass.kubernetes.io/is-default-class": "true",
			},
		},
		Provisioner: "ossplugin.csi.alibabacloud.com",
	}

	explicitSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "standard-nfs",
		},
		Provisioner: "nfs-client",
	}

	ws := &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-user2",
			Namespace: "default",
		},
		Spec: aiv1alpha1.WorkspaceSpec{
			Owner: "user2",
			Storage: aiv1alpha1.StorageSpec{
				Size:         "5Gi",
				StorageClass: "standard-nfs", // explicitly provided
			},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(defaultSC, explicitSC, ws).
		Build()

	r := &WorkspaceReconciler{
		Client: k8sClient,
		Scheme: scheme,
	}

	ctx := context.Background()
	pvc, err := r.reconcilePVC(ctx, ws)
	if err != nil {
		t.Fatalf("unexpected error from reconcilePVC: %v", err)
	}

	fetchedPVC := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: pvc.Name}, fetchedPVC); err != nil {
		t.Fatalf("failed to get created PVC: %v", err)
	}

	// Must use explicit storageClass "standard-nfs", NOT "alicloud-oss"
	if fetchedPVC.Spec.StorageClassName == nil || *fetchedPVC.Spec.StorageClassName != "standard-nfs" {
		t.Errorf("expected StorageClassName 'standard-nfs', got %v", fetchedPVC.Spec.StorageClassName)
	}
	if fetchedPVC.Spec.VolumeName != "" {
		t.Errorf("expected empty VolumeName for non-OSS dynamic PVC, got %s", fetchedPVC.Spec.VolumeName)
	}
	if len(fetchedPVC.Spec.AccessModes) == 0 || fetchedPVC.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("expected accessMode ReadWriteOnce, got %v", fetchedPVC.Spec.AccessModes)
	}
}

func TestReconcilePVC_EnvDefaultStorageClass(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = storagev1.AddToScheme(scheme)
	_ = aiv1alpha1.AddToScheme(scheme)

	envSC := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "env-oss",
		},
		Provisioner: "ossplugin.csi.alibabacloud.com",
		Parameters: map[string]string{
			"bucket": "env-bucket",
		},
	}

	ws := &aiv1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-user3",
			Namespace: "default",
		},
		Spec: aiv1alpha1.WorkspaceSpec{
			Owner: "user3",
			Storage: aiv1alpha1.StorageSpec{
				Size: "10Gi",
			},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(envSC, ws).
		Build()

	r := &WorkspaceReconciler{
		Client: k8sClient,
		Scheme: scheme,
	}

	os.Setenv("DEFAULT_STORAGE_CLASS", "env-oss")
	defer os.Unsetenv("DEFAULT_STORAGE_CLASS")

	ctx := context.Background()
	pvc, err := r.reconcilePVC(ctx, ws)
	if err != nil {
		t.Fatalf("unexpected error from reconcilePVC: %v", err)
	}

	fetchedPVC := &corev1.PersistentVolumeClaim{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: pvc.Name}, fetchedPVC); err != nil {
		t.Fatalf("failed to get created PVC: %v", err)
	}

	if fetchedPVC.Spec.StorageClassName == nil || *fetchedPVC.Spec.StorageClassName != "env-oss" {
		t.Errorf("expected StorageClassName 'env-oss', got %v", fetchedPVC.Spec.StorageClassName)
	}
	if fetchedPVC.Spec.VolumeName != "ws-user3-pv" {
		t.Errorf("expected VolumeName 'ws-user3-pv', got %s", fetchedPVC.Spec.VolumeName)
	}
}
