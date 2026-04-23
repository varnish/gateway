#!/usr/bin/env bash
# C08 (P1): simultaneous Gateway + HTTPRoute changes.
#
# Apply an HTTPRoute edit and a Gateway edit in parallel, repeatedly.
# Tests that the two controllers' ConfigMap co-ownership (Gateway owns
# main.vcl, HTTPRoute owns routing.json) actually holds under races —
# neither controller should clobber the other's fields.
#
# Verification:
#   After the burst settles, both the canary HTTPRoute's vhost AND the
#   custom Gateway annotation are observable.
set -euo pipefail

gw_ns=${GATEWAY_NAMESPACE:-varnish-load}
gw_name=${GATEWAY_NAME:-load}
route_ns=${ROUTE_NAMESPACE:-varnish-load}
cycles=${SIMUL_CYCLES:-10}
settle_s=${SIMUL_SETTLE_S:-20}
cm_name="${gw_name}-vcl"

canary=c08-canary
canary_host="c08.load.local"
anno_key="chaos.example.com/c08-epoch"

apply_route() {
  local epoch=$1
  kubectl apply -f - >/dev/null <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: $canary
  namespace: $route_ns
  labels:
    chaos-scenario: C08
    epoch: "$epoch"
spec:
  parentRefs: [{ name: $gw_name }]
  hostnames: ["$canary_host"]
  rules:
    - matches: [{ path: { type: PathPrefix, value: "/" } }]
      backendRefs:
        - name: echo-a
          port: 8080
EOF
}

apply_gw_annotation() {
  local epoch=$1
  kubectl -n "$gw_ns" annotate gateway "$gw_name" \
    "$anno_key=$epoch" --overwrite >/dev/null
}

cleanup() {
  kubectl -n "$route_ns" delete httproute "$canary" --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$gw_ns" annotate gateway "$gw_name" "$anno_key-" --overwrite >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "C08: $cycles parallel apply bursts"
for ((i = 1; i <= cycles; i++)); do
  apply_route "$i" &
  apply_gw_annotation "$i" &
  wait
done

echo "C08: waiting ${settle_s}s for controllers to converge"
sleep "$settle_s"

# Final state checks.
final_epoch=$cycles

# 1. HTTPRoute's vhost lands in routing.json.
routing=$(kubectl -n "$gw_ns" get configmap "$cm_name" \
  -o jsonpath='{.data.routing\.json}' 2>/dev/null || echo '{}')
if ! echo "$routing" | jq -e --arg h "$canary_host" '.vhosts[$h]' >/dev/null; then
  echo "C08: canary vhost $canary_host missing from routing.json" >&2
  echo "     (HTTPRoute controller lost its update under contention)" >&2
  exit 1
fi

# 2. Gateway annotation was preserved.
got=$(kubectl -n "$gw_ns" get gateway "$gw_name" \
  -o jsonpath="{.metadata.annotations.${anno_key//./\\.}}" 2>/dev/null || true)
if [[ "$got" != "$final_epoch" ]]; then
  echo "C08: gateway annotation $anno_key=$got, expected $final_epoch" >&2
  echo "     (Gateway edits lost under contention)" >&2
  exit 1
fi

# 3. main.vcl still exists — the HTTPRoute controller must not have
#    clobbered the Gateway's owned ConfigMap field.
main_vcl=$(kubectl -n "$gw_ns" get configmap "$cm_name" \
  -o jsonpath='{.data.main\.vcl}' 2>/dev/null || echo "")
if [[ -z "$main_vcl" ]]; then
  echo "C08: main.vcl missing from $cm_name — Gateway controller's field was clobbered" >&2
  exit 1
fi

echo "C08: both controllers retained their ConfigMap fields under contention"
