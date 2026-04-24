#!/usr/bin/env bash
# Snapshot Prometheus metrics from the operator and one chaperone data-plane
# pod. Used by run.sh to track resource leaks and reconcile storms around
# chaos events.
#
# Usage:
#   source metrics-scrape.sh
#   metrics_scrape_start <outdir>         # spawn port-forwards, wait for ready
#   metrics_snapshot <marker>             # write <outdir>/<marker>-{operator,chaperone}.prom
#   metrics_scrape_stop                   # kill port-forwards
#
# Env overrides:
#   OPERATOR_NAMESPACE  (default: varnish-gateway-system)
#   OPERATOR_DEPLOY     (default: varnish-gateway-operator)
#   GATEWAY_NAMESPACE   (default: varnish-load)
#   GATEWAY_NAME        (default: load)
#
# Port-forward ports are chosen from the 18000s range to avoid colliding
# with the gateway/collector forwards that the caller may already own.

_METRICS_OP_PORT=18090
_METRICS_CH_PORT=18091
_METRICS_OP_PF_PID=
_METRICS_CH_PF_PID=
_METRICS_OUTDIR=

metrics_scrape_start() {
  _METRICS_OUTDIR=${1:?outdir required}
  mkdir -p "$_METRICS_OUTDIR"

  local op_ns=${OPERATOR_NAMESPACE:-varnish-gateway-system}
  local op_deploy=${OPERATOR_DEPLOY:-varnish-gateway-operator}
  local gw_ns=${GATEWAY_NAMESPACE:-varnish-load}
  local gw_name=${GATEWAY_NAME:-load}

  # Pick one data-plane pod — metrics are per-pod; we track a single one.
  local ch_pod
  ch_pod=$(kubectl -n "$gw_ns" get pod \
    -l "gateway.networking.k8s.io/gateway-name=$gw_name" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [[ -z "$ch_pod" ]]; then
    echo "metrics-scrape: no chaperone pod for gateway $gw_name in $gw_ns; skipping" >&2
    return 1
  fi
  echo "$ch_pod" >"$_METRICS_OUTDIR/.chaperone-pod"

  kubectl -n "$op_ns" port-forward "deploy/$op_deploy" \
    "$_METRICS_OP_PORT:8080" >/dev/null 2>&1 &
  _METRICS_OP_PF_PID=$!
  kubectl -n "$gw_ns" port-forward "pod/$ch_pod" \
    "$_METRICS_CH_PORT:8080" >/dev/null 2>&1 &
  _METRICS_CH_PF_PID=$!

  # Wait up to 10s for both endpoints to respond.
  local deadline=$((SECONDS + 10)) op_ok=0 ch_ok=0
  while (( SECONDS < deadline )); do
    (( op_ok )) || curl -sSf -m 1 "http://127.0.0.1:$_METRICS_OP_PORT/metrics" -o /dev/null 2>/dev/null && op_ok=1
    (( ch_ok )) || curl -sSf -m 1 "http://127.0.0.1:$_METRICS_CH_PORT/metrics" -o /dev/null 2>/dev/null && ch_ok=1
    (( op_ok && ch_ok )) && return 0
    sleep 0.2
  done
  echo "metrics-scrape: timed out waiting for metrics endpoints (op=$op_ok ch=$ch_ok)" >&2
  metrics_scrape_stop
  return 1
}

metrics_snapshot() {
  local marker=${1:?marker required}
  [[ -z "$_METRICS_OUTDIR" ]] && return 0
  curl -sS -m 3 "http://127.0.0.1:$_METRICS_OP_PORT/metrics" \
    -o "$_METRICS_OUTDIR/${marker}-operator.prom" || true
  curl -sS -m 3 "http://127.0.0.1:$_METRICS_CH_PORT/metrics" \
    -o "$_METRICS_OUTDIR/${marker}-chaperone.prom" || true
}

metrics_scrape_stop() {
  [[ -n "$_METRICS_OP_PF_PID" ]] && kill "$_METRICS_OP_PF_PID" 2>/dev/null || true
  [[ -n "$_METRICS_CH_PF_PID" ]] && kill "$_METRICS_CH_PF_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  _METRICS_OP_PF_PID=
  _METRICS_CH_PF_PID=
}
