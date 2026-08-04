#!/usr/bin/env bash
# Stand up the Tier 1 test rig: docs/design/architecture.md §5.
#
#   kind cluster (hub): istio, kreg-controller
#   docker network (same as kind): exabgp-atl-1, exabgp-atl-2
#
# Idempotent — safe to re-run. Each step checks whether its target
# already exists before acting.
#
# Env vars (all optional):
#   KIND_CLUSTER_NAME  kind cluster to use/create (default: kind)
#   IMG                controller image tag (default: kreg-controller:tier1)
set -euo pipefail

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-kind}"
KUBE_CONTEXT="kind-${KIND_CLUSTER_NAME}"
IMG="${IMG:-kreg-controller:tier1}"
KREG_LISTEN_PORT=1790

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TIER1_DIR="${REPO_ROOT}/hack/tier1"
cd "${REPO_ROOT}"

log() { printf '\n==> %s\n' "$*"; }

log "kind cluster (${KIND_CLUSTER_NAME})"
if ! kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
	kind create cluster --name "${KIND_CLUSTER_NAME}"
fi
kubectl config use-context "${KUBE_CONTEXT}" >/dev/null

log "Istio (base + istiod)"
if ! kubectl --context "${KUBE_CONTEXT}" get ns istio-system >/dev/null 2>&1; then
	kubectl --context "${KUBE_CONTEXT}" create namespace istio-system
fi
if ! helm status istio-base -n istio-system >/dev/null 2>&1; then
	helm repo add istio https://istio-release.storage.googleapis.com/charts >/dev/null
	helm repo update istio >/dev/null
	helm install istio-base istio/base -n istio-system --set defaultRevision=default --wait --kube-context "${KUBE_CONTEXT}"
fi
if ! helm status istiod -n istio-system >/dev/null 2>&1; then
	helm install istiod istio/istiod -n istio-system --wait --timeout 3m --kube-context "${KUBE_CONTEXT}"
fi

log "KREG CRDs"
make install >/dev/null

log "kreg-controller (namespace kreg-tier1, image ${IMG})"
make docker-build IMG="${IMG}" >/dev/null
kind load docker-image "${IMG}" --name "${KIND_CLUSTER_NAME}"
"${REPO_ROOT}/bin/kustomize" build config/tier1 | sed "s|kreg-controller:tier1|${IMG}|" | kubectl --context "${KUBE_CONTEXT}" apply -f -

# Poll by label rather than `kubectl wait` on a resolved pod name: with
# hostNetwork, a RollingUpdate can replace the pod mid-wait (old and new
# can't both hold the same host port on one node), and `wait` binds to
# the specific name(s) it saw at the start — if the pod gets replaced,
# it waits on a name that no longer exists instead of noticing the
# replacement succeeded. Re-querying the label selector each iteration
# is immune to that.
echo "waiting for controller pod..."
ready=""
for i in $(seq 1 45); do
	ready=$(kubectl --context "${KUBE_CONTEXT}" -n kreg-tier1 get pods -l control-plane=controller-manager \
		-o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
	[ "${ready}" = "True" ] && break
	# Stuck on the hostNetwork port conflict from a prior pod that hasn't
	# finished terminating yet: nudge it along. Harmless/no-op otherwise.
	if [ "$i" -eq 15 ]; then
		kubectl --context "${KUBE_CONTEXT}" -n kreg-tier1 delete pods -l control-plane=controller-manager \
			--field-selector=status.phase=Pending --ignore-not-found --wait=false || true
	fi
	sleep 2
done
if [ "${ready}" != "True" ]; then
	echo "controller pod did not become ready in time:" >&2
	kubectl --context "${KUBE_CONTEXT}" -n kreg-tier1 get pods -l control-plane=controller-manager >&2
	exit 1
fi

log "network addressing"
KREG_NODE_IP="$(docker inspect "${KIND_CLUSTER_NAME}-control-plane" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')"
SUBNET="$(docker network inspect kind -f '{{json .IPAM.Config}}' | jq -r '.[] | select(.Subnet | contains(".")) | .Subnet')"
SUBNET_BASE="$(python3 -c "print('.'.join('${SUBNET}'.split('/')[0].split('.')[:3]))")"
ATL1_IP="${SUBNET_BASE}.11"
ATL2_IP="${SUBNET_BASE}.12"
echo "kreg node: ${KREG_NODE_IP}:${KREG_LISTEN_PORT}  atl-1: ${ATL1_IP}  atl-2: ${ATL2_IP}"

log "ExaBGP speakers (atl-1, atl-2)"
RENDER_DIR="$(mktemp -d)"
trap 'rm -rf "${RENDER_DIR}"' EXIT
for name in atl-1 atl-2; do
	mkdir -p "${RENDER_DIR}/exabgp-${name}"
	cp "${TIER1_DIR}/exabgp-${name}/announce.sh" "${RENDER_DIR}/exabgp-${name}/"
	local_ip_var="ATL${name#atl-}_IP"
	sed -e "s/__KREG_NODE_IP__/${KREG_NODE_IP}/" -e "s/__LOCAL_ADDR__/${!local_ip_var}/" \
		"${TIER1_DIR}/exabgp-${name}/exabgp.conf.tmpl" >"${RENDER_DIR}/exabgp-${name}/exabgp.conf"
	docker rm -f "exabgp-${name}" >/dev/null 2>&1 || true
	docker run -d --name "exabgp-${name}" --network kind --ip "${!local_ip_var}" \
		-v "${RENDER_DIR}/exabgp-${name}":/scripts:ro \
		python:3-slim \
		sh -c "pip install --quiet exabgp && exabgp /scripts/exabgp.conf" >/dev/null
done

log "BGPPeerConfig + CommunityMap fixtures"
kubectl --context "${KUBE_CONTEXT}" apply -f - <<EOF
apiVersion: kreg.twr.dev/v1alpha1
kind: BGPPeerConfig
metadata:
  name: tier1-rig
spec:
  localASN: 4200000000
  routerID: 10.0.0.1
  listenPort: ${KREG_LISTEN_PORT}
  mode: RouteReflectorClient
  peers:
    - name: atl-1
      address: ${ATL1_IP}
      remoteASN: 4200000000
    - name: atl-2
      address: ${ATL2_IP}
      remoteASN: 4200000000
  clusterBindings:
    - clusterID: atl-1
      allowedPrefixes: ["198.51.100.0/26"]
      locality: {region: us-east, zone: us-east-atl-a}
    - clusterID: atl-2
      allowedPrefixes: ["198.51.100.64/26"]
      locality: {region: us-east, zone: us-east-atl-b}
---
apiVersion: kreg.twr.dev/v1alpha1
kind: CommunityMap
metadata:
  name: default
spec:
  rules:
    - match: {largeCommunity: "4200000000:1:*"}
      set: {field: weight, fromCommunityValue: true}
    - match: {largeCommunity: "4200000000:2:1"}
      set: {field: tier, value: canary}
    - match: {largeCommunity: "4200000000:3:1"}
      set: {field: drain, value: "true"}
    - match: {largeCommunity: "4200000000:4:*"}
      set: {field: serviceTag, fromCommunityValue: true}
  fallbacks:
    weightFrom: MED
    defaultWeight: 100
  onUnmappedCommunity: Ignore
EOF

log "waiting for BGP sessions to establish"
for i in $(seq 1 30); do
	established=$(kubectl --context "${KUBE_CONTEXT}" get bgppeerconfig tier1-rig -o jsonpath='{range .status.peers[*]}{.sessionState}{"\n"}{end}' 2>/dev/null | grep -c Established || true)
	[ "${established}" = "2" ] && break
	sleep 2
done

echo
kubectl --context "${KUBE_CONTEXT}" get bgppeerconfig tier1-rig -o yaml | sed -n '/^status:/,$p'
echo
echo "Rig is up. Apply a BGPBackendPolicy selecting 198.51.100.0/24 to see it flow through."
echo "Drive live changes: docker exec exabgp-atl-1 sh -c 'echo \"withdraw route 198.51.100.10/32 next-hop self large-community [4200000000:1:80 4200000000:4:80]\" > /tmp/cmds'"
echo "Tear down: hack/tier1/down.sh"
