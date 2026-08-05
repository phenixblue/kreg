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

// This file is an internal test (package ingest, not ingest_test)
// specifically so it can drive the peer server's underlying
// *gobgpserver.BgpServer directly via AddPath to stand in for an
// external BGP speaker (what ExaBGP or Calico/Cilium would be in
// production). Manager itself deliberately has no AddPath-like method —
// KREG is read-only on the BGP side (docs/design/architecture.md,
// README Non-goals) — so this capability only exists here, as test
// scaffolding representing the other end of the wire, never as
// something the real Manager can do.
package ingest

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	api "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/apiutil"
	"github.com/osrg/gobgp/v3/pkg/packet/bgp"

	kregv1alpha1 "github.com/phenixblue/kreg/api/v1alpha1"
	"github.com/phenixblue/kreg/internal/pipeline"
)

// advertise makes speaker originate one route with the given attributes,
// standing in for an external BGP speaker in these tests.
func advertise(ctx context.Context, speaker *Manager, prefix string, prefixLen uint8, med uint32, asPath []uint32, communities [][3]uint32) error {
	nlri, err := apiutil.MarshalNLRI(bgp.NewIPAddrPrefix(prefixLen, prefix))
	if err != nil {
		return err
	}

	largeCommunities := make([]*bgp.LargeCommunity, len(communities))
	for i, c := range communities {
		largeCommunities[i] = bgp.NewLargeCommunity(c[0], c[1], c[2])
	}

	attrs, err := apiutil.MarshalPathAttributes([]bgp.PathAttributeInterface{
		bgp.NewPathAttributeOrigin(0),
		bgp.NewPathAttributeNextHop("0.0.0.0"), // mandatory well-known attribute; value unused by this test
		bgp.NewPathAttributeAsPath([]bgp.AsPathParamInterface{
			bgp.NewAs4PathParam(bgp.BGP_ASPATH_ATTR_TYPE_SEQ, asPath),
		}),
		bgp.NewPathAttributeMultiExitDisc(med),
		bgp.NewPathAttributeLargeCommunities(largeCommunities),
	})
	if err != nil {
		return err
	}

	_, err = speaker.server.AddPath(ctx, &api.AddPathRequest{
		TableType: api.TableType_GLOBAL,
		Path: &api.Path{
			Nlri:   nlri,
			Pattrs: attrs,
			Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST},
		},
	})
	return err
}

var _ = Describe("Manager", func() {
	var ctx context.Context
	var cancel context.CancelFunc
	var kreg, speaker *Manager

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background())

		kreg = NewManager()
		speaker = NewManager()
		go kreg.Serve()
		go speaker.Serve()

		DeferCleanup(func() {
			kreg.Stop()
			speaker.Stop()
			cancel()
		})

		// Loopback, non-privileged ports: kreg listens on 17900 and
		// peers with speaker on 17901; both directions, since BGP
		// sessions are symmetric once configured. Same ASN on both
		// sides — iBGP to a route reflector, matching the real peering
		// model in docs/design/architecture.md §2.1 ("remoteASN:
		// 4200000000 # iBGP to the RR"). An eBGP session here would
		// have GoBGP correctly prepend the speaker's own ASN on send,
		// which isn't what real RR peering does.
		const asn = 4200000000
		Expect(kreg.Reconfigure(ctx, &kregv1alpha1.BGPPeerConfigSpec{
			LocalASN:   asn,
			RouterID:   "10.0.0.1",
			ListenPort: ptrInt32(17900),
			Peers: []kregv1alpha1.BGPPeer{{
				Name:      "speaker",
				Address:   "127.0.0.1:17901",
				RemoteASN: asn,
			}},
		}, nil)).To(Succeed())

		Expect(speaker.Reconfigure(ctx, &kregv1alpha1.BGPPeerConfigSpec{
			LocalASN:   asn,
			RouterID:   "10.0.0.2",
			ListenPort: ptrInt32(17901),
			Peers: []kregv1alpha1.BGPPeer{{
				Name:      "kreg",
				Address:   "127.0.0.1:17900",
				RemoteASN: asn,
			}},
		}, nil)).To(Succeed())

		Eventually(func() kregv1alpha1.PeerSessionState {
			statuses, err := kreg.Status(ctx)
			if err != nil || len(statuses) == 0 {
				return ""
			}
			return statuses[0].SessionState
		}, 10*time.Second, 100*time.Millisecond).Should(Equal(kregv1alpha1.PeerSessionStateEstablished))
	})

	It("decodes a real advertised route over the wire into a RIBRoute", func() {
		Expect(advertise(ctx, speaker, "198.51.100.10", 32, 100, []uint32{4200000101},
			[][3]uint32{{4200000000, 1, 80}, {4200000000, 4, 80}})).To(Succeed())

		Eventually(func() ([]pipeline.RIBRoute, error) {
			return kreg.Snapshot(ctx)
		}, 10*time.Second, 100*time.Millisecond).Should(HaveLen(1))

		routes, err := kreg.Snapshot(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(routes).To(HaveLen(1))

		route := routes[0]
		Expect(route.Prefix).To(Equal("198.51.100.10/32"))
		Expect(route.Peer).To(Equal("127.0.0.1"))
		Expect(route.MED).To(Equal(uint32(100)))
		Expect(route.ASPath).To(Equal([]uint32{4200000101}))
		Expect(route.LargeCommunities).To(ConsistOf("4200000000:1:80", "4200000000:4:80"))
	})

	It("reports Established session state and prefix counts in Status", func() {
		Expect(advertise(ctx, speaker, "198.51.100.20", 32, 50, []uint32{4200000101}, nil)).To(Succeed())

		Eventually(func() int32 {
			statuses, err := kreg.Status(ctx)
			if err != nil || len(statuses) == 0 {
				return 0
			}
			return statuses[0].PrefixesReceived
		}, 10*time.Second, 100*time.Millisecond).Should(BeNumerically(">=", 1))

		statuses, err := kreg.Status(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(statuses).To(HaveLen(1))
		Expect(statuses[0].SessionState).To(Equal(kregv1alpha1.PeerSessionStateEstablished))
	})

	Describe("toAPIPeer", func() {
		// Unit-level, not a live session: GoBGP's TCP-MD5 socket option
		// (RFC 2385) is Linux-only — darwin's setTcpMD5SigSockopt always
		// errors "not supported" — so a real loopback MD5 session can't be
		// exercised portably here. Live wire-level verification happens
		// against the Tier 1 rig's real Linux containers instead; this
		// just confirms the resolved password reaches GoBGP's peer config.
		peer := kregv1alpha1.BGPPeer{
			Name:      "rr-atl-a",
			Address:   "10.0.10.1",
			RemoteASN: 4200000000,
		}

		It("sets AuthPassword when a password is resolved for this peer", func() {
			apiPeer := toAPIPeer(peer, "s3cr3t")
			Expect(apiPeer.Conf.AuthPassword).To(Equal("s3cr3t"))
		})

		It("leaves AuthPassword empty when no password is resolved", func() {
			apiPeer := toAPIPeer(peer, "")
			Expect(apiPeer.Conf.AuthPassword).To(BeEmpty())
		})
	})
})

func ptrInt32(v int32) *int32 { return &v }
