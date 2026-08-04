#!/usr/bin/env bash
# Tear down what up.sh created. Leaves the kind cluster, Istio, and KREG
# CRDs in place — only the tier1-specific pieces come down. Doesn't touch
# any BGPBackendPolicy (e.g. prod-web) you applied separately.
set -euo pipefail

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-kind}"
KUBE_CONTEXT="kind-${KIND_CLUSTER_NAME}"

log() { printf '\n==> %s\n' "$*"; }

log "ExaBGP speakers"
docker rm -f exabgp-atl-1 exabgp-atl-2 >/dev/null 2>&1 || true

log "BGPPeerConfig + CommunityMap fixtures"
kubectl --context "${KUBE_CONTEXT}" delete bgppeerconfig tier1-rig --ignore-not-found
kubectl --context "${KUBE_CONTEXT}" delete communitymap default --ignore-not-found

log "kreg-controller"
kubectl --context "${KUBE_CONTEXT}" delete namespace kreg-tier1 --ignore-not-found

echo
echo "Torn down. kind cluster, Istio, and KREG CRDs are untouched — rerun hack/tier1/up.sh to bring it back."
