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

	"github.com/phenixblue/kreg/internal/authorize"
	"github.com/phenixblue/kreg/internal/pipeline"
)

var _ = Describe("RejectedCountsByPeer", func() {
	It("counts only Rejected routes, grouped by peer", func() {
		routes := []pipeline.RIBRoute{
			{Peer: rrAtlA, Rejected: true},
			{Peer: rrAtlA, Rejected: true},
			{Peer: rrAtlA, Rejected: false},
			{Peer: "rr-atl-b", Rejected: true},
		}

		counts := authorize.RejectedCountsByPeer(routes)
		Expect(counts).To(HaveKeyWithValue(rrAtlA, int32(2)))
		Expect(counts).To(HaveKeyWithValue("rr-atl-b", int32(1)))
	})

	It("returns an empty map when nothing is rejected", func() {
		routes := []pipeline.RIBRoute{{Peer: rrAtlA, Rejected: false}}

		Expect(authorize.RejectedCountsByPeer(routes)).To(BeEmpty())
	})
})

var _ = Describe("RejectionTracker", func() {
	It("returns 0 for a peer with no recorded counts", func() {
		tracker := &authorize.RejectionTracker{}
		Expect(tracker.Get(rrAtlA)).To(Equal(int32(0)))
	})

	It("returns the most recently Set count for a peer", func() {
		tracker := &authorize.RejectionTracker{}
		tracker.Set(map[string]int32{rrAtlA: 3})
		Expect(tracker.Get(rrAtlA)).To(Equal(int32(3)))

		tracker.Set(map[string]int32{rrAtlA: 5})
		Expect(tracker.Get(rrAtlA)).To(Equal(int32(5)))
	})
})
