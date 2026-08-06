#!/usr/bin/env bash
# Tear down everything up.sh created: containerlab destroy removes the
# FRR nodes and, via the k8s-kind node kind, the hub/spoke-a/spoke-b kind
# clusters too - unlike Tier 1, there's no "leave the cluster running"
# middle ground here, since the whole point is the multi-cluster
# topology, not a single reusable cluster.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TIER2_DIR="${REPO_ROOT}/hack/tier2"

log() { printf '\n==> %s\n' "$*"; }

# Non-interactive, matching up.sh: fail fast rather than let a
# password prompt hang teardown while the trailing || true below still
# prints a false "torn down" success.
sudo -n true || { echo "passwordless sudo access is required (containerlab wires network namespaces as root)" >&2; exit 1; }

log "containerlab topology (frr-rr-a, frr-rr-b, hub, spoke-a, spoke-b)"
cd "${TIER2_DIR}"
if ! sudo -n containerlab inspect -t topology.clab.yml >/dev/null 2>&1; then
	echo "nothing deployed - nothing to tear down"
	exit 0
fi
sudo -n containerlab destroy -t topology.clab.yml --cleanup

echo
echo "Torn down. Rerun hack/tier2/up.sh to bring it back."
