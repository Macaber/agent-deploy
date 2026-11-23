package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

func TestRunAsUserFromAnnotation(t *testing.T) {
	cases := []struct {
		name    string
		ann     map[string]string
		uid     int64
		set     bool
		wantErr bool
	}{
		{"absent", nil, 0, false, false},
		{"empty", map[string]string{runAsUserAnnotation: ""}, 0, false, false},
		{"valid", map[string]string{runAsUserAnnotation: "10001"}, 10001, true, false},
		{"spaces", map[string]string{runAsUserAnnotation: " 10001 "}, 10001, true, false},
		{"zero is opt-out", map[string]string{runAsUserAnnotation: "0"}, 0, true, false},
		{"negative", map[string]string{runAsUserAnnotation: "-1"}, 0, false, true},
		{"non numeric", map[string]string{runAsUserAnnotation: "abc"}, 0, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws := &aiv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Annotations: c.ann}}
			uid, set, err := runAsUserFromAnnotation(ws)
			if (err != nil) != c.wantErr || uid != c.uid || set != c.set {
				t.Fatalf("got (%d,%v,%v), want (%d,%v,err=%v)", uid, set, err, c.uid, c.set, c.wantErr)
			}
		})
	}
}

func TestApplyNonRoot(t *testing.T) {
	psc := &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	csc := &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"CHOWN"}}}
	applyNonRoot(psc, csc, 10001)
	if *psc.RunAsUser != 10001 || *psc.RunAsGroup != 10001 || !*psc.RunAsNonRoot {
		t.Fatalf("bad pod security context: %+v", psc)
	}
	if psc.FSGroup != nil || psc.SeccompProfile == nil {
		t.Fatalf("FSGroup must stay nil and seccomp kept: %+v", psc)
	}
	if len(csc.Capabilities.Add) != 0 || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("capabilities must be drop ALL only: %+v", csc.Capabilities)
	}
}

func TestMountOptionsMapUID(t *testing.T) {
	cases := []struct {
		opts string
		want bool
	}{
		{"-o max_stat_cache_size=100000 -o allow_other", false},
		{"-o allow_other -o uid=10001,gid=10001,umask=022", true},
		{"-o uid=10001 -o gid=10001", true},
		{"-o uid=10001", false},
		{"-o uid=1000,gid=1000", false},
		{"-o uid=100010,gid=100010", false},
		{"", false},
	}
	for _, c := range cases {
		if got := mountOptionsMapUID(c.opts, 10001); got != c.want {
			t.Errorf("%q: got %v want %v", c.opts, got, c.want)
		}
	}
}

func ossPV(name, opts string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: csiDriverOSS, VolumeHandle: name,
				VolumeAttributes: map[string]string{"otherOpts": opts}},
		}},
	}
}

func TestResolveRunAsUser(t *testing.T) {
	const ns, pvcName = "ns", "pvc-a"
	boundPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: pvcName},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-a"},
	}
	nfsPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-a"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: "nas.csi.alibabacloud.com", VolumeHandle: "x"}}},
	}
	cases := []struct {
		name     string
		def      int64
		ann      map[string]string
		objs     []client.Object
		pvcName  string
		wantUID  int64
		wantSkip bool
	}{
		{"ready PV gets default", 10001, nil, []client.Object{boundPVC, ossPV("pv-a", "-o uid=10001,gid=10001,umask=022")}, pvcName, 10001, false},
		{"legacy PV stays root", 10001, nil, []client.Object{boundPVC, ossPV("pv-a", "-o allow_other")}, pvcName, 0, true},
		{"missing PVC stays root", 10001, nil, nil, pvcName, 0, true},
		{"unbound PVC stays root", 10001, nil, []client.Object{&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: pvcName}}}, pvcName, 0, true},
		{"non-OSS PV stays root", 10001, nil, []client.Object{boundPVC, nfsPV}, pvcName, 0, true},
		{"no PVC name stays root", 10001, nil, nil, "", 0, true},
		{"annotation 0 opts out silently", 10001, map[string]string{runAsUserAnnotation: "0"}, []client.Object{boundPVC, ossPV("pv-a", "-o uid=10001,gid=10001")}, pvcName, 0, false},
		{"annotation overrides uid", 10001, map[string]string{runAsUserAnnotation: "20000"}, []client.Object{boundPVC, ossPV("pv-a", "-o uid=20000,gid=20000")}, pvcName, 20000, false},
		{"annotation uid not on PV", 10001, map[string]string{runAsUserAnnotation: "20000"}, []client.Object{boundPVC, ossPV("pv-a", "-o uid=10001,gid=10001")}, pvcName, 0, true},
		{"invalid annotation falls back to default", 10001, map[string]string{runAsUserAnnotation: "x"}, []client.Object{boundPVC, ossPV("pv-a", "-o uid=10001,gid=10001")}, pvcName, 10001, false},
		{"default disabled", 0, nil, nil, pvcName, 0, false},
		{"default disabled but annotation set", 0, map[string]string{runAsUserAnnotation: "10001"}, []client.Object{boundPVC, ossPV("pv-a", "-o uid=10001,gid=10001")}, pvcName, 10001, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = aiv1alpha1.AddToScheme(scheme)
			r := &WorkspaceReconciler{
				Client:           fake.NewClientBuilder().WithScheme(scheme).WithObjects(c.objs...).Build(),
				DefaultRunAsUser: c.def,
			}
			ws := &aiv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "w", Annotations: c.ann}}
			uid, skip, err := r.resolveRunAsUser(context.Background(), ws, c.pvcName)
			if err != nil {
				t.Fatal(err)
			}
			if uid != c.wantUID || (skip != "") != c.wantSkip {
				t.Fatalf("got uid=%d skip=%q, want uid=%d skip=%v", uid, skip, c.wantUID, c.wantSkip)
			}
		})
	}
}
