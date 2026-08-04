# Tier 1 test rig

Stands up Tier 1 from `docs/design/architecture.md` §5: a real deployed
`kreg-controller` in a real `kind` cluster with Istio installed, peering
with real ExaBGP speakers over real BGP — not fixtures, not a one-shot
script. Validates real BGP wire parsing and real Istio config acceptance
without the cost of real clusters, and lets you drive live route changes
(flaps, community edits, withdrawals) against a running rig.

## Usage

```bash
make tier1-up      # idempotent — safe to re-run
make tier1-down    # tears down the rig; leaves the kind cluster/Istio/CRDs
```

`tier1-up` needs `kind`, `docker`, `helm`, `kubectl`, `jq`, and `python3`
on `PATH`. It creates a `kind` cluster if one by that name doesn't
already exist (override the name with `KIND_CLUSTER_NAME`).

## What it stands up

- Istio (`istio-base` + `istiod`, via Helm) and KREG's CRDs, in whatever
  kind cluster `KIND_CLUSTER_NAME` points at (default `kind`).
- `kreg-controller`, built from source and deployed for real (not
  `go run`) into a dedicated `kreg-tier1` namespace — see
  `config/tier1/kustomization.yaml`. Runs with `hostNetwork: true` on a
  non-privileged BGP port (1790, not 179) so it's reachable from the
  ExaBGP containers without loosening the pod's `restricted` security
  context.
- Two `exabgp-atl-1`/`exabgp-atl-2` containers on the `kind` Docker
  network, simulating the `atl-1`/`atl-2` workload clusters from the
  design doc's worked example. Each has a `process` script
  (`exabgp-atl-*/announce.sh`) that announces one `/32` with large
  communities matching the doc's `CommunityMap` rules.
- A `BGPPeerConfig` (`tier1-rig`) peering with both, and the `CommunityMap`
  (`default`) that decodes their communities.

The ExaBGP containers' own address and the controller's node address
aren't fixed — the kind Docker network's subnet varies by machine — so
`up.sh` discovers the subnet and the node's actual IP at run time and
renders `exabgp-atl-*/exabgp.conf.tmpl` into a temp directory before
starting each container.

## Driving live changes

Each ExaBGP container reads commands from a FIFO at `/tmp/cmds`:

```bash
docker exec exabgp-atl-1 sh -c 'echo "withdraw route 198.51.100.10/32 next-hop self large-community [4200000000:1:80 4200000000:4:80]" > /tmp/cmds'
```

Same syntax for `announce route ...` to re-advertise or change
communities. See the [ExaBGP command
reference](https://github.com/Exa-Networks/exabgp/wiki/Command-Reference)
— note this rig uses ExaBGP 4.2.25's current config keywords (`connect`
for a non-standard peer port, not the older `peer-port`).

## What it doesn't do

Apply your own `BGPBackendPolicy` selecting `198.51.100.0/24` to see
routes actually flow into `Service`/`EndpointSlice`/`DestinationRule`
objects — this rig doesn't create one for you (`gateways/prod-web` from
the step-1 functional test works if it's still around). No Gateway API
CRDs, no `Gateway`/`HTTPRoute` fixtures, no real data-plane traffic —
see `docs/design/architecture.md` §5 for why that's out of scope here.
