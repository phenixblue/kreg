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

package report_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
	"github.com/phenixblue/kreg/internal/reconcile"
	"github.com/phenixblue/kreg/internal/report"
)

func docCandidate() pipeline.BackendCandidate {
	serviceTag := int32(80)
	return pipeline.BackendCandidate{
		Prefix:           "198.51.100.10/32",
		ClusterID:        "atl-1",
		Peer:             "rr-atl-a",
		Locality:         pipeline.Locality{Region: "us-east", Zone: "us-east-atl-a"},
		MED:              100,
		ASPath:           []uint32{4200000101},
		LargeCommunities: []string{"4200000000:1:80", "4200000000:4:80"},
		Weight:           80,
		Tier:             "stable",
		ServiceTag:       &serviceTag,
	}
}

func policyMatchingServiceTag80(name, namespace string) kregv1alpha1.BGPBackendPolicy {
	tag := int32(80)
	return kregv1alpha1.BGPBackendPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: kregv1alpha1.BGPBackendPolicySpec{
			Selector: kregv1alpha1.BackendSelector{
				Prefixes:   []string{"198.51.100.0/24"},
				ServiceTag: &tag,
			},
		},
	}
}

var _ = Describe("BuildAdvertisedBackends", func() {
	It("matches the doc's naming convention", func() {
		backends := report.BuildAdvertisedBackends([]pipeline.BackendCandidate{docCandidate()}, nil)
		Expect(backends).To(HaveLen(1))
		Expect(backends[0].Name).To(Equal("198-51-100-10-32-atl-1"))
	})

	It("labels every backend it builds as owned by this controller", func() {
		// AdvertisedBackendReconciler's prune step lists by this label so
		// it never deletes an object of this kind it didn't create.
		backends := report.BuildAdvertisedBackends([]pipeline.BackendCandidate{docCandidate()}, nil)
		Expect(backends[0].Labels).To(Equal(map[string]string{reconcile.ManagedByLabel: report.ManagedByValue}))
	})

	It("reports an authorized, non-draining candidate as Active", func() {
		backends := report.BuildAdvertisedBackends([]pipeline.BackendCandidate{docCandidate()}, nil)
		status := backends[0].Status
		Expect(status.State).To(Equal(kregv1alpha1.BackendStateActive))
		Expect(status.Reason).To(BeEmpty())
		Expect(status.Prefix).To(Equal("198.51.100.10/32"))
		Expect(status.ClusterID).To(Equal("atl-1"))
		Expect(status.Locality).To(Equal(kregv1alpha1.Locality{Region: "us-east", Zone: "us-east-atl-a"}))
		Expect(status.Attributes.Weight).To(Equal(int32(80)))
		Expect(status.Attributes.MED).To(Equal(int64(100)))
		Expect(status.Attributes.ASPath).To(Equal([]int64{4200000101}))
		Expect(status.Attributes.LargeCommunities).To(ConsistOf("4200000000:1:80", "4200000000:4:80"))
	})

	It("reports a draining candidate as Draining", func() {
		candidate := docCandidate()
		candidate.Drain = true

		backends := report.BuildAdvertisedBackends([]pipeline.BackendCandidate{candidate}, nil)
		Expect(backends[0].Status.State).To(Equal(kregv1alpha1.BackendStateDraining))
	})

	It("reports a Damp-decided state (e.g. Dampened) ahead of Drain, with Damp's own reason", func() {
		candidate := docCandidate()
		candidate.Drain = true // would be Draining if Damp hadn't already decided otherwise
		candidate.Damping = &pipeline.DampingInfo{
			State:  kregv1alpha1.BackendStateDampened,
			Reason: "flap-dampened: score 3400 (>= suppressThreshold 3000)",
		}

		backends := report.BuildAdvertisedBackends([]pipeline.BackendCandidate{candidate}, nil)
		Expect(backends[0].Status.State).To(Equal(kregv1alpha1.BackendStateDampened))
		Expect(backends[0].Status.Reason).To(Equal(candidate.Damping.Reason))
	})

	It("reports Rejected ahead of any Damp-decided state", func() {
		candidate := docCandidate()
		candidate.Rejected = true
		candidate.Reason = "prefix 198.51.100.10/32 not in allowedPrefixes for any cluster"
		candidate.Damping = &pipeline.DampingInfo{State: kregv1alpha1.BackendStateDampened, Reason: "should be shadowed"}

		backends := report.BuildAdvertisedBackends([]pipeline.BackendCandidate{candidate}, nil)
		Expect(backends[0].Status.State).To(Equal(kregv1alpha1.BackendStateRejected))
		Expect(backends[0].Status.Reason).To(Equal(candidate.Reason))
	})

	It("populates flapCount24h/dampeningPenalty/timestamps from Damping when set", func() {
		observedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		withdrawnAt := observedAt.Add(-5 * time.Second)
		candidate := docCandidate()
		candidate.Damping = &pipeline.DampingInfo{
			State:          kregv1alpha1.BackendStateHoldDown,
			Reason:         "withdrawn 5s ago, grace 30s",
			Score:          340.4,
			FlapCount24h:   2,
			LastObservedAt: observedAt,
			WithdrawnAt:    &withdrawnAt,
		}

		backends := report.BuildAdvertisedBackends([]pipeline.BackendCandidate{candidate}, nil)
		stability := backends[0].Status.Stability
		Expect(stability.FlapCount24h).To(Equal(int32(2)))
		Expect(stability.DampeningPenalty).To(Equal(int32(340))) // rounded from 340.4
		Expect(stability.LastObservedAt).To(gstruct.PointTo(Equal(metav1.NewTime(observedAt))))
		Expect(stability.WithdrawnAt).To(gstruct.PointTo(Equal(metav1.NewTime(withdrawnAt))))
		Expect(stability.SuppressedSince).To(BeNil())
		Expect(stability.PendingSince).To(BeNil())
	})

	It("leaves Damping-derived status fields zero when Damping is nil", func() {
		// The state Damp never evaluates a candidate at all -- e.g. an
		// Authorize-rejected route.
		candidate := docCandidate()

		backends := report.BuildAdvertisedBackends([]pipeline.BackendCandidate{candidate}, nil)
		stability := backends[0].Status.Stability
		Expect(stability.FlapCount24h).To(Equal(int32(0)))
		Expect(stability.DampeningPenalty).To(Equal(int32(0)))
		Expect(stability.LastObservedAt).To(BeNil())
		Expect(stability.WithdrawnAt).To(BeNil())
		Expect(stability.SuppressedSince).To(BeNil())
		Expect(stability.PendingSince).To(BeNil())
	})

	It("reports a rejected candidate as Rejected, with no bindings even if a policy would otherwise match", func() {
		candidate := docCandidate()
		candidate.Rejected = true
		candidate.Reason = "prefix 198.51.100.10/32 not in allowedPrefixes for any cluster"

		policies := []kregv1alpha1.BGPBackendPolicy{policyMatchingServiceTag80("prod-web", "gateways")}
		backends := report.BuildAdvertisedBackends([]pipeline.BackendCandidate{candidate}, policies)

		status := backends[0].Status
		Expect(status.State).To(Equal(kregv1alpha1.BackendStateRejected))
		Expect(status.Reason).To(Equal(candidate.Reason))
		Expect(status.BoundPolicies).To(BeEmpty())
		Expect(status.GeneratedResources).To(BeEmpty())
	})

	It("computes boundPolicies and generatedResources for a matching policy", func() {
		policies := []kregv1alpha1.BGPBackendPolicy{policyMatchingServiceTag80("prod-web", "gateways")}
		backends := report.BuildAdvertisedBackends([]pipeline.BackendCandidate{docCandidate()}, policies)

		status := backends[0].Status
		Expect(status.BoundPolicies).To(ConsistOf("gateways/prod-web"))
		Expect(status.GeneratedResources).To(ConsistOf("EndpointSlice/gateways/prod-web-kreg-atl-1-198-51-100-10-32"))
	})

	It("excludes a policy whose selector doesn't match", func() {
		otherTag := int32(81)
		policy := kregv1alpha1.BGPBackendPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "gateways"},
			Spec: kregv1alpha1.BGPBackendPolicySpec{
				Selector: kregv1alpha1.BackendSelector{ServiceTag: &otherTag},
			},
		}

		backends := report.BuildAdvertisedBackends([]pipeline.BackendCandidate{docCandidate()}, []kregv1alpha1.BGPBackendPolicy{policy})
		Expect(backends[0].Status.BoundPolicies).To(BeEmpty())
	})

	It("names an Authorize-rejected (unattributed) candidate with the unattributed sentinel", func() {
		candidate := pipeline.BackendCandidate{
			Prefix:   "203.0.113.5/32",
			Rejected: true,
			Reason:   "prefix 203.0.113.5/32 not in allowedPrefixes for any cluster",
		}

		backends := report.BuildAdvertisedBackends([]pipeline.BackendCandidate{candidate}, nil)
		Expect(backends[0].Name).To(Equal("203-0-113-5-32-unattributed"))
	})

	It("handles multiple candidates independently", func() {
		second := docCandidate()
		second.Prefix = "198.51.100.70/32"
		second.ClusterID = "atl-2"

		backends := report.BuildAdvertisedBackends([]pipeline.BackendCandidate{docCandidate(), second}, nil)
		Expect(backends).To(HaveLen(2))
		Expect(backends[0].Name).To(Equal("198-51-100-10-32-atl-1"))
		Expect(backends[1].Name).To(Equal("198-51-100-70-32-atl-2"))
	})
})
