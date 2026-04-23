#!/usr/bin/env bash
# C06 (P1): node drain with gateway pods running.
#
# Pick a node currently running a gateway pod, cordon + drain it, and
# verify the pod reschedules to another node while k6 traffic is in
# flight. Requires ≥2 schedulable nodes; on single-node clusters the
# scenario exits with a clear skip.
#
# Verification:
#   1. Gateway deployment regains the target replica count on a
#      different node within RESCHEDULE_S.
#   2. Drained node is uncordoned at the end so the cluster is clean.
set -euo pipefail

gw_ns=${GATEWAY_NAMESPACE:-varnish-load}
gw_name=${GATEWAY_NAME:-load}
reschedule_s=${RESCHEDULE_S:-90}
drain_timeout=${DRAIN_TIMEOUT_S:-60}

# Need at least two schedulable nodes. Filter out control-plane-only
# nodes when the distro marks them unschedulable already.
nodes=$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name} {.spec.unschedulable}{"\n"}{end}' \
  | awk '$2 != "true" {print $1}')
node_count=$(echo "$nodes" | grep -c . || true)
if (( node_count < 2 )); then
  echo "C06: SKIP — need ≥2 schedulable nodes, have $node_count" >&2
  exit 0
fi

# Find a node hosting at least one gateway pod.
target_node=$(kubectl -n "$gw_ns" get pod \
  -l "gateway.networking.k8s.io/gateway-name=$gw_name" \
  -o jsonpath='{.items[0].spec.nodeName}')
if [[ -z "$target_node" ]]; then
  echo "C06: no gateway pods found for $gw_name in $gw_ns" >&2
  exit 1
fi
echo "C06: draining $target_node (hosts gateway pod)"

uncordon() {
  kubectl uncordon "$target_node" >/dev/null 2>&1 || true
}
trap uncordon EXIT

kubectl drain "$target_node" \
  --ignore-daemonsets \
  --delete-emptydir-data \
  --force \
  --timeout="${drain_timeout}s" >/dev/null

echo "C06: waiting up to ${reschedule_s}s for gateway pods to reschedule"
deadline=$((SECONDS + reschedule_s))
while (( SECONDS < deadline )); do
  # All gateway pods are Ready AND none are on the drained node.
  pods_json=$(kubectl -n "$gw_ns" get pod \
    -l "gateway.networking.k8s.io/gateway-name=$gw_name" \
    -o json)
  not_ready=$(echo "$pods_json" | jq '[.items[] | select([.status.conditions[]? | select(.type=="Ready" and .status=="True")] | length == 0)] | length')
  on_drained=$(echo "$pods_json" | jq --arg n "$target_node" '[.items[] | select(.spec.nodeName == $n)] | length')
  if (( not_ready == 0 && on_drained == 0 )); then
    echo "C06: gateway pods rescheduled off $target_node and all Ready"
    break
  fi
  sleep 3
done

# Re-check after the loop.
pods_json=$(kubectl -n "$gw_ns" get pod \
  -l "gateway.networking.k8s.io/gateway-name=$gw_name" \
  -o json)
not_ready=$(echo "$pods_json" | jq '[.items[] | select([.status.conditions[]? | select(.type=="Ready" and .status=="True")] | length == 0)] | length')
on_drained=$(echo "$pods_json" | jq --arg n "$target_node" '[.items[] | select(.spec.nodeName == $n)] | length')
if (( not_ready > 0 || on_drained > 0 )); then
  echo "C06: reschedule failed — not_ready=$not_ready on_drained=$on_drained after ${reschedule_s}s" >&2
  exit 1
fi

echo "C06: reschedule complete; uncordoning $target_node"
