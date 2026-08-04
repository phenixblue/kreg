#!/usr/bin/env bash
# Drives repeated withdraw/announce cycles against exabgp-atl-1's route
# (198.51.100.10/32, from docs/design/architecture.md's worked example)
# to exercise the Damper: enough flaps in quick succession should cross
# a BGPStabilityConfig's suppressThreshold and move the matching
# AdvertisedBackend to state: Dampened.
#
# Requires the Tier 1 rig already up (`make tier1-up`) and a
# BGPStabilityConfig named "default" applied with thresholds tight
# enough to trigger within this script's cycle count/interval -- the
# rig itself doesn't ship one, since the "no config yet" default
# (dampening disabled) is deliberately the safe out-of-the-box behavior.
# For example:
#
#   cat <<'EOF' | kubectl apply -f -
#   apiVersion: kreg.twr.dev/v1alpha1
#   kind: BGPStabilityConfig
#   metadata:
#     name: default
#   spec:
#     withdrawalGrace: 5s
#     dampening:
#       enabled: true
#       halfLife: 15s
#       suppressThreshold: 2500
#       reuseThreshold: 750
#       maxSuppress: 2m
#   EOF
#
# Env vars (all optional):
#   CONTAINER  ExaBGP container to drive (default: exabgp-atl-1)
#   PREFIX     route to flap (default: 198.51.100.10/32, atl-1's)
#   CYCLES     number of withdraw+announce cycles (default: 5)
#   INTERVAL   seconds between each withdraw and each announce (default: 1)
set -euo pipefail

CONTAINER="${CONTAINER:-exabgp-atl-1}"
PREFIX="${PREFIX:-198.51.100.10/32}"
CYCLES="${CYCLES:-5}"
INTERVAL="${INTERVAL:-1}"
COMMUNITIES="large-community [4200000000:1:80 4200000000:4:80]"

log() { printf '\n==> %s\n' "$*"; }

log "flapping ${PREFIX} on ${CONTAINER}: ${CYCLES} withdraw/announce cycles, ${INTERVAL}s apart"

for i in $(seq 1 "${CYCLES}"); do
	log "cycle ${i}/${CYCLES}: withdraw"
	docker exec "${CONTAINER}" sh -c "echo 'withdraw route ${PREFIX} next-hop self ${COMMUNITIES}' > /tmp/cmds"
	sleep "${INTERVAL}"

	log "cycle ${i}/${CYCLES}: announce"
	docker exec "${CONTAINER}" sh -c "echo 'announce route ${PREFIX} next-hop self ${COMMUNITIES}' > /tmp/cmds"
	sleep "${INTERVAL}"
done

log "done — check the result, e.g.:"
echo "  kubectl get advertisedbackend 198-51-100-10-32-atl-1 -o yaml"
