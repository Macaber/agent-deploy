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

// Revision history bound: every Pod template change under the Recreate strategy
// leaves a 0-replica ReplicaSet behind, and the API-server default keeps 10 of
// them per Deployment forever. This suite pins the operator-set bound and the
// convergence of Deployments that still carry the default.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

var _ = Describe("Workspace Deployment revision history bound", func() {
	const (
		name      = "revhistory-ws"
		namespace = "default"
	)

	ctx := context.Background()
	wsKey := types.NamespacedName{Name: name, Namespace: namespace}
	deployKey := types.NamespacedName{Name: name + "-deploy", Namespace: namespace}

	reconciler := func() *WorkspaceReconciler {
		return &WorkspaceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

	reconcileOnce := func() error {
		_, err := reconciler().Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
		return err
	}

	cleanup := func() {
		ws := &aiv1alpha1.Workspace{}
		if err := k8sClient.Get(ctx, wsKey, ws); err == nil {
			_ = k8sClient.Delete(ctx, ws)
		}
		_ = k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deployKey.Name, Namespace: namespace}})
		_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name + "-svc", Namespace: namespace}})
		_ = k8sClient.Delete(ctx, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name + "-ingress", Namespace: namespace}})
		_ = k8sClient.Delete(ctx, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: sharedNetpolName, Namespace: namespace}})
	}

	getDeployment := func() *appsv1.Deployment {
		deploy := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, deployKey, deploy)).To(Succeed())
		return deploy
	}

	BeforeEach(func() {
		cleanup()
		Expect(k8sClient.Create(ctx, &aiv1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: aiv1alpha1.WorkspaceSpec{
				Owner:   "revhistory-owner",
				Runtime: aiv1alpha1.RuntimeSpec{Image: "nginx:alpine"},
				Storage: aiv1alpha1.StorageSpec{Size: "1Gi"},
			},
		})).To(Succeed())
		// The first reconcile initializes status, the second one creates the children.
		Expect(reconcileOnce()).To(Succeed())
		Expect(reconcileOnce()).To(Succeed())
	})

	AfterEach(func() {
		cleanup()
	})

	It("creates the Deployment with a bounded revision history", func() {
		deploy := getDeployment()
		Expect(deploy.Spec.RevisionHistoryLimit).NotTo(BeNil())
		Expect(*deploy.Spec.RevisionHistoryLimit).To(Equal(workspaceRevisionHistoryLimit))
		Expect(workspaceRevisionHistoryLimit).To(BeNumerically("<", 10),
			"the bound must actually reduce the API-server default of 10")
	})

	It("converges a Deployment that still carries the API-server default", func() {
		By("simulating a Deployment created before the bound was introduced")
		deploy := getDeployment()
		legacyLimit := int32(10)
		deploy.Spec.RevisionHistoryLimit = &legacyLimit
		Expect(k8sClient.Update(ctx, deploy)).To(Succeed())

		Expect(reconcileOnce()).To(Succeed())

		Expect(*getDeployment().Spec.RevisionHistoryLimit).To(Equal(workspaceRevisionHistoryLimit))
	})

	It("does not rewrite the Deployment once it is converged (steady-state zero writes)", func() {
		deploy := getDeployment()
		resourceVersion := deploy.ResourceVersion

		By("reconciling a steady-state workspace")
		Expect(reconcileOnce()).To(Succeed())

		Expect(getDeployment().ResourceVersion).To(Equal(resourceVersion),
			"a converged Deployment must not be updated on every reconcile")
	})

	It("does not fight a Deployment that was deleted and drifted out of the bound", func() {
		By("deleting the Deployment and letting the operator recreate it")
		Expect(k8sClient.Delete(ctx, getDeployment())).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, deployKey, &appsv1.Deployment{}))
		}).Should(BeTrue())

		Expect(reconcileOnce()).To(Succeed())

		Expect(*getDeployment().Spec.RevisionHistoryLimit).To(Equal(workspaceRevisionHistoryLimit))
	})
})
