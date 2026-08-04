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

// docBindings mirrors the ClusterBinding worked example in
// docs/design/architecture.md §2.1.
func docBindings() []kregv1alpha1.ClusterBinding {
	return []kregv1alpha1.ClusterBinding{
		{
			ClusterID:       "atl-1",
			AllowedPrefixes: []string{"198.51.100.0/26"},
			Locality:        kregv1alpha1.Locality{Region: "us-east", Zone: "us-east-atl-a"},
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
		routes := []pipeline.RIBRoute{{Prefix: "198.51.100.10/32", Peer: "rr-atl-a"}}

		authorized, err := authorize.Authorize(routes, docBindings())
		Expect(err).NotTo(HaveOccurred())
		Expect(authorized).To(HaveLen(1))
		Expect(authorized[0].ClusterID).To(Equal("atl-1"))
		Expect(authorized[0].Locality).To(Equal(pipeline.Locality{Region: "us-east", Zone: "us-east-atl-a"}))
		Expect(authorized[0].Peer).To(Equal("rr-atl-a")) // untouched fields survive
	})

	It("attributes adjacent bindings independently", func() {
		routes := []pipeline.RIBRoute{
			{Prefix: "198.51.100.10/32"}, // in atl-1's /26
			{Prefix: "198.51.100.70/32"}, // in atl-2's /26
		}

		authorized, err := authorize.Authorize(routes, docBindings())
		Expect(err).NotTo(HaveOccurred())
		Expect(authorized).To(HaveLen(2))
		Expect(authorized[0].ClusterID).To(Equal("atl-1"))
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

	It("ignores a peer-asserted ClusterID and re-derives it from prefix position", func() {
		// A route that (however it happened) already carries a ClusterID
		// from ingest must not be trusted — only allowedPrefixes decides.
		routes := []pipeline.RIBRoute{{
			Prefix:    "198.51.100.70/32", // actually atl-2's range
			ClusterID: "atl-1",            // asserted, wrong
		}}

		authorized, err := authorize.Authorize(routes, docBindings())
		Expect(err).NotTo(HaveOccurred())
		Expect(authorized[0].ClusterID).To(Equal("atl-2"))
	})

	It("rejects every route when there are no bindings at all", func() {
		routes := []pipeline.RIBRoute{{Prefix: "198.51.100.10/32"}}

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
			ClusterID:       "atl-1",
			AllowedPrefixes: []string{"not-a-prefix"},
			Locality:        kregv1alpha1.Locality{Region: "us-east", Zone: "us-east-atl-a"},
		}}
		routes := []pipeline.RIBRoute{{Prefix: "198.51.100.10/32"}}

		_, err := authorize.Authorize(routes, bindings)
		Expect(err).To(HaveOccurred())
	})
})
