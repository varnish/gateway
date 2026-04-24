#!/usr/bin/env bash
# Stand up a kind cluster primed to run the chaos suite.
#
# Builds and loads all required images (operator, chaperone, echo,
# collector), installs Chaos Mesh, deploys the operator and load
# suite, and prints the port-forwards you need to run C01.
#
# Idempotent — safe to re-run. Each step checks whether its work is
# already done.
#
# Usage:
#   ./test/chaos/kind-setup.sh             # full setup
#   ./test/chaos/kind-setup.sh teardown    # remove the cluster
#
# Requires: kind (go tool), kubectl, helm, docker.
set -euo pipefail

repo=$(cd "$(dirname "$0")/../.." && pwd)
cluster=${KIND_CLUSTER_NAME:-varnish-gw}
chaos_version=${CHAOS_MESH_VERSION:-2.6.3}
version=${VERSION:-kind}  # image tag used by make kind-load

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing dependency: $1" >&2
    exit 2
  }
}

need kubectl
need helm
need docker
# kind is a go tool in this repo; the Makefile shells out to it.

if [[ "${1:-}" == "teardown" ]]; then
  echo "==> deleting kind cluster $cluster"
  make -C "$repo" kind-delete KIND_CLUSTER_NAME="$cluster" || true
  exit 0
fi

echo "==> ensuring kind cluster $cluster exists"
if ! kubectl config get-contexts "kind-$cluster" >/dev/null 2>&1; then
  make -C "$repo" kind-create KIND_CLUSTER_NAME="$cluster"
else
  echo "    cluster already present, skipping creation"
fi
kubectl config use-context "kind-$cluster" >/dev/null

echo "==> building docker images (tag=$version)"
make -C "$repo" docker VERSION="$version" >/dev/null
make -C "$repo" load-docker VERSION="$version" >/dev/null

echo "==> loading images into kind"
for img in \
    "ghcr.io/varnish/gateway-operator:$version" \
    "ghcr.io/varnish/gateway-chaperone:$version" \
    "ghcr.io/varnish/gateway-echo:$version" \
    "ghcr.io/varnish/gateway-ledger-collector:$version"; do
  go tool kind load docker-image "$img" --name "$cluster"
done

echo "==> deploying operator"
make -C "$repo" kind-deploy VERSION="$version" >/dev/null

echo "==> installing Chaos Mesh $chaos_version"
if ! helm status chaos-mesh -n chaos-mesh >/dev/null 2>&1; then
  helm repo add chaos-mesh https://charts.chaos-mesh.org >/dev/null 2>&1 || true
  helm repo update >/dev/null
  # kind uses containerd; the socket path differs from the default.
  helm install chaos-mesh chaos-mesh/chaos-mesh \
    --namespace chaos-mesh \
    --create-namespace \
    --version "$chaos_version" \
    --set chaosDaemon.runtime=containerd \
    --set chaosDaemon.socketPath=/run/containerd/containerd.sock \
    --wait --timeout 3m
else
  echo "    chaos-mesh release already present, skipping"
fi

echo "==> deploying load suite"
make -C "$repo" load-up >/dev/null

echo
echo "Cluster ready. To run C01:"
echo
echo "  # terminal 1 — collector port-forward"
echo "  kubectl -n varnish-load port-forward svc/ledger-collector 9090:8080"
echo
echo "  # terminal 2 — gateway port-forward (service is named after the Gateway)"
echo "  kubectl -n varnish-load port-forward svc/load 8080:80"
echo
echo "  # terminal 3 — run the scenario"
echo "  export GATEWAY_URL=http://127.0.0.1:8080"
echo "  export COLLECTOR_URL=http://127.0.0.1:9090"
echo "  make chaos-run SCENARIO=C01"
echo
echo "Teardown with:  $0 teardown"
