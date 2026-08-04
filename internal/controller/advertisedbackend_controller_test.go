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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
)

const (
	testAtl1Address = "198.51.100.10/32"
	testAtl1        = "atl-1"
)

var _ = Describe("AdvertisedBackend Controller", func() {
	const policyName = "adv-test-policy"
	const policyNamespace = "default"

	ctx := context.Background()

	AfterEach(func() {
		policy := &kregv1alpha1.BGPBackendPolicy{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: policyName, Namespace: policyNamespace}, policy); err == nil {
			Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
		}

		var backends kregv1alpha1.AdvertisedBackendList
		Expect(k8sClient.List(ctx, &backends)).To(Succeed())
		for i := range backends.Items {
			Expect(k8sClient.Delete(ctx, &backends.Items[i])).To(Succeed())
		}
	})

	It("reports Active/Rejected state, boundPolicies, and generatedResources", func() {
		appProtocol := "https"
		serviceTag := int32(80)
		policy := &kregv1alpha1.BGPBackendPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: policyName, Namespace: policyNamespace},
			Spec: kregv1alpha1.BGPBackendPolicySpec{
				TargetRef: gatewayv1.LocalPolicyTargetReference{
					Group: "gateway.networking.k8s.io",
					Kind:  "Gateway",
					Name:  policyName,
				},
				Selector: kregv1alpha1.BackendSelector{
					Prefixes:   []string{"198.51.100.0/24"},
					ServiceTag: &serviceTag,
				},
				Backend: kregv1alpha1.BackendConfig{
					Port:        8443,
					AppProtocol: &appProtocol,
					TLS: kregv1alpha1.BackendTLSConfig{
						Mode: kregv1alpha1.BackendTLSModeSimple,
						SNI:  "adv-test.internal",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())

		active := pipeline.BackendCandidate{
			Prefix:     testAtl1Address,
			ClusterID:  testAtl1,
			ServiceTag: &serviceTag,
		}
		rejected := pipeline.BackendCandidate{
			Prefix:   "203.0.113.5/32",
			Rejected: true,
			Reason:   "prefix 203.0.113.5/32 not in allowedPrefixes for any cluster",
		}

		r := &AdvertisedBackendReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Snapshot: fakeSnapshotSource{candidates: []pipeline.BackendCandidate{active, rejected}},
		}
		_, err := r.Reconcile(ctx, reconcile.Request{})
		Expect(err).NotTo(HaveOccurred())

		var activeBackend kregv1alpha1.AdvertisedBackend
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "198-51-100-10-32-atl-1"}, &activeBackend)).To(Succeed())
		Expect(activeBackend.Status.State).To(Equal(kregv1alpha1.BackendStateActive))
		Expect(activeBackend.Status.BoundPolicies).To(ConsistOf(policyNamespace + "/" + policyName))
		Expect(activeBackend.Status.GeneratedResources).To(ConsistOf("EndpointSlice/" + policyNamespace + "/" + policyName + "-kreg-atl-1-198-51-100-10-32"))
		Expect(activeBackend.Status.FirstSeen).NotTo(BeNil())
		firstSeen := activeBackend.Status.FirstSeen

		var rejectedBackend kregv1alpha1.AdvertisedBackend
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "203-0-113-5-32-unattributed"}, &rejectedBackend)).To(Succeed())
		Expect(rejectedBackend.Status.State).To(Equal(kregv1alpha1.BackendStateRejected))
		Expect(rejectedBackend.Status.Reason).To(Equal(rejected.Reason))
		Expect(rejectedBackend.Status.BoundPolicies).To(BeEmpty())

		By("reconciling again with the same candidates: FirstSeen is preserved, no duplicate objects")
		_, err = r.Reconcile(ctx, reconcile.Request{})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "198-51-100-10-32-atl-1"}, &activeBackend)).To(Succeed())
		Expect(activeBackend.Status.FirstSeen.Time).To(Equal(firstSeen.Time))

		By("reconciling with the rejected route withdrawn entirely: its record is pruned")
		r.Snapshot = fakeSnapshotSource{candidates: []pipeline.BackendCandidate{active}}
		_, err = r.Reconcile(ctx, reconcile.Request{})
		Expect(err).NotTo(HaveOccurred())
		err = k8sClient.Get(ctx, types.NamespacedName{Name: "203-0-113-5-32-unattributed"}, &rejectedBackend)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
})
