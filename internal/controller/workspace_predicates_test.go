package controller

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

func TestWorkspacePredicate(t *testing.T) {
	pred := workspacePredicate()

	t.Run("Create and Delete events return true", func(t *testing.T) {
		if !pred.Create(event.CreateEvent{}) {
			t.Error("expected Create to return true")
		}
		if !pred.Delete(event.DeleteEvent{}) {
			t.Error("expected Delete to return true")
		}
	})

	t.Run("Update with nil objects returns false", func(t *testing.T) {
		if pred.Update(event.UpdateEvent{}) {
			t.Error("expected Update with nil objects to return false")
		}
	})

	t.Run("Update with Generation changed returns true", func(t *testing.T) {
		oldWs := &aiv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
		newWs := &aiv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
		if !pred.Update(event.UpdateEvent{ObjectOld: oldWs, ObjectNew: newWs}) {
			t.Error("expected Update with Generation changed to return true")
		}
	})

	t.Run("Update with DeletionTimestamp changed returns true", func(t *testing.T) {
		now := metav1.Now()
		oldWs := &aiv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
		newWs := &aiv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Generation: 1, DeletionTimestamp: &now}}
		if !pred.Update(event.UpdateEvent{ObjectOld: oldWs, ObjectNew: newWs}) {
			t.Error("expected Update with DeletionTimestamp changed to return true")
		}
	})

	t.Run("Update with Finalizers changed returns true", func(t *testing.T) {
		oldWs := &aiv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Generation: 1, Finalizers: []string{workspaceFinalizer}}}
		newWs := &aiv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
		if !pred.Update(event.UpdateEvent{ObjectOld: oldWs, ObjectNew: newWs}) {
			t.Error("expected Update with Finalizers changed to return true")
		}
	})

	t.Run("Update with Labels changed returns true", func(t *testing.T) {
		oldWs := &aiv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Generation: 1, Labels: map[string]string{"oa": "user1"}}}
		newWs := &aiv1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Generation: 1, Labels: map[string]string{"oa": "user2"}}}
		if !pred.Update(event.UpdateEvent{ObjectOld: oldWs, ObjectNew: newWs}) {
			t.Error("expected Update with Labels changed to return true")
		}
	})

	t.Run("Update with LastActiveTime changed returns true", func(t *testing.T) {
		t1 := metav1.NewTime(time.Now().Add(-1 * time.Hour))
		t2 := metav1.NewTime(time.Now())
		oldWs := &aiv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Generation: 1},
			Status:     aiv1alpha1.WorkspaceStatus{LastActiveTime: &t1},
		}
		newWs := &aiv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Generation: 1},
			Status:     aiv1alpha1.WorkspaceStatus{LastActiveTime: &t2},
		}
		if !pred.Update(event.UpdateEvent{ObjectOld: oldWs, ObjectNew: newWs}) {
			t.Error("expected Update with LastActiveTime changed to return true")
		}
	})

	t.Run("Update with only Phase, PodName, or Endpoint changed returns false", func(t *testing.T) {
		t1 := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		oldWs := &aiv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Generation: 1},
			Status: aiv1alpha1.WorkspaceStatus{
				Phase:          aiv1alpha1.WorkspaceStarting,
				PodName:        "",
				Endpoint:       "",
				PVCName:        "ws-pvc",
				LastActiveTime: &t1,
			},
		}
		newWs := &aiv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Generation: 1},
			Status: aiv1alpha1.WorkspaceStatus{
				Phase:          aiv1alpha1.WorkspaceRunning,
				PodName:        "ws-pod-xyz",
				Endpoint:       "ws.domain.com",
				PVCName:        "ws-pvc",
				LastActiveTime: &t1,
			},
		}
		if pred.Update(event.UpdateEvent{ObjectOld: oldWs, ObjectNew: newWs}) {
			t.Error("expected status-only update (Phase/PodName/Endpoint) to return false and be ignored")
		}
	})
}

func TestDeploymentPredicate(t *testing.T) {
	pred := deploymentPredicate()

	t.Run("Update with ReadyReplicas changed returns true", func(t *testing.T) {
		oldDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Generation: 1},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 0},
		}
		newDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Generation: 1},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
		}
		if !pred.Update(event.UpdateEvent{ObjectOld: oldDeploy, ObjectNew: newDeploy}) {
			t.Error("expected Update with ReadyReplicas changed to return true")
		}
	})

	t.Run("Update with Generation changed returns true", func(t *testing.T) {
		oldDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
		newDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
		if !pred.Update(event.UpdateEvent{ObjectOld: oldDeploy, ObjectNew: newDeploy}) {
			t.Error("expected Update with Generation changed to return true")
		}
	})

	t.Run("Update with unchanged ReadyReplicas and Generation returns false", func(t *testing.T) {
		oldDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Generation: 1},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 1, Replicas: 1},
		}
		newDeploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Generation: 1},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 1, Replicas: 1, UpdatedReplicas: 1},
		}
		if pred.Update(event.UpdateEvent{ObjectOld: oldDeploy, ObjectNew: newDeploy}) {
			t.Error("expected update without Generation/ReadyReplicas change to return false")
		}
	})
}

func TestSharedNetpolPredicate(t *testing.T) {
	pred := sharedNetpolPredicate()

	newNetpol := func(name string) *networkingv1.NetworkPolicy {
		return &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		}
	}

	t.Run("Create and Delete events only match the shared template", func(t *testing.T) {
		if !pred.Create(event.CreateEvent{Object: newNetpol(sharedNetpolName)}) {
			t.Error("expected Create for the shared template to return true")
		}
		if pred.Create(event.CreateEvent{Object: newNetpol("some-ws-netpol")}) {
			t.Error("expected Create for a legacy per-workspace policy to return false")
		}
		if pred.Create(event.CreateEvent{}) {
			t.Error("expected Create with nil object to return false")
		}
		if !pred.Delete(event.DeleteEvent{Object: newNetpol(sharedNetpolName)}) {
			t.Error("expected Delete for the shared template to return true")
		}
		if pred.Delete(event.DeleteEvent{Object: newNetpol("some-ws-netpol")}) {
			t.Error("expected Delete for a legacy per-workspace policy to return false")
		}
	})

	t.Run("Update only reacts to content changes of the shared template", func(t *testing.T) {
		if pred.Update(event.UpdateEvent{ObjectOld: newNetpol("some-ws-netpol"), ObjectNew: newNetpol("some-ws-netpol")}) {
			t.Error("expected Update for legacy policies to return false")
		}

		oldShared := newNetpol(sharedNetpolName)
		unchanged := newNetpol(sharedNetpolName)
		if pred.Update(event.UpdateEvent{ObjectOld: oldShared, ObjectNew: unchanged}) {
			t.Error("expected Update without content change to return false")
		}

		changed := newNetpol(sharedNetpolName)
		changed.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
		if !pred.Update(event.UpdateEvent{ObjectOld: oldShared, ObjectNew: changed}) {
			t.Error("expected Update with spec change of the shared template to return true")
		}
	})
}
