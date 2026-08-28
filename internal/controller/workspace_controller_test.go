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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	aiv1alpha1 "github.com/example/workspace-operator/api/v1alpha1"
)

var _ = Describe("Workspace Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		workspace := &aiv1alpha1.Workspace{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind Workspace")
			err := k8sClient.Get(ctx, typeNamespacedName, workspace)
			if err != nil && errors.IsNotFound(err) {
				kataRuntime := "kata"
				resource := &aiv1alpha1.Workspace{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: aiv1alpha1.WorkspaceSpec{
						Owner: "test-owner",
						Runtime: aiv1alpha1.RuntimeSpec{
							Image:            "nginx:alpine",
							RuntimeClassName: &kataRuntime,
						},
						Storage: aiv1alpha1.StorageSpec{
							Size: "1Gi",
						},
						NetworkPolicy: &aiv1alpha1.WorkspaceNetworkPolicySpec{},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// Cleanup logic after each test
			resource := &aiv1alpha1.Workspace{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				By("Cleanup the specific resource instance Workspace")
				_ = k8sClient.Delete(ctx, resource)
			}
		})

		It("should successfully reconcile security sandbox and NetworkPolicy", func() {
			By("Reconciling the workspace")
			controllerReconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// First reconcile adds finalizer, second reconcile creates child resources
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Deployment security settings (Kata RuntimeClass, no ServiceLinks, no SA token)")
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      resourceName + "-deploy",
				Namespace: resourceNamespace,
			}, deploy)).To(Succeed())

			Expect(deploy.Spec.Template.Spec.RuntimeClassName).NotTo(BeNil())
			Expect(*deploy.Spec.Template.Spec.RuntimeClassName).To(Equal("kata"))
			Expect(deploy.Spec.Template.Spec.EnableServiceLinks).NotTo(BeNil())
			Expect(*deploy.Spec.Template.Spec.EnableServiceLinks).To(BeFalse())
			Expect(deploy.Spec.Template.Spec.AutomountServiceAccountToken).NotTo(BeNil())
			Expect(*deploy.Spec.Template.Spec.AutomountServiceAccountToken).To(BeFalse())

			By("Verifying NetworkPolicy creation and isolation rules")
			netpol := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      resourceName + "-netpol",
				Namespace: resourceNamespace,
			}, netpol)).To(Succeed())

			Expect(netpol.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeIngress))
			Expect(netpol.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))
			Expect(len(netpol.Spec.Egress)).To(BeNumerically(">=", 2))

			By("Updating Workspace with custom BlockedCIDRs and AllowedCIDRs")
			resource := &aiv1alpha1.Workspace{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Spec.NetworkPolicy = &aiv1alpha1.WorkspaceNetworkPolicySpec{
				BlockedCIDRs: []string{"192.168.1.0/24", "10.0.0.0/16"},
				AllowedCIDRs: []string{"10.0.1.100/32"},
			}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      resourceName + "-netpol",
				Namespace: resourceNamespace,
			}, netpol)).To(Succeed())
			Expect(netpol.Spec.Egress[2].To[0].IPBlock.Except).To(ConsistOf("192.168.1.0/24", "10.0.0.0/16"))
			Expect(len(netpol.Spec.Egress)).To(Equal(4))
			Expect(netpol.Spec.Egress[3].To[0].IPBlock.CIDR).To(Equal("10.0.1.100/32"))

			By("Disabling NetworkPolicy via NetworkPolicy.Disabled and verifying deletion")
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Spec.NetworkPolicy.Disabled = true
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      resourceName + "-netpol",
				Namespace: resourceNamespace,
			}, netpol)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should successfully reconcile the resource with ConfigMap volume mounts", func() {
			By("updating the resource to include ConfigMapVolumeMounts")
			resource := &aiv1alpha1.Workspace{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Spec.ConfigMapVolumeMounts = []aiv1alpha1.ConfigMapVolumeMount{
				{
					ConfigMapName: "bocomwork-config",
					MountPath:     "/etc/bocomwork",
					ReadOnly:      true,
				},
			}
			resource.Spec.Runtime.InitContainers = []aiv1alpha1.InitContainerSpec{
				{
					Name:  "init-config",
					Image: "alpine:latest",
					ConfigMapVolumeMounts: []aiv1alpha1.ConfigMapVolumeMount{
						{
							ConfigMapName: "bocomwork-config",
							MountPath:     "/etc/bocomwork-init",
						},
					},
				},
			}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			By("Reconciling the updated resource")
			controllerReconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should preserve selector integrity when user provides custom labels", func() {
			By("updating the resource with user custom labels that attempt to override app and workspace")
			resource := &aiv1alpha1.Workspace{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Labels = map[string]string{
				"app":       "user-custom-app",
				"workspace": "override-ws",
				"custom":    "my-value",
			}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			By("Reconciling the workspace with conflicting user labels")
			controllerReconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Deployment labels and selector compatibility")
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      resourceName + "-deploy",
				Namespace: resourceNamespace,
			}, deploy)).To(Succeed())

			Expect(deploy.Spec.Selector.MatchLabels["app"]).To(Equal("workspace"))
			Expect(deploy.Spec.Selector.MatchLabels["workspace"]).To(Equal(resourceName))
			Expect(deploy.Spec.Template.Labels["app"]).To(Equal("workspace"))
			Expect(deploy.Spec.Template.Labels["workspace"]).To(Equal(resourceName))
			Expect(deploy.Spec.Template.Labels["custom"]).To(Equal("my-value"))
			Expect(deploy.Labels["app"]).To(Equal("workspace"))
			Expect(deploy.Labels["workspace"]).To(Equal(resourceName))
			Expect(deploy.Labels["custom"]).To(Equal("my-value"))

			By("Verifying Service labels and selector compatibility")
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      resourceName + "-svc",
				Namespace: resourceNamespace,
			}, svc)).To(Succeed())

			Expect(svc.Spec.Selector["app"]).To(Equal("workspace"))
			Expect(svc.Spec.Selector["workspace"]).To(Equal(resourceName))
			Expect(svc.Labels["app"]).To(Equal("workspace"))
			Expect(svc.Labels["workspace"]).To(Equal(resourceName))
			Expect(svc.Labels["custom"]).To(Equal("my-value"))

			By("Verifying Ingress labels")
			ingress := &networkingv1.Ingress{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      resourceName + "-ingress",
				Namespace: resourceNamespace,
			}, ingress)).To(Succeed())

			Expect(ingress.Labels["app"]).To(Equal("workspace"))
			Expect(ingress.Labels["workspace"]).To(Equal(resourceName))
			Expect(ingress.Labels["custom"]).To(Equal("my-value"))
		})
	})

	Context("When testing label helpers unit logic", func() {
		It("should properly construct workspace labels and selector labels", func() {
			ws := &aiv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-ws",
					Namespace: "default",
					Labels: map[string]string{
						"app":       "custom-app",
						"workspace": "override-name",
						"team":      "data",
					},
				},
			}

			selector := buildSelectorLabels(ws)
			Expect(selector).To(Equal(map[string]string{
				"app":       "workspace",
				"workspace": "my-ws",
			}))

			labels := buildWorkspaceLabels(ws)
			Expect(labels["app"]).To(Equal("workspace"))
			Expect(labels["workspace"]).To(Equal("my-ws"))
			Expect(labels["team"]).To(Equal("data"))

			// In bocomwork namespace, OA from env should be added if not present
			bocomWs := &aiv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "bocom-ws",
					Namespace: "bocomwork",
				},
				Spec: aiv1alpha1.WorkspaceSpec{
					Runtime: aiv1alpha1.RuntimeSpec{
						Env: []aiv1alpha1.EnvVar{
							{Name: "OA", Value: "zhangsan"},
						},
					},
				},
			}
			bocomLabels := buildWorkspaceLabels(bocomWs)
			Expect(bocomLabels["oa"]).To(Equal("zhangsan"))
			Expect(bocomLabels["app"]).To(Equal("workspace"))
			Expect(bocomLabels["workspace"]).To(Equal("bocom-ws"))
		})
	})

	Context("When reconciling storage changes", func() {
		const (
			storageWsName      = "test-storage-ws"
			storageWsNamespace = "default"
		)

		ctx := context.Background()
		storageKey := types.NamespacedName{
			Name:      storageWsName,
			Namespace: storageWsNamespace,
		}

		BeforeEach(func() {
			ws := &aiv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      storageWsName,
					Namespace: storageWsNamespace,
				},
				Spec: aiv1alpha1.WorkspaceSpec{
					Owner: "storage-owner",
					Runtime: aiv1alpha1.RuntimeSpec{
						Image: "nginx:alpine",
					},
					Storage: aiv1alpha1.StorageSpec{
						Size: "1Gi",
					},
				},
			}
			Expect(k8sClient.Create(ctx, ws)).To(Succeed())
		})

		AfterEach(func() {
			ws := &aiv1alpha1.Workspace{}
			if err := k8sClient.Get(ctx, storageKey, ws); err == nil {
				_ = k8sClient.Delete(ctx, ws)
			}
		})

		It("should expand PVC storage size when requested size is larger", func() {
			controllerReconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// First reconcile adds finalizer, second creates resources
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: storageKey})
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: storageKey})
			Expect(err).NotTo(HaveOccurred())

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: storageWsName + "-pvc", Namespace: storageWsNamespace}, pvc)).To(Succeed())
			Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("1Gi"))

			By("Updating Workspace spec.storage.size to 5Gi")
			ws := &aiv1alpha1.Workspace{}
			Expect(k8sClient.Get(ctx, storageKey, ws)).To(Succeed())
			ws.Spec.Storage.Size = "5Gi"
			Expect(k8sClient.Update(ctx, ws)).To(Succeed())

			By("Reconciling the expanded storage")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: storageKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: storageWsName + "-pvc", Namespace: storageWsNamespace}, pvc)).To(Succeed())
			Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("5Gi"))

			By("Attempting to shrink Workspace spec.storage.size to 2Gi")
			Expect(k8sClient.Get(ctx, storageKey, ws)).To(Succeed())
			ws.Spec.Storage.Size = "2Gi"
			Expect(k8sClient.Update(ctx, ws)).To(Succeed())

			By("Reconciling the shrunk storage request")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: storageKey})
			Expect(err).NotTo(HaveOccurred())

			// PVC storage should remain 5Gi (shrink is ignored/unsupported)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: storageWsName + "-pvc", Namespace: storageWsNamespace}, pvc)).To(Succeed())
			Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("5Gi"))
		})
	})
})
