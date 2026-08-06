#!/usr/bin/env bash
# Stand up the Tier 2 test rig: docs/design/architecture.md §5.
#
#   containerlab: frr-rr-a, frr-rr-b (real FRR route reflectors)
#   kind x 3, each created by containerlab's k8s-kind node kind:
#     hub     - istio + kreg-controller, RR-client to both RRs
#     spoke-a - Calico (BIRD), advertises a real Service VIP over BGP
#     spoke-b - Cilium (GoBGP), advertises a real Service VIP over BGP
#
# Unlike Tier 1's synthetic ExaBGP speakers peering directly with
# kreg-controller, this proves real CNI-driven BGP origination and real
# route-reflector reflection (kreg-controller sees each route's peer as
# the RR, never the origin - the "Attribution note" in §2.1 exercised for
# real) - plus a full HTTP round trip through Envoy to a real backend
# pod, over the BGP-routed underlay.
#
# containerlab needs real Linux network namespaces to wire veth links
# into, which doesn't work under Docker Desktop/OrbStack on macOS - run
# this on a real Linux box or VM (see hack/tier2/README.md).
#
# NOT idempotent the way hack/tier1/up.sh is: this stands up 5
# interlinked nodes across 3 Kubernetes clusters plus two CNIs, and
# reconciling a changed topology in place is riskier than it's worth for
# a dev rig. Safe to re-run only when nothing changed; otherwise run
# down.sh first.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TIER2_DIR="${REPO_ROOT}/hack/tier2"
IMG="${IMG:-kreg-controller:tier2}"

log() { printf '\n==> %s\n' "$*"; }

for bin in docker kind kubectl helm containerlab go git make openssl sed grep mktemp seq sleep sudo; do
	command -v "${bin}" >/dev/null || { echo "missing required tool: ${bin}" >&2; exit 1; }
done
# containerlab needs root to wire veth links into other containers'
# network namespaces. -n (non-interactive) rather than -v: this rig only
# works non-interactively against passwordless sudo anyway (containerlab
# itself is invoked the same way below), so failing fast here on the
# same terms is more honest than prompting for a password up front and
# then failing later regardless.
sudo -n true || { echo "passwordless sudo access is required (containerlab wires network namespaces as root)" >&2; exit 1; }

log "kreg-controller image (${IMG})"
cd "${REPO_ROOT}"
make docker-build IMG="${IMG}" >/dev/null

log "containerlab topology (frr-rr-a, frr-rr-b, hub, spoke-a, spoke-b)"
cd "${TIER2_DIR}"
if sudo -n containerlab inspect -t topology.clab.yml >/dev/null 2>&1; then
	echo "lab already deployed - skipping (run down.sh first for a clean rebuild)"
else
	sudo -n containerlab deploy -t topology.clab.yml
fi

for c in hub spoke-a spoke-b; do
	kind export kubeconfig --name "${c}" >/dev/null
	kind load docker-image "${IMG}" --name "${c}" >/dev/null
done

log "TLS cert for whoami (self-signed, ephemeral - regenerated each run)"
CERT_DIR="$(mktemp -d)"
trap 'rm -rf "${CERT_DIR}"' EXIT
openssl req -x509 -newkey rsa:2048 -nodes \
	-keyout "${CERT_DIR}/tls.key" -out "${CERT_DIR}/tls.crt" -days 30 \
	-subj "/CN=whoami.tier2.local" -addext "subjectAltName=DNS:whoami.tier2.local" >/dev/null 2>&1

log "Calico on spoke-a (clusterID atl-1, VIP 198.51.100.10)"
helm repo add projectcalico https://docs.tigera.io/calico/charts --force-update >/dev/null
helm repo update projectcalico >/dev/null
kubectl --context kind-spoke-a create namespace tigera-operator --dry-run=client -o yaml | kubectl --context kind-spoke-a apply -f - >/dev/null
helm template calico-crds projectcalico/crd.projectcalico.org.v1 --version v3.32.1 |
	kubectl --context kind-spoke-a apply --server-side -f - >/dev/null
helm upgrade --install calico projectcalico/tigera-operator --version v3.32.1 \
	-f calico/values.yaml --namespace tigera-operator --kube-context kind-spoke-a >/dev/null
echo "waiting for spoke-a node to be Ready..."
kubectl --context kind-spoke-a wait node --all --for=condition=Ready --timeout=3m >/dev/null
# calico/bgp.yaml uses the projectcalico.org/v3 aggregated API, served by
# calico-apiserver proxying the crd.projectcalico.org CRDs underneath -
# node Ready doesn't imply that aggregation is registered yet, so a
# kubectl apply here can race it ("no matches for kind BGPConfiguration")
# even though the base CRDs already exist.
echo "waiting for the projectcalico.org/v3 API to be available..."
calico_api_ready=""
for i in $(seq 1 60); do
	kubectl --context kind-spoke-a get bgpconfigurations.projectcalico.org >/dev/null 2>&1 && { calico_api_ready=1; break; }
	sleep 2
done
if [ -z "${calico_api_ready}" ]; then
	echo "projectcalico.org/v3 API never became available - calico-apiserver status:" >&2
	kubectl --context kind-spoke-a get pods -n calico-system -l k8s-app=calico-apiserver >&2 || true
	exit 1
fi
kubectl --context kind-spoke-a apply -f calico/bgp.yaml >/dev/null
kubectl --context kind-spoke-a create secret tls whoami-tls \
	--cert="${CERT_DIR}/tls.crt" --key="${CERT_DIR}/tls.key" \
	--dry-run=client -o yaml | kubectl --context kind-spoke-a apply -f - >/dev/null
kubectl --context kind-spoke-a apply -f whoami/spoke-a.yaml >/dev/null

log "Cilium on spoke-b (clusterID atl-2, VIP 198.51.100.74)"
helm repo add cilium https://helm.cilium.io/ --force-update >/dev/null
helm repo update cilium >/dev/null
helm upgrade --install cilium cilium/cilium --version 1.20.0 \
	-f cilium/values.yaml --namespace kube-system --kube-context kind-spoke-b >/dev/null
echo "waiting for spoke-b node to be Ready..."
kubectl --context kind-spoke-b wait node --all --for=condition=Ready --timeout=3m >/dev/null
kubectl --context kind-spoke-b apply -f cilium/bgp.yaml >/dev/null
kubectl --context kind-spoke-b create secret tls whoami-tls \
	--cert="${CERT_DIR}/tls.crt" --key="${CERT_DIR}/tls.key" \
	--dry-run=client -o yaml | kubectl --context kind-spoke-b apply -f - >/dev/null
kubectl --context kind-spoke-b apply -f whoami/spoke-b.yaml >/dev/null

log "Istio + KREG CRDs + kreg-controller on hub"
kubectl --context kind-hub create namespace istio-system --dry-run=client -o yaml | kubectl --context kind-hub apply -f - >/dev/null
helm repo add istio https://istio-release.storage.googleapis.com/charts --force-update >/dev/null
helm repo update istio >/dev/null
helm upgrade --install istio-base istio/base -n istio-system --set defaultRevision=default \
	--wait --kube-context kind-hub >/dev/null
helm upgrade --install istiod istio/istiod -n istio-system --wait --timeout 3m --kube-context kind-hub >/dev/null
kubectl --context kind-hub apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.1/standard-install.yaml >/dev/null
# make install's own kubectl calls have no --context flag of their own,
# so they rely on whatever the ambient kubeconfig's current-context is.
# No KUBECONFIG override here — respects whatever the caller already has
# set (the kind export kubeconfig calls above did too), rather than
# assuming ~/.kube/config, which would be wrong for a caller using a
# different kubeconfig path.
kubectl config use-context kind-hub >/dev/null
(cd "${REPO_ROOT}" && make install >/dev/null)
"${REPO_ROOT}/bin/kustomize" build "${REPO_ROOT}/config/tier2" |
	sed "s|kreg-controller:tier2|${IMG}|" | kubectl --context kind-hub apply -f - >/dev/null

echo "waiting for kreg-controller..."
ready=""
for i in $(seq 1 30); do
	ready=$(kubectl --context kind-hub -n kreg-tier2 get pods -l control-plane=controller-manager \
		-o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
	[ "${ready}" = "True" ] && break
	sleep 2
done
if [ "${ready}" != "True" ]; then
	echo "kreg-controller did not become ready in time:" >&2
	kubectl --context kind-hub -n kreg-tier2 get pods -l control-plane=controller-manager >&2 || true
	exit 1
fi

log "BGPPeerConfig + CommunityMap fixtures"
kubectl --context kind-hub apply -f - <<EOF
apiVersion: kreg.twr.dev/v1alpha1
kind: BGPPeerConfig
metadata:
  name: tier2-rig
spec:
  localASN: 4200000000
  routerID: 10.201.255.10
  listenPort: 1790
  mode: RouteReflectorClient
  peers:
    - name: rr-a
      address: 10.201.0.1
      remoteASN: 4200000000
    - name: rr-b
      address: 10.201.0.5
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
  fallbacks:
    weightFrom: MED
    defaultWeight: 100
  onUnmappedCommunity: Ignore
EOF

log "Gateway + HTTPRoute + BGPBackendPolicy (gateways/prod-web)"
kubectl --context kind-hub create namespace gateways --dry-run=client -o yaml | kubectl --context kind-hub apply -f - >/dev/null
kubectl --context kind-hub -n gateways create secret generic whoami-ca \
	--from-file=ca.crt="${CERT_DIR}/tls.crt" --dry-run=client -o yaml |
	kubectl --context kind-hub apply -f - >/dev/null
kubectl --context kind-hub apply -f gateway.yaml >/dev/null

log "waiting for BGP sessions to establish"
established=0
for i in $(seq 1 30); do
	established=$(kubectl --context kind-hub get bgppeerconfig tier2-rig \
		-o jsonpath='{range .status.peers[*]}{.sessionState}{"\n"}{end}' 2>/dev/null | grep -c Established || true)
	[ "${established}" = "2" ] && break
	sleep 2
done
if [ "${established}" != "2" ]; then
	echo "BGP sessions did not both reach Established in time:" >&2
	kubectl --context kind-hub get bgppeerconfig tier2-rig -o yaml >&2 | sed -n '/^status:/,$p' >&2
	exit 1
fi

echo
kubectl --context kind-hub get bgppeerconfig tier2-rig -o yaml | sed -n '/^status:/,$p'
echo
kubectl --context kind-hub get advertisedbackend 2>&1 || true
echo
echo "Rig is up. Try a full round trip:"
echo "  kubectl --context kind-hub run curltest --image=curlimages/curl --rm -i --restart=Never -n gateways -- curl -s http://prod-web-istio.gateways.svc.cluster.local:80/"
echo "Tear down: hack/tier2/down.sh"
