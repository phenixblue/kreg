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

package pipeline

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
)

func strPtr(s string) *string { return &s }

// tierCanaryCommunity is the "2:1" (tier) large community docCommunityMap
// maps to tier=canary — reused verbatim across several routes below.
const tierCanaryCommunity = "4200000000:2:1"

// docCommunityMap mirrors the CommunityMap example in
// docs/design/architecture.md §2.2.
func docCommunityMap() *kregv1alpha1.CommunityMapSpec {
	return &kregv1alpha1.CommunityMapSpec{
		Rules: []kregv1alpha1.CommunityRule{
			{
				Match: kregv1alpha1.CommunityMatch{LargeCommunity: "4200000000:1:*"},
				Set:   kregv1alpha1.CommunityFieldSet{Field: kregv1alpha1.CommunityFieldWeight, FromCommunityValue: true},
			},
			{
				Match: kregv1alpha1.CommunityMatch{LargeCommunity: tierCanaryCommunity},
				Set:   kregv1alpha1.CommunityFieldSet{Field: kregv1alpha1.CommunityFieldTier, Value: strPtr("canary")},
			},
			{
				Match: kregv1alpha1.CommunityMatch{LargeCommunity: "4200000000:3:1"},
				Set:   kregv1alpha1.CommunityFieldSet{Field: kregv1alpha1.CommunityFieldDrain, Value: strPtr("true")},
			},
			{
				Match: kregv1alpha1.CommunityMatch{LargeCommunity: "4200000000:4:*"},
				Set:   kregv1alpha1.CommunityFieldSet{Field: kregv1alpha1.CommunityFieldServiceTag, FromCommunityValue: true},
			},
		},
		Fallbacks: &kregv1alpha1.CommunityFallbacks{
			WeightFrom:     kregv1alpha1.WeightFallbackFromMED,
			PreferenceFrom: kregv1alpha1.PreferenceFallbackFromASPathLength,
			DefaultWeight:  100,
		},
		OnUnmappedCommunity: kregv1alpha1.UnmappedCommunityIgnore,
	}
}

var _ = Describe("Normalize", func() {
	It("decodes weight, tier, drain, and serviceTag from a single route's communities", func() {
		routes := []RIBRoute{{
			Prefix:           "198.51.100.10/32",
			ClusterID:        "atl-1",
			Peer:             "rr-atl-a",
			MED:              100,
			LargeCommunities: []string{"4200000000:1:80", tierCanaryCommunity, "4200000000:3:1", "4200000000:4:80"},
		}}

		candidates, err := Normalize(routes, docCommunityMap())
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(1))

		c := candidates[0]
		Expect(c.Weight).To(Equal(int32(80)))
		Expect(c.Tier).To(Equal("canary"))
		Expect(c.Drain).To(BeTrue())
		Expect(c.ServiceTag).To(gstruct.PointTo(Equal(int32(80))))
		Expect(c.Rejected).To(BeFalse())
	})

	It("falls back to MED-derived weight, inverted, when no weight community is present", func() {
		routes := []RIBRoute{{
			Prefix: "198.51.100.11/32",
			MED:    100,
			// tier community present, but nothing in the "1:*" (weight) or
			// "4:*" (serviceTag) function codes.
			LargeCommunities: []string{tierCanaryCommunity},
		}}

		candidates, err := Normalize(routes, docCommunityMap())
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates[0].Weight).To(Equal(int32(900))) // 1000 - MED(100)
		Expect(candidates[0].Tier).To(Equal("canary"))
	})

	It("falls back to defaultWeight when there is no weight community and no MED fallback configured", func() {
		cm := &kregv1alpha1.CommunityMapSpec{
			Fallbacks:           &kregv1alpha1.CommunityFallbacks{DefaultWeight: 42},
			OnUnmappedCommunity: kregv1alpha1.UnmappedCommunityIgnore,
		}
		routes := []RIBRoute{{Prefix: "198.51.100.12/32"}}

		candidates, err := Normalize(routes, cm)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates[0].Weight).To(Equal(int32(42)))
	})

	It("defaults weight to 100 with no CommunityMap at all", func() {
		routes := []RIBRoute{{Prefix: "198.51.100.13/32"}}

		candidates, err := Normalize(routes, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates[0].Weight).To(Equal(int32(100)))
		Expect(candidates[0].Rejected).To(BeFalse())
	})

	Describe("onUnmappedCommunity", func() {
		routeWithNoMatchingCommunities := []RIBRoute{{
			Prefix:           "198.51.100.14/32",
			LargeCommunities: []string{"64512:99:1"}, // matches none of docCommunityMap's rules
		}}

		It("Ignore proceeds silently with fallback values", func() {
			cm := docCommunityMap()
			cm.OnUnmappedCommunity = kregv1alpha1.UnmappedCommunityIgnore

			candidates, err := Normalize(routeWithNoMatchingCommunities, cm)
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates[0].Rejected).To(BeFalse())
			Expect(candidates[0].Reason).To(BeEmpty())
		})

		It("Warn proceeds but records a reason", func() {
			cm := docCommunityMap()
			cm.OnUnmappedCommunity = kregv1alpha1.UnmappedCommunityWarn

			candidates, err := Normalize(routeWithNoMatchingCommunities, cm)
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates[0].Rejected).To(BeFalse())
			Expect(candidates[0].Reason).NotTo(BeEmpty())
		})

		It("Reject excludes the candidate and records why", func() {
			cm := docCommunityMap()
			cm.OnUnmappedCommunity = kregv1alpha1.UnmappedCommunityReject

			candidates, err := Normalize(routeWithNoMatchingCommunities, cm)
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates[0].Rejected).To(BeTrue())
			Expect(candidates[0].Reason).NotTo(BeEmpty())
		})

		It("does not fire when at least one rule matched, even if others didn't", func() {
			// Only the tier community is present; weight/drain/serviceTag
			// communities are absent, but tier alone is enough for the
			// route to count as understood.
			routes := []RIBRoute{{
				Prefix:           "198.51.100.15/32",
				LargeCommunities: []string{tierCanaryCommunity},
			}}
			cm := docCommunityMap()
			cm.OnUnmappedCommunity = kregv1alpha1.UnmappedCommunityReject

			candidates, err := Normalize(routes, cm)
			Expect(err).NotTo(HaveOccurred())
			Expect(candidates[0].Rejected).To(BeFalse())
		})
	})

	It("returns an error when a weight community's value segment isn't numeric", func() {
		routes := []RIBRoute{{
			Prefix:           "198.51.100.16/32",
			LargeCommunities: []string{"4200000000:1:not-a-number"},
		}}

		_, err := Normalize(routes, docCommunityMap())
		Expect(err).To(HaveOccurred())
	})

	It("normalizes multiple routes independently", func() {
		routes := []RIBRoute{
			{Prefix: "198.51.100.10/32", LargeCommunities: []string{"4200000000:1:80"}},
			{Prefix: "198.51.100.20/32", LargeCommunities: []string{"4200000000:1:20"}},
		}

		candidates, err := Normalize(routes, docCommunityMap())
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(2))
		Expect(candidates[0].Weight).To(Equal(int32(80)))
		Expect(candidates[1].Weight).To(Equal(int32(20)))
	})

	It("carries an Authorize-level rejection straight through without decoding communities", func() {
		routes := []RIBRoute{{
			Prefix:           "203.0.113.5/32",
			LargeCommunities: []string{"4200000000:1:not-a-number"}, // would error if decoded
			Rejected:         true,
			Reason:           "prefix 203.0.113.5/32 not in allowedPrefixes for any cluster",
		}}

		candidates, err := Normalize(routes, docCommunityMap())
		Expect(err).NotTo(HaveOccurred())
		Expect(candidates).To(HaveLen(1))
		Expect(candidates[0].Rejected).To(BeTrue())
		Expect(candidates[0].Reason).To(Equal("prefix 203.0.113.5/32 not in allowedPrefixes for any cluster"))
		Expect(candidates[0].Weight).To(Equal(int32(0))) // not decoded, not fallback-filled
	})
})
