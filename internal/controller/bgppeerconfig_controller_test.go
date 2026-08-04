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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
)

// fakePeerManager stands in for a real *ingest.Manager (which needs a
// live GoBGP server) so this test exercises the reconciler's
// orchestration — fetch spec, reconfigure, write status — without
// needing a real BGP session. Real GoBGP wire-level behavior is covered
// separately by internal/ingest's loopback tests.
type fakePeerManager struct {
	reconfigureSpec *kregv1alpha1.BGPPeerConfigSpec
	statuses        []kregv1alpha1.PeerStatus
}

func (f *fakePeerManager) Reconfigure(_ context.Context, spec *kregv1alpha1.BGPPeerConfigSpec) error {
	f.reconfigureSpec = spec
	return nil
}

func (f *fakePeerManager) Status(context.Context) ([]kregv1alpha1.PeerStatus, error) {
	return f.statuses, nil
}

var _ = Describe("BGPPeerConfig Controller", func() {
	Context("When reconciling a resource", func() {
		// BGPPeerConfig is cluster-scoped: no namespace.
		const resourceName = "test-resource"

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName}
		bgppeerconfig := &kregv1alpha1.BGPPeerConfig{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind BGPPeerConfig")
			err := k8sClient.Get(ctx, typeNamespacedName, bgppeerconfig)
			if err != nil && errors.IsNotFound(err) {
				resource := &kregv1alpha1.BGPPeerConfig{
					ObjectMeta: metav1.ObjectMeta{Name: resourceName},
					Spec: kregv1alpha1.BGPPeerConfigSpec{
						LocalASN: 4200000000,
						RouterID: "10.0.0.1",
						Peers: []kregv1alpha1.BGPPeer{{
							Name:      "rr-atl-a",
							Address:   "10.0.10.1",
							RemoteASN: 4200000000,
						}},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &kregv1alpha1.BGPPeerConfig{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())

			By("Cleanup the specific resource instance BGPPeerConfig")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("reconfigures the peer manager from spec and records status", func() {
			By("Reconciling the created resource")
			manager := &fakePeerManager{
				statuses: []kregv1alpha1.PeerStatus{{
					Name:         "10.0.10.1",
					SessionState: kregv1alpha1.PeerSessionStateEstablished,
				}},
			}
			controllerReconciler := &BGPPeerConfigReconciler{
				Client:  k8sClient,
				Scheme:  k8sClient.Scheme(),
				Manager: manager,
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("passing the resource's spec to the peer manager")
			Expect(manager.reconfigureSpec).NotTo(BeNil())
			Expect(manager.reconfigureSpec.Peers).To(HaveLen(1))
			Expect(manager.reconfigureSpec.Peers[0].Address).To(Equal("10.0.10.1"))

			By("recording the peer manager's status")
			var updated kregv1alpha1.BGPPeerConfig
			Expect(k8sClient.Get(ctx, typeNamespacedName, &updated)).To(Succeed())
			Expect(updated.Status.Peers).To(HaveLen(1))
			Expect(updated.Status.Peers[0].SessionState).To(Equal(kregv1alpha1.PeerSessionStateEstablished))
		})
	})
})
