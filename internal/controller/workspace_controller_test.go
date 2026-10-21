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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
				if controllerutil.ContainsFinalizer(resource, workspaceFinalizer) {
					controllerutil.RemoveFinalizer(resource, workspaceFinalizer)
					_ = k8sClient.Update(ctx, resource)
				}
				_ = k8sClient.Delete(ctx, resource)
			}

			// Task 2.1: the shared namespace-level NetworkPolicy template is not owned
			// by any Workspace, so remove it explicitly to isolate specs.
			sharedNetpol := &networkingv1.NetworkPolicy{}
			sharedKey := types.NamespacedName{Name: sharedNetpolName, Namespace: resourceNamespace}
			if err := k8sClient.Get(ctx, sharedKey, sharedNetpol); err == nil {
				_ = k8sClient.Delete(ctx, sharedNetpol)
			}
		})

		It("should reconcile a single shared namespace-level NetworkPolicy template", func() {
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

			By("Verifying Deployment container resources (50% request ratio)")
			Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(deploy.Spec.Template.Spec.Containers[0].Resources.Limits.Cpu().String()).To(Equal("500m"))
			Expect(deploy.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().String()).To(Equal("250m"))
			Expect(deploy.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String()).To(Equal("1Gi"))
			Expect(deploy.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().String()).To(Equal("512Mi"))

			sharedKey := types.NamespacedName{Name: sharedNetpolName, Namespace: resourceNamespace}
			legacyKey := types.NamespacedName{Name: resourceName + legacyNetpolSuffix, Namespace: resourceNamespace}

			By("Verifying the shared namespace-level NetworkPolicy template")
			netpol := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, sharedKey, netpol)).To(Succeed())
			Expect(netpol.Spec.PodSelector.MatchLabels).To(Equal(map[string]string{
				managedByLabelKey: managedByLabelValue,
			}))
			Expect(netpol.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeIngress))
			Expect(netpol.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))
			Expect(netpol.Spec.Ingress).To(HaveLen(1))
			Expect(netpol.Spec.Ingress[0].From).To(BeEmpty())
			Expect(netpol.Spec.Ingress[0].Ports).To(BeEmpty())
			Expect(len(netpol.Spec.Egress)).To(BeNumerically(">=", 2))
			Expect(netpol.Spec.Egress[1].To[0].IPBlock.Except).To(ConsistOf(
				"10.0.0.0/8",
				"100.64.0.0/10",
				"169.254.0.0/16",
				"172.16.0.0/12",
				"192.168.0.0/16",
			))
			// The shared template is namespace-level infrastructure: it must NOT be
			// owned by (and garbage collected with) a single Workspace.
			Expect(netpol.OwnerReferences).To(BeEmpty())

			By("Verifying no per-workspace NetworkPolicy is created anymore")
			err = k8sClient.Get(ctx, legacyKey, &networkingv1.NetworkPolicy{})
			Expect(errors.IsNotFound(err)).To(BeTrue())

			By("Verifying workspace pods carry the managed-by label")
			Expect(deploy.Spec.Template.Labels[managedByLabelKey]).To(Equal(managedByLabelValue))

			By("Verifying child resources stay clean of the pod-only managed-by label (P3-5)")
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      resourceName + "-pvc",
				Namespace: resourceNamespace,
			}, pvc)).To(Succeed())
			Expect(pvc.Labels).NotTo(HaveKey(managedByLabelKey))

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      resourceName + "-svc",
				Namespace: resourceNamespace,
			}, svc)).To(Succeed())
			Expect(svc.Labels).NotTo(HaveKey(managedByLabelKey))
			Expect(deploy.Labels).NotTo(HaveKey(managedByLabelKey))

			By("Verifying per-workspace NetworkPolicy customization is ignored by the shared template")
			resource := &aiv1alpha1.Workspace{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Spec.NetworkPolicy = &aiv1alpha1.WorkspaceNetworkPolicySpec{
				BlockedCIDRs: []string{"192.168.1.0/24", "10.0.0.0/16"},
				AllowedCIDRs: []string{"10.0.1.100/32"},
				AllowedNamespaces: []aiv1alpha1.WorkspaceNetworkPolicyNamespaceRule{
					{Namespace: "logging"},
				},
			}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// The shared template stays static: DNS + public Internet only, the
			// CR-level exceptions are deliberately not merged.
			Expect(k8sClient.Get(ctx, sharedKey, netpol)).To(Succeed())
			Expect(netpol.Spec.Egress).To(HaveLen(2))
			err = k8sClient.Get(ctx, legacyKey, &networkingv1.NetworkPolicy{})
			Expect(errors.IsNotFound(err)).To(BeTrue())

			By("Disabling NetworkPolicy and verifying the workspace opts out via labels while the shared template remains")
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Spec.NetworkPolicy.Disabled = true
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// The shared template must survive a per-workspace opt-out because other
			// workspaces in the namespace still rely on it.
			Expect(k8sClient.Get(ctx, sharedKey, netpol)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      resourceName + "-deploy",
				Namespace: resourceNamespace,
			}, deploy)).To(Succeed())
			_, hasManagedBy := deploy.Spec.Template.Labels[managedByLabelKey]
			Expect(hasManagedBy).To(BeFalse())
		})

		It("should apply controller-wide namespace egress exceptions to the shared template", func() {
			By("Reconciling the workspace with a controller-wide egress exception")
			controllerReconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				GlobalAllowedNamespaces: []aiv1alpha1.WorkspaceNetworkPolicyNamespaceRule{
					{
						Namespace:   "monitoring",
						PodSelector: map[string]string{"app.kubernetes.io/name": "otel-collector"},
						Ports:       []int32{4318},
					},
				},
			}

			// First reconcile adds finalizer, second reconcile creates child resources.
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			sharedKey := types.NamespacedName{Name: sharedNetpolName, Namespace: resourceNamespace}

			By("Verifying the global exception lands in the shared NetworkPolicy without any CR change")
			netpol := &networkingv1.NetworkPolicy{}
			Expect(k8sClient.Get(ctx, sharedKey, netpol)).To(Succeed())
			otelRule := findNamespaceEgressRule(netpol, "monitoring")
			Expect(otelRule).NotTo(BeNil())
			Expect(otelRule.Ports).To(HaveLen(1))
			Expect(*otelRule.Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))
			Expect(otelRule.Ports[0].Port.IntVal).To(Equal(int32(4318)))

			By("Verifying per-workspace namespace rules are no longer merged into the shared template")
			resource := &aiv1alpha1.Workspace{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Spec.NetworkPolicy = &aiv1alpha1.WorkspaceNetworkPolicySpec{
				AllowedNamespaces: []aiv1alpha1.WorkspaceNetworkPolicyNamespaceRule{
					{
						Namespace:   "monitoring",
						PodSelector: map[string]string{"app.kubernetes.io/name": "otel-collector"},
						Ports:       []int32{4318},
					},
					{
						Namespace: "logging",
					},
				},
			}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, sharedKey, netpol)).To(Succeed())
			// The controller-wide default stays exactly once...
			Expect(countNamespaceEgressRules(netpol, "monitoring")).To(Equal(1))
			// ...and the CR-only rule must NOT be merged into the shared template.
			Expect(findNamespaceEgressRule(netpol, "logging")).To(BeNil())
		})

		It("should clean up the legacy per-workspace NetworkPolicy on reconcile", func() {
			By("Pre-creating the legacy per-workspace NetworkPolicy from an older operator version")
			legacyName := resourceName + legacyNetpolSuffix
			legacyNetpol := &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      legacyName,
					Namespace: resourceNamespace,
					Labels:    map[string]string{"app": "workspace", "workspace": resourceName},
				},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "workspace", "workspace": resourceName},
					},
				},
			}
			Expect(k8sClient.Create(ctx, legacyNetpol)).To(Succeed())

			controllerReconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the legacy NetworkPolicy is deleted and only the shared template remains")
			err = k8sClient.Get(ctx, types.NamespacedName{Name: legacyName, Namespace: resourceNamespace}, &networkingv1.NetworkPolicy{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      sharedNetpolName,
				Namespace: resourceNamespace,
			}, &networkingv1.NetworkPolicy{})).To(Succeed())
		})

		// P1-3: a user configuring spec.networkPolicy.allowedCIDRs after Task 2.1 would
		// otherwise get no signal at all — only a controller-side log line.
		It("should emit a Warning event when per-workspace networkPolicy customization is ignored", func() {
			ws := &aiv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{Name: "netpol-event-ws", Namespace: resourceNamespace},
				Spec: aiv1alpha1.WorkspaceSpec{
					NetworkPolicy: &aiv1alpha1.WorkspaceNetworkPolicySpec{
						AllowedCIDRs: []string{"10.10.20.5/32"},
					},
				},
			}
			recorder := record.NewFakeRecorder(8)
			controllerReconciler := &WorkspaceReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: recorder,
			}

			By("Reconciling the network policy step for a workspace with deprecated per-workspace overrides")
			Expect(controllerReconciler.reconcileNetworkPolicy(ctx, ws)).To(Succeed())

			By("Verifying a Warning event with the override reason was emitted")
			var events []string
			for len(recorder.Events) > 0 {
				events = append(events, <-recorder.Events)
			}
			Expect(events).NotTo(BeEmpty())
			Expect(events).To(ContainElement(And(
				ContainSubstring("Warning"),
				ContainSubstring("NetworkPolicyOverridden"),
			)))
		})

		// Regression (Task 2.1): the controller no longer adds a finalizer, so a
		// Workspace delete is immediate and the delete-event reconcile only ever
		// sees NotFound. The garbage collection used to live exclusively in the
		// DeletionTimestamp branch, which made it unreachable — the shared template
		// then leaked in the namespace forever. This uses the real deletion path
		// (no hand-added finalizer, which is a state that does not exist in
		// production).
		It("should delete the shared NetworkPolicy after the last Workspace is deleted", func() {
			wsName := "gc-test-ws"
			// A dedicated namespace keeps the assertion honest: the shared
			// template is namespace-scoped, and the shared "default" namespace is
			// occupied by the fixture Workspace, so it must not be collected there.
			wsNamespace := "gc-namespace"
			wsKey := types.NamespacedName{Name: wsName, Namespace: wsNamespace}
			sharedKey := types.NamespacedName{Name: sharedNetpolName, Namespace: wsNamespace}

			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: wsNamespace}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, ns)
			})

			ws := &aiv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{Name: wsName, Namespace: wsNamespace},
				Spec: aiv1alpha1.WorkspaceSpec{
					Owner:   "user-gc",
					Runtime: aiv1alpha1.RuntimeSpec{Image: "ubuntu:latest"},
					Storage: aiv1alpha1.StorageSpec{Size: "1Gi"},
				},
			}
			Expect(k8sClient.Create(ctx, ws)).To(Succeed())
			DeferCleanup(func() {
				latest := &aiv1alpha1.Workspace{}
				if err := k8sClient.Get(ctx, wsKey, latest); err == nil {
					_ = k8sClient.Delete(ctx, latest)
				}
			})

			controllerReconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Two passes: the first converges the child resources, the second
			// creates the shared template once the Workspace is no longer "new".
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the shared template exists while the Workspace is alive")
			Expect(k8sClient.Get(ctx, sharedKey, &networkingv1.NetworkPolicy{})).To(Succeed())

			By("Deleting the Workspace through the real path (no finalizer)")
			latest := &aiv1alpha1.Workspace{}
			Expect(k8sClient.Get(ctx, wsKey, latest)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(latest, workspaceFinalizer)).To(BeFalse(),
				"the controller must not add a finalizer; the test would otherwise exercise a state that cannot occur in production")
			Expect(k8sClient.Delete(ctx, latest)).To(Succeed())

			// Deletion is immediate, so the reconcile only sees NotFound from here on.
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the shared template was garbage-collected")
			Eventually(func() bool {
				err := k8sClient.Get(ctx, sharedKey, &networkingv1.NetworkPolicy{})
				return errors.IsNotFound(err)
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue(),
				"the shared NetworkPolicy must be deleted once the last Workspace in the namespace is gone")
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

			// First reconcile adds finalizer, second reconcile creates/updates child resources
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
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
			// P3-5: managed-by belongs to Pods only; child resources stay clean.
			_, hasManagedBy := labels[managedByLabelKey]
			Expect(hasManagedBy).To(BeFalse())

			// Task 2.1: pods carry the managed-by label so the shared namespace-level
			// NetworkPolicy template selects them.
			podLabels := buildPodLabels(ws)
			Expect(podLabels[managedByLabelKey]).To(Equal(managedByLabelValue))
			Expect(podLabels["team"]).To(Equal("data"))

			// Workspaces with network isolation disabled opt out via label omission.
			disabledWs := &aiv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "disabled-ws",
					Namespace: "default",
				},
				Spec: aiv1alpha1.WorkspaceSpec{
					NetworkPolicy: &aiv1alpha1.WorkspaceNetworkPolicySpec{Disabled: true},
				},
			}
			disabledPodLabels := buildPodLabels(disabledWs)
			_, hasManagedBy = disabledPodLabels[managedByLabelKey]
			Expect(hasManagedBy).To(BeFalse())
			Expect(disabledPodLabels["app"]).To(Equal("workspace"))

			// A user-supplied managed-by label must not defeat the opt-out.
			disabledWithUserLabel := &aiv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "disabled-ws-user-label",
					Namespace: "default",
					Labels:    map[string]string{managedByLabelKey: managedByLabelValue},
				},
				Spec: aiv1alpha1.WorkspaceSpec{
					NetworkPolicy: &aiv1alpha1.WorkspaceNetworkPolicySpec{Disabled: true},
				},
			}
			_, hasManagedBy = buildPodLabels(disabledWithUserLabel)[managedByLabelKey]
			Expect(hasManagedBy).To(BeFalse())

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
			bocomPodLabels := buildPodLabels(bocomWs)
			Expect(bocomPodLabels["oa"]).To(Equal("zhangsan"))
			Expect(bocomPodLabels["app"]).To(Equal("workspace"))
			Expect(bocomPodLabels["workspace"]).To(Equal("bocom-ws"))
			Expect(bocomPodLabels[managedByLabelKey]).To(Equal(managedByLabelValue))
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
				if controllerutil.ContainsFinalizer(ws, workspaceFinalizer) {
					controllerutil.RemoveFinalizer(ws, workspaceFinalizer)
					_ = k8sClient.Update(ctx, ws)
				}
				_ = k8sClient.Delete(ctx, ws)
			}
		})

		It("should expand PVC storage size when requested size is larger", func() {
			allowExpansion := true
			sc := &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: "expandable-sc",
				},
				Provisioner:          "kubernetes.io/no-provisioner",
				AllowVolumeExpansion: &allowExpansion,
			}
			_ = k8sClient.Create(ctx, sc)

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

			// Mark PVC as dynamically provisioned and bound so K8s admission allows expanding requests.storage
			scName := "expandable-sc"
			pvc.Spec.StorageClassName = &scName
			if pvc.Annotations == nil {
				pvc.Annotations = make(map[string]string)
			}
			pvc.Annotations["volume.beta.kubernetes.io/storage-provisioner"] = "kubernetes.io/no-provisioner"
			pvc.Annotations["volume.kubernetes.io/storage-provisioner"] = "kubernetes.io/no-provisioner"
			Expect(k8sClient.Update(ctx, pvc)).To(Succeed())

			pvc.Status.Phase = corev1.ClaimBound
			Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())

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

	Context("When computing container resource requirements", func() {
		It("should set limit equal to specified CPU/Memory and request equal to 50% limit", func() {
			// Test case: cpu: "1", memory: "2" (plain int for memory, should be treated as 2Gi)
			res1 := buildResourceRequirements("1", "2")
			Expect(res1.Limits.Cpu().String()).To(Equal("1"))
			Expect(res1.Requests.Cpu().String()).To(Equal("500m"))
			Expect(res1.Limits.Memory().String()).To(Equal("2Gi"))
			Expect(res1.Requests.Memory().String()).To(Equal("1Gi"))

			// Test case: cpu: "1", memory: "2Gi" (explicit unit)
			res2 := buildResourceRequirements("1", "2Gi")
			Expect(res2.Limits.Cpu().String()).To(Equal("1"))
			Expect(res2.Requests.Cpu().String()).To(Equal("500m"))
			Expect(res2.Limits.Memory().String()).To(Equal("2Gi"))
			Expect(res2.Requests.Memory().String()).To(Equal("1Gi"))

			// Test case: cpu: "2", memory: "4Gi"
			res3 := buildResourceRequirements("2", "4Gi")
			Expect(res3.Limits.Cpu().String()).To(Equal("2"))
			Expect(res3.Requests.Cpu().String()).To(Equal("1"))
			Expect(res3.Limits.Memory().String()).To(Equal("4Gi"))
			Expect(res3.Requests.Memory().String()).To(Equal("2Gi"))

			// Test case: cpu: "500m", memory: "1Gi"
			res4 := buildResourceRequirements("500m", "1Gi")
			Expect(res4.Limits.Cpu().String()).To(Equal("500m"))
			Expect(res4.Requests.Cpu().String()).To(Equal("250m"))
			Expect(res4.Limits.Memory().String()).To(Equal("1Gi"))
			Expect(res4.Requests.Memory().String()).To(Equal("512Mi"))

			// Test case: empty (defaults: 500m / 1Gi)
			resDef := buildResourceRequirements("", "")
			Expect(resDef.Limits.Cpu().String()).To(Equal("500m"))
			Expect(resDef.Requests.Cpu().String()).To(Equal("250m"))
			Expect(resDef.Limits.Memory().String()).To(Equal("1Gi"))
			Expect(resDef.Requests.Memory().String()).To(Equal("512Mi"))

			// Test case: memory: "512Mi"
			resMi := buildResourceRequirements("1", "512Mi")
			Expect(resMi.Limits.Cpu().String()).To(Equal("1"))
			Expect(resMi.Requests.Cpu().String()).To(Equal("500m"))
			Expect(resMi.Limits.Memory().String()).To(Equal("512Mi"))
			Expect(resMi.Requests.Memory().String()).To(Equal("256Mi"))
		})

		It("should not set OwnerReference on PVC and should retain PVC when workspace is deleted", func() {
			wsName := "persist-test-ws"
			wsNamespace := "default"
			wsKey := types.NamespacedName{Name: wsName, Namespace: wsNamespace}

			ws := &aiv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      wsName,
					Namespace: wsNamespace,
				},
				Spec: aiv1alpha1.WorkspaceSpec{
					Owner: "user-persist",
					Runtime: aiv1alpha1.RuntimeSpec{
						Image: "ubuntu:latest",
					},
					Storage: aiv1alpha1.StorageSpec{
						Size: "2Gi",
					},
				},
			}
			Expect(k8sClient.Create(ctx, ws)).To(Succeed())

			controllerReconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Reconcile workspace
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			// 1. Verify that no finalizer was added to Workspace
			latestWs := &aiv1alpha1.Workspace{}
			Expect(k8sClient.Get(ctx, wsKey, latestWs)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(latestWs, workspaceFinalizer)).To(BeFalse())

			// 2. Verify that PVC is created and has NO OwnerReference to Workspace
			pvcName := wsName + "-pvc"
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: wsNamespace}, pvc)).To(Succeed())
			Expect(pvc.OwnerReferences).To(BeEmpty())
			Expect(pvc.Labels["workspace"]).To(Equal(wsName))

			// 3. Test legacy ownerReference stripping on existing PVC
			pvc.OwnerReferences = []metav1.OwnerReference{
				{
					APIVersion: "ai.example.com/v1alpha1",
					Kind:       "Workspace",
					Name:       wsName,
					UID:        latestWs.UID,
				},
			}
			Expect(k8sClient.Update(ctx, pvc)).To(Succeed())

			// Reconcile again, should strip legacy owner reference
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: wsNamespace}, pvc)).To(Succeed())
			Expect(pvc.OwnerReferences).To(BeEmpty())

			// 4. Test legacy finalizer handling on Workspace deletion.
			// The finalizer is added by hand on purpose: this reproduces the
			// upgrade state where an older operator version already stamped it
			// and the current version must strip it. This is NOT the normal
			// deletion path — for that (and for shared NetworkPolicy garbage
			// collection) see "should delete the shared NetworkPolicy after the
			// last Workspace is deleted".
			controllerutil.AddFinalizer(latestWs, workspaceFinalizer)
			Expect(k8sClient.Update(ctx, latestWs)).To(Succeed())

			// Delete Workspace
			Expect(k8sClient.Delete(ctx, latestWs)).To(Succeed())

			// Reconcile deletion
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			// Workspace should be deleted (finalizer was removed by reconciler)
			err = k8sClient.Get(ctx, wsKey, latestWs)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			// 5. Verify PVC still exists and was NOT deleted
			retainedPVC := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: wsNamespace}, retainedPVC)).To(Succeed())

			// Cleanup the PVC
			Expect(k8sClient.Delete(ctx, retainedPVC)).To(Succeed())
		})
	})
})

// findNamespaceEgressRule returns the first egress rule targeting the given namespace
// through a kubernetes.io/metadata.name namespaceSelector, or nil when absent.
func findNamespaceEgressRule(netpol *networkingv1.NetworkPolicy, namespace string) *networkingv1.NetworkPolicyEgressRule {
	for i := range netpol.Spec.Egress {
		for _, peer := range netpol.Spec.Egress[i].To {
			if peer.NamespaceSelector != nil &&
				peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == namespace {
				return &netpol.Spec.Egress[i]
			}
		}
	}
	return nil
}

// countNamespaceEgressRules counts egress rules targeting the given namespace.
func countNamespaceEgressRules(netpol *networkingv1.NetworkPolicy, namespace string) int {
	count := 0
	for _, rule := range netpol.Spec.Egress {
		for _, peer := range rule.To {
			if peer.NamespaceSelector != nil &&
				peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == namespace {
				count++
			}
		}
	}
	return count
}
