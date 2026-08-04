# KREG — Kubernetes Routable Edge Gateway

A control-plane translator that consumes BGP routing state advertised by
Kubernetes clusters and reconciles it into Istio + Gateway API configuration.

KREG generates config. It is never in the packet path.

## The problem

Multi-cluster north/south ingress usually gets solved one of two ways: a cloud
provider's global load balancer, or DNS-based global server load balancing.
Both require the ingress tier to hold credentials for every workload cluster,
and both steer traffic at a layer far above the network.

KREG inverts this. Workload clusters advertise their own reachability over
BGP — service VIPs as `/32`s, with large communities carrying weight,
locality, tier, and drain state. An edge ingress tier running Istio consumes
that routing state and programs itself accordingly. Anycast and ECMP do the
steering. The ingress tier never holds a kubeconfig for any workload cluster.

## Status

Early design. Nothing is implemented yet. See
[`docs/design/architecture.md`](docs/design/architecture.md) for the CRD
surface, reconcile model, failure semantics, and dev/production footprints.

## Design at a glance

```
workload clusters (Calico/BIRD, Cilium/GoBGP)
        │  advertise service VIP /32s + large communities
        ▼
   route reflectors (BIRD / FRR)
        │  iBGP
        ▼
   kreg-controller ──► ServiceEntry / WorkloadEntry / DestinationRule
        │                              │
        │                              ▼
        └──────────────────────►  istiod ──► Istio ingress gateway
                                                    ▲
                                          anycast + ECMP from the edge
```

App teams author plain `HTTPRoute`s. The BGP layer is invisible to them.

## API

Five kinds under `kreg.io/v1alpha1`:

| Kind | Scope | Purpose |
|---|---|---|
| `BGPPeerConfig` | Cluster | Peering config and the prefix→cluster trust boundary |
| `CommunityMap` | Cluster | Decodes BGP communities into weight, tier, locality, drain |
| `BGPBackendPolicy` | Namespaced | Policy attachment onto a `Gateway` or `HTTPRoute` |
| `AdvertisedBackend` | Cluster | Read-only materialized view of the RIB, for debugging |
| `BGPGatewayClassConfig` | Cluster | Defaults inherited by policies |

## Non-goals

- Replacing your CNI's BGP speaker. KREG consumes what Calico, Cilium,
  MetalLB, or kube-router already advertise.
- Being a gateway. Istio does that.
- Originating routes. KREG is read-only on the BGP side in v1.
- East-west / service mesh connectivity. That's Submariner, Skupper, or
  Istio multicluster.

## Prior art

- [Kuadrant](https://kuadrant.io) — same hub/spoke Gateway API policy-attachment
  shape, but steers via DNS rather than BGP.
- [Admiral](https://github.com/istio-ecosystem/admiral) — Istio config
  generation from multi-cluster discovery.
- [GoBMP / Jalapeno](https://github.com/cisco-open/jalapeno) — turning BGP
  state into structured, queryable data.

## License

TBD.
