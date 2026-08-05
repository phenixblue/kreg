# Tier 2 test rig

Stands up Tier 2 from `docs/design/architecture.md` §5: real CNI BGP
speakers (Calico's BIRD, Cilium's GoBGP) originating routes, real FRR
route reflectors, and `kreg-controller` peering as a real RR-client — not
the synthetic ExaBGP speakers Tier 1 peers with directly. This is the
first rig in the project that can prove:

- **Real reflection.** Tier 1 has no route reflector — ExaBGP peers
  directly with kreg-controller, so `RIBRoute.Peer` is always the origin.
  Here, kreg-controller only ever sees the RR's address as `peer`, while
  `clusterID` still comes only from `allowedPrefixes` matching — the
  "Attribution note" in §2.1, exercised for real.
- **A real end-to-end request.** A full HTTP round trip through Envoy to
  a real backend pod, over the BGP-routed underlay — not just that the
  generated config looks right.

`frr-edge` (the doc's diagram includes it for "ECMP on the edge") is
deliberately not part of this rig — nothing in kreg-controller talks to
it, so it's topology realism rather than something under test.

## Requirements

**Run this on a real Linux box or VM, not directly on macOS.**
`containerlab` needs real Linux network namespaces to wire veth links
into, which doesn't work through Docker Desktop/OrbStack's VM boundary on
a Mac — this is exactly the limitation §5's Tier 2 section calls out. An
OrbStack (or Lima/UTM) Linux VM works fine; run `up.sh` from inside it.

Needs `docker`, `kind`, `kubectl`, `helm`, `containerlab`, `go`, and `git`
on `PATH` inside that VM — `up.sh` checks for all of them up front. Not
idempotent the way `hack/tier1/up.sh` is (see `up.sh`'s own comment for
why); safe to re-run only when nothing's changed.

```bash
hack/tier2/up.sh      # takes a few minutes: 3 kind clusters, 2 CNIs, Istio
hack/tier2/down.sh    # tears down everything, including all 3 kind clusters
```

## Topology

```
containerlab
├── frr-rr-a, frr-rr-b     real FRR route reflectors
└── links (containerlab's k8s-kind + ext-container node kinds) into:

kind x 3, each created by containerlab's k8s-kind node kind
├── hub     - istio + kreg-controller, RR-client to both RRs
├── spoke-a - Calico (BIRD), clusterID atl-1, VIP 198.51.100.10
└── spoke-b - Cilium (GoBGP), clusterID atl-2, VIP 198.51.100.74
```

Same shared-ASN (`4200000000`) / prefix convention as Tier 1 and the
worked example in §2.1 — `atl-1` is `198.51.100.0/26`, `atl-2` is
`198.51.100.64/26`.

## Real infra gaps this rig had to work around

None of these is a kreg-controller bug — all three are genuine properties
of the systems involved, and worth knowing if you're debugging this rig
or extending it.

**kreg-controller never programs the kernel routing table.** Per
`docs/design/architecture.md`'s "KREG generates config. It is never in
the packet path" — its embedded GoBGP is a RIB consumer for
control-plane decisions only. That means nothing makes hub's *kernel*
know how to reach either spoke's VIP, even though kreg-controller itself
correctly sees both routes. In production, real routers at the network
edge do this; here, `topology.clab.yml` adds one explicit static route
per VIP on hub, each pointing at the RR directly connected to that VIP's
origin spoke (`frr-rr-a` for spoke-a, `frr-rr-b` for spoke-b) — see the
next point for why it's one route per RR rather than one route through
either RR.

**Calico installs learned BGP routes into the kernel; Cilium doesn't.**
Calico's BIRD behaves like a real router — `redistribute connected` on
the RRs (so every node can reach every other node's underlay address, not
just the advertised VIPs) shows up in spoke-a's kernel routing table
automatically. Cilium's BGP control plane is scoped to service/pod-CIDR
*advertisement*, not general route learning, so the same redistributed
routes never reach spoke-b's kernel — without a fix, return traffic from
whoami back to hub has nowhere to go and the TCP handshake times out,
even though every BGP table along the path looks correct.
`topology.clab.yml` adds one explicit static route on spoke-b for this;
spoke-a needs no equivalent.

**Calico doesn't set next-hop-self per BGP session, and the two RRs
aren't otherwise in sync.** Calico advertises the same next-hop value to
both `frr-rr-a` and `frr-rr-b`, rather than each session's own local
address. Whichever RR isn't directly connected to that specific next-hop
can't resolve it and marks the route "inaccessible" — even though it has
a perfectly healthy direct session with the client that sent it — unless
it separately knows a path to that next-hop some other way. `frr-rr-a`
and `frr-rr-b` peer directly with each other (see the `frr.conf`s' `rr-peer`
neighbor) so each learns the other's redistributed connected routes and
can resolve any next-hop either spoke advertises. That peering link fixes
BGP-level route *validity* — it's why kreg-controller sees both VIPs as
`Active` rather than one flapping unpredictably between builds — but
actually forwarding data traffic through it as a transit hop turned out
not to work reliably (not fully root-caused). That's why hub's two static
routes above each go directly to a spoke's own connected RR instead of
picking one RR for the whole VIP range: it sidesteps needing the RR-RR
link for the data plane entirely, using it only for what it's actually
needed for.

## What it stands up beyond the BGP path

- `traefik/whoami`, serving HTTPS (self-signed, generated fresh by
  `up.sh` each run), on both spokes — `backend.tls.mode: SIMPLE` is the
  only TLS posture the Istio driver implements in v1, and it's required,
  so the backend has to actually speak TLS for a real round trip to work.
- `gateways/prod-web` — a `Gateway` + `HTTPRoute` + `BGPBackendPolicy`
  selecting both spokes' VIPs. Unlike `hack/tier1` (which leaves this out
  by default), it ships here since proving the full round trip is the
  point.

## Driving it

```bash
kubectl --context kind-hub run curltest --image=curlimages/curl --rm -i --restart=Never -n gateways \
  -- curl -s http://prod-web-istio.gateways.svc.cluster.local:80/
```

Repeat a few times — responses should alternate between `spoke-a` and
`spoke-b` in the whoami body, showing both real backends being selected.

`kubectl --context kind-hub get advertisedbackend` and
`kubectl --context kind-hub get bgppeerconfig tier2-rig -o yaml` show the
same control-plane state Tier 1's rig does, now backed by real CNI
speakers and real reflection instead of a direct ExaBGP peering.
