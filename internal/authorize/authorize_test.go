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

package authorize_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/authorize"
	"github.com/phenixblue/kreg/internal/pipeline"
)

const (
	atl1         = "atl-1"
	usEast       = "us-east"
	usEastAtlA   = "us-east-atl-a"
	rrAtlA       = "rr-atl-a"
	atl1Address  = "198.51.100.10/32"
	atl1Address2 = "198.51.100.11/32"
	atl1Address3 = "198.51.100.12/32"
	atl2Address  = "198.51.100.70/32"
)

// docBindings mirrors the ClusterBinding worked example in
// docs/design/architecture.md §2.1.
func docBindings() []kregv1alpha1.ClusterBinding {
	return []kregv1alpha1.ClusterBinding{
		{
			ClusterID:       atl1,
			AllowedPrefixes: []string{"198.51.100.0/26"},
			Locality:        kregv1alpha1.Locality{Region: usEast, Zone: usEastAtlA},
		},
		{
			ClusterID:       "atl-2",
			AllowedPrefixes: []string{"198.51.100.64/26"},
			Locality:        kregv1alpha1.Locality{Region: "us-east", Zone: "us-east-atl-b"},
		},
	}
}

var _ = Describe("Authorize", func() {
	It("attributes a route to the binding whose allowedPrefixes contains it", func() {
		routes := []pipeline.RIBRoute{{Prefix: atl1Address, Peer: rrAtlA}}

		authorized, err := authorize.Authorize(routes, docBindings())
		Expect(err).NotTo(HaveOccurred())
		Expect(authorized).To(HaveLen(1))
		Expect(authorized[0].ClusterID).To(Equal(atl1))
		Expect(authorized[0].Locality).To(Equal(pipeline.Locality{Region: usEast, Zone: usEastAtlA}))
		Expect(authorized[0].Peer).To(Equal(rrAtlA)) // untouched fields survive
	})

	It("attributes adjacent bindings independently", func() {
		routes := []pipeline.RIBRoute{
			{Prefix: atl1Address}, // in atl-1's /26
			{Prefix: atl2Address}, // in atl-2's /26
		}

		authorized, err := authorize.Authorize(routes, docBindings())
		Expect(err).NotTo(HaveOccurred())
		Expect(authorized).To(HaveLen(2))
		Expect(authorized[0].ClusterID).To(Equal(atl1))
		Expect(authorized[1].ClusterID).To(Equal("atl-2"))
	})

	It("keeps a route matching no binding, flagged Rejected rather than attributed", func() {
		routes := []pipeline.RIBRoute{{Prefix: "203.0.113.5/32"}}

		authorized, err := authorize.Authorize(routes, docBindings())
		Expect(err).NotTo(HaveOccurred())
		Expect(authorized).To(HaveLen(1))
		Expect(authorized[0].Rejected).To(BeTrue())
		Expect(authorized[0].Reason).NotTo(BeEmpty())
		Expect(authorized[0].ClusterID).To(BeEmpty())
	})

	It("clears a peer-asserted ClusterID and Locality on a rejected route", func() {
		// A rejected route must not leak untrusted attribution downstream
		// into AdvertisedBackend reporting (e.g. a stale ClusterID
		// producing a misleading object name instead of "unattributed").
		routes := []pipeline.RIBRoute{{
			Prefix:    "203.0.113.5/32",
			ClusterID: atl1,
			Locality:  pipeline.Locality{Region: usEast, Zone: usEastAtlA},
		}}

		authorized, err := authorize.Authorize(routes, docBindings())
		Expect(err).NotTo(HaveOccurred())
		Expect(authorized[0].Rejected).To(BeTrue())
		Expect(authorized[0].ClusterID).To(BeEmpty())
		Expect(authorized[0].Locality).To(Equal(pipeline.Locality{}))
	})

	It("ignores a peer-asserted ClusterID and re-derives it from prefix position", func() {
		// A route that (however it happened) already carries a ClusterID
		// from ingest must not be trusted — only allowedPrefixes decides.
		routes := []pipeline.RIBRoute{{
			Prefix:    atl2Address, // actually atl-2's range
			ClusterID: atl1,        // asserted, wrong
		}}

		authorized, err := authorize.Authorize(routes, docBindings())
		Expect(err).NotTo(HaveOccurred())
		Expect(authorized[0].ClusterID).To(Equal("atl-2"))
	})

	It("rejects every route when there are no bindings at all", func() {
		routes := []pipeline.RIBRoute{{Prefix: atl1Address}}

		authorized, err := authorize.Authorize(routes, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(authorized).To(HaveLen(1))
		Expect(authorized[0].Rejected).To(BeTrue())
	})

	It("returns an error for a route with an unparseable prefix", func() {
		routes := []pipeline.RIBRoute{{Prefix: "not-a-prefix"}}

		_, err := authorize.Authorize(routes, docBindings())
		Expect(err).To(HaveOccurred())
	})

	It("returns an error for a binding with an unparseable allowedPrefixes entry", func() {
		bindings := []kregv1alpha1.ClusterBinding{{
			ClusterID:       atl1,
			AllowedPrefixes: []string{"not-a-prefix"},
			Locality:        kregv1alpha1.Locality{Region: usEast, Zone: usEastAtlA},
		}}
		routes := []pipeline.RIBRoute{{Prefix: atl1Address}}

		_, err := authorize.Authorize(routes, bindings)
		Expect(err).To(HaveOccurred())
	})

	Describe("maxPrefixes", func() {
		maxPrefixesBindings := func(max int32) []kregv1alpha1.ClusterBinding {
			bindings := docBindings()
			bindings[0].MaxPrefixes = &max
			return bindings
		}

		It("leaves a binding unlimited when maxPrefixes is unset", func() {
			routes := []pipeline.RIBRoute{
				{Prefix: atl1Address},
				{Prefix: atl1Address2},
				{Prefix: atl1Address3},
			}

			authorized, err := authorize.Authorize(routes, docBindings())
			Expect(err).NotTo(HaveOccurred())
			for _, r := range authorized {
				Expect(r.Rejected).To(BeFalse())
			}
		})

		It("keeps every route when the binding is exactly at its maxPrefixes cap", func() {
			routes := []pipeline.RIBRoute{
				{Prefix: atl1Address},
				{Prefix: atl1Address2},
			}

			authorized, err := authorize.Authorize(routes, maxPrefixesBindings(2))
			Expect(err).NotTo(HaveOccurred())
			for _, r := range authorized {
				Expect(r.Rejected).To(BeFalse())
			}
		})

		It("fails closed on the excess routes, deterministically, without touching the session", func() {
			// Deliberately out of prefix order, to prove selection is by
			// sorted Prefix, not RIB iteration order.
			routes := []pipeline.RIBRoute{
				{Prefix: atl1Address3, Peer: rrAtlA},
				{Prefix: atl1Address, Peer: rrAtlA},
				{Prefix: atl1Address2, Peer: rrAtlA},
			}

			authorized, err := authorize.Authorize(routes, maxPrefixesBindings(2))
			Expect(err).NotTo(HaveOccurred())
			Expect(authorized).To(HaveLen(3))

			byPrefix := map[string]pipeline.RIBRoute{}
			for _, r := range authorized {
				byPrefix[r.Prefix] = r
			}

			Expect(byPrefix[atl1Address].Rejected).To(BeFalse())
			Expect(byPrefix[atl1Address2].Rejected).To(BeFalse())

			excess := byPrefix[atl1Address3]
			Expect(excess.Rejected).To(BeTrue())
			Expect(excess.Reason).NotTo(BeEmpty())
			// Unlike an allowedPrefixes miss, attribution is known and
			// legitimate here — only capacity is exceeded — so it stays
			// visible via AdvertisedBackend rather than being cleared.
			Expect(excess.ClusterID).To(Equal(atl1))
			Expect(excess.Locality).To(Equal(pipeline.Locality{Region: usEast, Zone: usEastAtlA}))
			// The peer session itself is untouched by this rejection.
			Expect(excess.Peer).To(Equal(rrAtlA))
		})

		It("caps each binding independently", func() {
			routes := []pipeline.RIBRoute{
				{Prefix: atl1Address},  // atl-1
				{Prefix: atl1Address2}, // atl-1, over cap
				{Prefix: atl2Address},  // atl-2, uncapped
			}

			authorized, err := authorize.Authorize(routes, maxPrefixesBindings(1))
			Expect(err).NotTo(HaveOccurred())

			byPrefix := map[string]pipeline.RIBRoute{}
			for _, r := range authorized {
				byPrefix[r.Prefix] = r
			}
			Expect(byPrefix[atl1Address].Rejected).To(BeFalse())
			Expect(byPrefix[atl1Address2].Rejected).To(BeTrue())
			Expect(byPrefix[atl2Address].Rejected).To(BeFalse())
		})
	})
})
