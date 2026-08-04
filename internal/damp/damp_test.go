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

package damp_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/damp"
	"github.com/phenixblue/kreg/internal/pipeline"
)

var _ = Describe("PriorStateFromAdvertisedBackends", func() {
	It("reconstructs a full candidate and damping info from a record Damp has evaluated before", func() {
		serviceTag := int32(80)
		observedAt := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		withdrawnAt := metav1.NewTime(observedAt.Add(-5 * time.Second))

		items := []kregv1alpha1.AdvertisedBackend{{
			ObjectMeta: metav1.ObjectMeta{Name: "198-51-100-10-32-atl-1"},
			Status: kregv1alpha1.AdvertisedBackendStatus{
				Prefix:    "198.51.100.10/32",
				ClusterID: "atl-1",
				Peer:      "rr-atl-a",
				Locality:  kregv1alpha1.Locality{Region: "us-east", Zone: "us-east-atl-a"},
				Attributes: kregv1alpha1.BackendAttributes{
					Weight:           80,
					Tier:             "stable",
					Drain:            false,
					ServiceTag:       &serviceTag,
					MED:              100,
					ASPath:           []int64{4200000101},
					LargeCommunities: []string{"4200000000:1:80"},
				},
				State:  kregv1alpha1.BackendStateHoldDown,
				Reason: "withdrawn 5s ago, grace 30s",
				Stability: kregv1alpha1.StabilityStatus{
					FlapCount24h:     2,
					DampeningPenalty: 340,
					LastObservedAt:   &observedAt,
					WithdrawnAt:      &withdrawnAt,
				},
			},
		}}

		prior := damp.PriorStateFromAdvertisedBackends(items)
		Expect(prior).To(HaveKey("198-51-100-10-32-atl-1"))

		p := prior["198-51-100-10-32-atl-1"]
		Expect(p.Candidate).To(Equal(pipeline.BackendCandidate{
			Prefix:           "198.51.100.10/32",
			ClusterID:        "atl-1",
			Peer:             "rr-atl-a",
			Locality:         pipeline.Locality{Region: "us-east", Zone: "us-east-atl-a"},
			MED:              100,
			ASPath:           []uint32{4200000101},
			LargeCommunities: []string{"4200000000:1:80"},
			Weight:           80,
			Tier:             "stable",
			ServiceTag:       &serviceTag,
		}))
		Expect(p.Damping.State).To(Equal(kregv1alpha1.BackendStateHoldDown))
		Expect(p.Damping.Score).To(Equal(float64(340)))
		Expect(p.Damping.FlapCount24h).To(Equal(int32(2)))
		Expect(p.Damping.LastObservedAt).To(Equal(observedAt.Time))
		Expect(p.Damping.WithdrawnAt).To(gstruct.PointTo(Equal(withdrawnAt.Time)))
		Expect(p.Damping.SuppressedSince).To(BeNil())
		Expect(p.Damping.PendingSince).To(BeNil())
	})

	It("omits a record Damp has never evaluated (lastObservedAt unset)", func() {
		items := []kregv1alpha1.AdvertisedBackend{{
			ObjectMeta: metav1.ObjectMeta{Name: "203-0-113-5-32-unattributed"},
			Status: kregv1alpha1.AdvertisedBackendStatus{
				Prefix: "203.0.113.5/32",
				State:  kregv1alpha1.BackendStateRejected,
				Reason: "prefix 203.0.113.5/32 not in allowedPrefixes for any cluster",
			},
		}}

		prior := damp.PriorStateFromAdvertisedBackends(items)
		Expect(prior).To(BeEmpty())
	})
})
