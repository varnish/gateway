#!/usr/bin/env bash
# bundle_diagnostics — capture post-mortem data for a failed scenario so
# the user has something to look at the next morning. Never fails: each
# section is best-effort.
#
# Usage: bundle_diagnostics <dst_dir> <scenario_id>

bundle_diagnostics() {
  local dst=$1 id=$2
  mkdir -p "$dst"

  echo "# bundle for $id at $(date -u +%FT%TZ)" >"$dst/bundle.meta"

  kubectl -n "$OPERATOR_NS" logs -l "$OPERATOR_SELECTOR" \
    --all-containers --tail=2000 >"$dst/operator-logs.txt" 2>&1 || true

  for pod_ref in $(kubectl -n "$CHAOS_NS" get pod \
                 -l "gateway.networking.k8s.io/gateway-name=$GATEWAY_NAME" \
                 -o name 2>/dev/null); do
    local pname=${pod_ref##*/}
    kubectl -n "$CHAOS_NS" logs "$pod_ref" --all-containers --tail=2000 \
      >"$dst/chaperone-${pname}.txt" 2>&1 || true
  done

  kubectl -n chaos-mesh logs -l app.kubernetes.io/component=controller-manager \
    --tail=500 >"$dst/chaos-controller.txt" 2>&1 || true

  kubectl -n "$CHAOS_NS" describe "gateway/$GATEWAY_NAME" \
    >"$dst/describe-gateway.txt" 2>&1 || true
  kubectl -n "$CHAOS_NS" describe httproute \
    >"$dst/describe-httproutes.txt" 2>&1 || true

  kubectl -n "$CHAOS_NS" get "configmap/${GATEWAY_NAME}-vcl" -o yaml \
    >"$dst/configmap-${GATEWAY_NAME}-vcl.yaml" 2>&1 || true

  local first_pod
  first_pod=$(kubectl -n "$CHAOS_NS" get pod \
         -l "gateway.networking.k8s.io/gateway-name=$GATEWAY_NAME" \
         -o name 2>/dev/null | head -1)
  if [[ -n "$first_pod" ]]; then
    kubectl -n "$CHAOS_NS" exec "$first_pod" -c chaperone -- \
      cat /var/run/varnish/ghost.json >"$dst/ghost.json" 2>&1 || true
  fi

  kubectl get events -A --sort-by=.lastTimestamp \
    >"$dst/events.txt" 2>&1 || true
  kubectl get pods -A -o wide >"$dst/pods.txt" 2>&1 || true
  kubectl get "$CHAOS_KINDS" -A -o yaml \
    >"$dst/chaos-resources.yaml" 2>&1 || true
}
