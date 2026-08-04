/*
Copyright 2026 phenixblue.

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
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
	kregreconcile "github.com/phenixblue/kreg/internal/reconcile"
)

// fakeSnapshotSource returns a fixed snapshot, standing in for the real
// GoBGP-backed source that build-order step 2 adds.
type fakeSnapshotSource struct {
	candidates []pipeline.BackendCandidate
}

func (f fakeSnapshotSource) Snapshot(context.Context) ([]pipeline.BackendCandidate, error) {
	return f.candidates, nil
}

// noopDriver never produces a traffic-policy object. Used here instead of
// the real Istio driver so this test doesn't depend on Istio's
// DestinationRule CRD being installed in envtest — the Istio driver's own
// output is already covered by golden-file tests in internal/reconcile
// that need no cluster at all.
type noopDriver struct{}

func (noopDriver) Lower(*kregv1alpha1.BGPBackendPolicy, []pipeline.BackendCandidate, string) ([]client.Object, error) {
	return nil, nil
}

var _ = Describe("BGPBackendPolicy Controller", func() {
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
		bgpbackendpolicy := &kregv1alpha1.BGPBackendPolicy{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind BGPBackendPolicy")
			err := k8sClient.Get(ctx, typeNamespacedName, bgpbackendpolicy)
			if err != nil && errors.IsNotFound(err) {
				appProtocol := "https"
				resource := &kregv1alpha1.BGPBackendPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: kregv1alpha1.BGPBackendPolicySpec{
						TargetRef: gatewayv1.LocalPolicyTargetReference{
							Group: "gateway.networking.k8s.io",
							Kind:  "Gateway",
							Name:  "test-resource",
						},
						Selector: kregv1alpha1.BackendSelector{
							Prefixes: []string{"198.51.100.0/24"},
						},
						Backend: kregv1alpha1.BackendConfig{
							Port:        8443,
							AppProtocol: &appProtocol,
							TLS: kregv1alpha1.BackendTLSConfig{
								Mode: kregv1alpha1.BackendTLSModeSimple,
								SNI:  "test-resource.internal",
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &kregv1alpha1.BGPBackendPolicy{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())

			By("Cleanup the specific resource instance BGPBackendPolicy")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			svc := &corev1.Service{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-resource-kreg", Namespace: resourceNamespace}, svc); err == nil {
				Expect(k8sClient.Delete(ctx, svc)).To(Succeed())
			}

			Expect(k8sClient.DeleteAllOf(ctx, &discoveryv1.EndpointSlice{},
				client.InNamespace(resourceNamespace),
				client.MatchingLabels{kregreconcile.ManagedByLabel: resourceName},
			)).To(Succeed())
		})

		It("renders and applies the portable Service for the selected backends", func() {
			By("Reconciling the created resource")
			controllerReconciler := &BGPBackendPolicyReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Snapshot: fakeSnapshotSource{},
				Driver:   noopDriver{},
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("creating the portable Service")
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-resource-kreg", Namespace: resourceNamespace}, svc)).To(Succeed())
			Expect(svc.Labels[kregreconcile.ManagedByLabel]).To(Equal(resourceName))

			By("recording what it generated in status")
			var updated kregv1alpha1.BGPBackendPolicy
			Expect(k8sClient.Get(ctx, typeNamespacedName, &updated)).To(Succeed())
			Expect(updated.Status.Generated).To(ContainElement("Service/default/test-resource-kreg"))
		})

		It("prunes a stale EndpointSlice when a previously-selected candidate is withdrawn", func() {
			controllerReconciler := &BGPBackendPolicyReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				Snapshot: fakeSnapshotSource{candidates: []pipeline.BackendCandidate{
					{Prefix: "198.51.100.10/32", ClusterID: "atl-1"},
					{Prefix: "198.51.100.20/32", ClusterID: "atl-2"},
				}},
				Driver: noopDriver{},
			}

			By("reconciling with two candidates")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			listOpts := []client.ListOption{
				client.InNamespace(resourceNamespace),
				client.MatchingLabels{kregreconcile.ManagedByLabel: resourceName},
			}
			var slices discoveryv1.EndpointSliceList
			Expect(k8sClient.List(ctx, &slices, listOpts...)).To(Succeed())
			Expect(slices.Items).To(HaveLen(2))

			By("reconciling again with atl-2 withdrawn")
			controllerReconciler.Snapshot = fakeSnapshotSource{candidates: []pipeline.BackendCandidate{
				{Prefix: "198.51.100.10/32", ClusterID: "atl-1"},
			}}
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("pruning the withdrawn candidate's EndpointSlice")
			Expect(k8sClient.List(ctx, &slices, listOpts...)).To(Succeed())
			Expect(slices.Items).To(HaveLen(1))
			Expect(slices.Items[0].Name).To(Equal("test-resource-kreg-atl-1"))
		})
	})
})
