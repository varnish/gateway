#!/usr/bin/env bash
# C05 (P1): chaperone ↔ apiserver network partition.
#
# Apply a Chaos Mesh NetworkChaos that blocks chaperone pod egress
# to the kube-apiserver for PARTITION_S seconds. During the partition
# the informers cannot receive updates; after reconnect, chaperone
# must resync without restart loops.
#
# Verification:
#   1. Chaperone pods do not enter CrashLoopBackOff during or after.
#   2. A canary HTTPRoute applied *during* the partition eventually
#      lands in ghost.json after the partition is lifted, proving
#      informers resynced and the chaperone reload pipeline resumed.
set -euo pipefail

gw_ns=${GATEWAY_NAMESPACE:-varnish-load}
gw_name=${GATEWAY_NAME:-load}
route_ns=${ROUTE_NAMESPACE:-varnish-load}
partition_s=${PARTITION_S:-60}
converge_s=${PARTITION_CONVERGE_S:-30}

# NetworkChaos targeting chaperone pods' egress to the kube-apiserver.
# externalTargets uses the in-cluster apiserver service name — this is
# recognised by Chaos Mesh and resolved to the apiserver IPs.
cr=$(mktemp --suffix=.yaml)
trap 'rm -f "$cr"' EXIT
cat >"$cr" <<EOF
apiVersion: chaos-mesh.org/v1alpha1
kind: NetworkChaos
metadata:
  name: c05-apiserver-partition
  namespace: $gw_ns
spec:
  action: partition
  mode: all
  selector:
    namespaces: [$gw_ns]
    labelSelectors:
      gateway.networking.k8s.io/gateway-name: $gw_name
  direction: to
  externalTargets:
    - kubernetes.default.svc.cluster.local
  duration: ${partition_s}s
EOF

canary=c05-canary
canary_host="c05.load.local"

cleanup() {
  kubectl delete -f "$cr" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$route_ns" delete httproute "$canary" --ignore-not-found >/dev/null 2>&1 || true
}
trap 'cleanup; rm -f "$cr"' EXIT

echo "C05: applying NetworkChaos for ${partition_s}s"
kubectl apply -f "$cr" >/dev/null

# Give the partition a moment to take effect, then apply the canary
# *during* the partition — it should not reach chaperone yet.
sleep 5
echo "C05: applying canary HTTPRoute during partition"
kubectl apply -f - >/dev/null <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: $canary
  namespace: $route_ns
  labels:
    chaos-scenario: C05
spec:
  parentRefs: [{ name: $gw_name }]
  hostnames: ["$canary_host"]
  rules:
    - matches: [{ path: { type: PathPrefix, value: "/" } }]
      backendRefs:
        - name: echo-a
          port: 8080
EOF

# Wait for partition to end.
echo "C05: waiting ${partition_s}s for partition to lift"
sleep "$partition_s"
kubectl delete -f "$cr" --ignore-not-found >/dev/null

# Verify chaperone pods are not crashlooping.
echo "C05: checking chaperone pod health"
bad=$(kubectl -n "$gw_ns" get pod \
  -l "gateway.networking.k8s.io/gateway-name=$gw_name" \
  -o jsonpath='{range .items[*]}{.status.phase}{" "}{.status.containerStatuses[*].restartCount}{"\n"}{end}')
while IFS= read -r line; do
  restarts=$(echo "$line" | awk '{for (i=2; i<=NF; i++) sum+=$i; print sum}')
  if (( ${restarts:-0} > 2 )); then
    echo "C05: chaperone container restarted ${restarts}× during partition (CrashLoopBackOff suspected)" >&2
    exit 1
  fi
done <<<"$bad"

# Verify canary converges after resync.
echo "C05: waiting ${converge_s}s for canary to resync into ghost.json"
pod=$(kubectl -n "$gw_ns" get pod \
  -l "gateway.networking.k8s.io/gateway-name=$gw_name" \
  -o jsonpath='{.items[0].metadata.name}')
deadline=$((SECONDS + converge_s))
while (( SECONDS < deadline )); do
  ghost_json=$(kubectl -n "$gw_ns" exec "$pod" -c chaperone -- \
    cat /var/run/varnish/ghost.json 2>/dev/null || echo '{}')
  if echo "$ghost_json" | jq -e --arg h "$canary_host" '.vhosts[$h]' >/dev/null 2>&1; then
    echo "C05: canary $canary_host resynced into ghost.json"
    exit 0
  fi
  sleep 2
done

echo "C05: canary $canary_host never appeared in ghost.json after partition (no resync)" >&2
exit 1
