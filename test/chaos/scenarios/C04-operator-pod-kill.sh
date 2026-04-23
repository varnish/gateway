#!/usr/bin/env bash
# C04: operator pod kill mid-reconciliation.
#
# Apply a canary HTTPRoute to trigger reconcile activity, then kill the
# operator pod while it is still processing. Verify that:
#   1. the operator restarts cleanly
#   2. the pre-kill canary is reflected in routing.json (the reconcile
#      that was in flight either completed or was safely retried)
#   3. a post-recovery canary is also reconciled correctly (no
#      partial state blocking the next reconcile)
#   4. deleting both canaries leaves no trace in routing.json
set -euo pipefail

op_ns=${OPERATOR_NAMESPACE:-varnish-gateway-system}
op_selector=${OPERATOR_SELECTOR:-app.kubernetes.io/component=operator}
route_ns=${ROUTE_NAMESPACE:-varnish-load}
gw_name=${GATEWAY_NAME:-load}
gw_ns=${GATEWAY_NAMESPACE:-varnish-load}
cm_name="${gw_name}-vcl"

pre_host="c04-pre.load.local"
post_host="c04-post.load.local"

apply_route() {
  local name=$1 host=$2
  kubectl apply -f - >/dev/null <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: $name
  namespace: $route_ns
  labels:
    chaos-scenario: C04
spec:
  parentRefs: [{ name: $gw_name }]
  hostnames: ["$host"]
  rules:
    - matches: [{ path: { type: PathPrefix, value: "/" } }]
      backendRefs:
        - name: echo-a
          port: 8080
EOF
}

wait_for_vhost() {
  local host=$1 want=$2 timeout=${3:-30}
  local deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    local routing
    routing=$(kubectl -n "$gw_ns" get configmap "$cm_name" \
      -o jsonpath='{.data.routing\.json}' 2>/dev/null || echo '{}')
    local present
    if echo "$routing" | jq -e --arg h "$host" '.vhosts[$h]' >/dev/null 2>&1; then
      present=1
    else
      present=0
    fi
    if [[ "$want" == "present" && "$present" == "1" ]]; then return 0; fi
    if [[ "$want" == "absent"  && "$present" == "0" ]]; then return 0; fi
    sleep 1
  done
  echo "C04: timed out waiting for $host to be $want in $cm_name" >&2
  return 1
}

cleanup() {
  kubectl -n "$route_ns" delete httproute -l chaos-scenario=C04 --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

# 1. Pre-kill canary — asks the operator to reconcile.
echo "C04: applying pre-kill canary $pre_host"
apply_route c04-pre "$pre_host"

# 2. Kill as quickly as possible to overlap with the reconcile.
echo "C04: killing operator pod(s) matching $op_selector in $op_ns"
kubectl -n "$op_ns" delete pod -l "$op_selector" --grace-period=0 --force >/dev/null

# 3. Wait for rollout recovery.
echo "C04: waiting for operator rollout"
kubectl -n "$op_ns" rollout status deploy \
  -l "$op_selector" --timeout=60s

# 4. Verify pre-kill canary eventually lands (the reconcile either
#    completed before the kill or the new pod retried it).
echo "C04: verifying pre-kill canary converged into routing.json"
wait_for_vhost "$pre_host" present 60

# 5. Post-recovery canary — proves next reconcile works with no
#    partial state blocking it.
echo "C04: applying post-recovery canary $post_host"
apply_route c04-post "$post_host"
wait_for_vhost "$post_host" present 30

# 6. Clean up and confirm both are gone.
echo "C04: deleting canaries"
kubectl -n "$route_ns" delete httproute c04-pre c04-post --ignore-not-found >/dev/null
wait_for_vhost "$pre_host" absent 30
wait_for_vhost "$post_host" absent 30

echo "C04: operator recovered cleanly, pre- and post-kill reconciles both succeeded"
