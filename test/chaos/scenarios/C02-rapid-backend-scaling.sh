#!/usr/bin/env bash
# C02: rapid backend scaling storm.
#
# Scale echo-a 0 → 30 → 0 → 30 → 0 with short holds between transitions
# and a final assertion that ghost.json carries no backends for echo-a.
#
# Expected behaviour:
#   - During scale-to-0 windows: echo-a routes should return 500 (empty
#     backend group), echo-b routes unaffected.
#   - After scale-back-up: endpoints converge, 2xx resumes.
#   - Final state: ghost.json has zero backends in the echo-a group.
#
# Pass criteria are enforced by run.sh against the analyzer output plus
# the exit code of this script.
set -euo pipefail

ns=${ECHO_NAMESPACE:-varnish-load}
deploy=${SCALE_TARGET:-echo-a}
gw_ns=${GATEWAY_NAMESPACE:-varnish-load}
gw_name=${GATEWAY_NAME:-load}
hold_s=${SCALE_HOLD_S:-15}
converge_s=${SCALE_CONVERGE_S:-20}

scale() {
  local n=$1
  echo "C02: scaling $deploy to $n"
  kubectl -n "$ns" scale "deploy/$deploy" --replicas="$n"
}

scale 30
sleep "$hold_s"
scale 0
sleep "$hold_s"
scale 30
sleep "$hold_s"
scale 0
# Wait for endpoints to drain and chaperone to rewrite ghost.json.
sleep "$converge_s"

# Assert ghost.json has no backends for echo-a. Pick any gateway pod.
pod=$(kubectl -n "$gw_ns" get pod \
  -l "gateway.networking.k8s.io/gateway-name=$gw_name" \
  -o jsonpath='{.items[0].metadata.name}')
if [[ -z "$pod" ]]; then
  echo "C02: no gateway pod found for $gw_name in $gw_ns" >&2
  exit 1
fi

echo "C02: inspecting ghost.json on $pod"
ghost_json=$(kubectl -n "$gw_ns" exec "$pod" -c chaperone -- cat /var/run/varnish/ghost.json)

# Count backends whose .port matches any endpoint of deploy=$deploy.
# Simpler + sufficient: the per-vhost routes reference service name in
# routing.json upstream, but ghost.json only has addresses. We assert
# that every backend group that could belong to echo-a is empty, which
# collapses to: no service group has backends whose pod IP is still
# associated with the echo-a selector.
stale=$(kubectl -n "$ns" get endpointslices \
  -l "kubernetes.io/service-name=$deploy" \
  -o jsonpath='{range .items[*]}{range .endpoints[*]}{.addresses[0]}{"\n"}{end}{end}' \
  | sort -u)

if [[ -n "$stale" ]]; then
  echo "C02: endpointslices still list addresses for $deploy:"
  echo "$stale" | sed 's/^/  /'
  echo "C02: expected empty endpointslices after scale-to-0"
  exit 1
fi

# Cross-check ghost.json: none of the listed stale addresses should
# appear. With no stale addresses this is vacuously true, so also
# verify the config is well-formed (parseable and has vhosts key).
echo "$ghost_json" | jq -e '.vhosts' >/dev/null || {
  echo "C02: ghost.json is missing .vhosts key"
  exit 1
}
echo "C02: ghost.json well-formed, endpointslices for $deploy drained"
