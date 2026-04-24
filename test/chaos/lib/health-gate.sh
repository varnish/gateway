#!/usr/bin/env bash
# health_gate — block until the cluster is in a known-good baseline so
# the next scenario starts from a reproducible state. Returns 0 on
# success, non-zero on timeout (echoes which check failed).
#
# Env overrides:
#   HEALTH_TIMEOUT_S (default 180)
#   plus CHAOS_NS / GATEWAY_NAME / OPERATOR_NS / OPERATOR_SELECTOR
#   (see lib/common.sh)

health_gate() {
  local timeout=${HEALTH_TIMEOUT_S:-180}
  local deadline=$(( $(date +%s) + timeout ))
  local reason=""

  while (( $(date +%s) < deadline )); do
    if ! kubectl -n "$CHAOS_NS" get "gateway/$GATEWAY_NAME" \
         -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null \
         | grep -q '^True$'; then
      reason="gateway/$GATEWAY_NAME not Programmed"
    elif ! kubectl -n "$OPERATOR_NS" get deploy -l "$OPERATOR_SELECTOR" \
           -o jsonpath='{.items[*].status.availableReplicas}' 2>/dev/null \
           | grep -qE '^[1-9]'; then
      reason="operator deployment not Available"
    elif ! kubectl -n "$CHAOS_NS" get pod \
           -l "gateway.networking.k8s.io/gateway-name=$GATEWAY_NAME" \
           -o jsonpath='{.items[*].status.containerStatuses[*].ready}' 2>/dev/null \
           | grep -q 'true'; then
      reason="chaperone pods not Ready"
    elif [[ -n "$(kubectl get "$CHAOS_KINDS" -A --no-headers 2>/dev/null)" ]]; then
      reason="residual Chaos Mesh CRs present"
    elif ! kubectl -n "$CHAOS_NS" get endpointslices \
           -l kubernetes.io/service-name=echo-a --no-headers 2>/dev/null \
           | grep -q .; then
      reason="no endpointslices for echo-a"
    else
      return 0
    fi
    sleep 3
  done

  echo "health_gate: timeout after ${timeout}s — $reason" >&2
  return 1
}
