#!/usr/bin/env bash
# Snapshot Prometheus metrics from the operator and one chaperone data-plane
# pod. Used by run.sh to track resource leaks and reconcile storms around
# chaos events.
#
# Usage:
#   source metrics-scrape.sh            # (after common.sh)
#   metrics_scrape_start <outdir>       # spawn port-forwards, wait for ready
#   metrics_snapshot <marker>           # write <outdir>/<marker>-{operator,chaperone}.prom
#   metrics_scrape_stop                 # kill port-forwards
#
# Reads common.sh: OPERATOR_NS, OPERATOR_SELECTOR, CHAOS_NS, GATEWAY_NAME.
#
# Port-forward ports are chosen from the 18000s range to avoid colliding
# with the short-lived collector port-forward that run.sh already owns.

_METRICS_OP_PORT=18090
_METRICS_CH_PORT=18091
_METRICS_OP_PF_PID=
_METRICS_CH_PF_PID=
_METRICS_OUTDIR=

metrics_scrape_start() {
  _METRICS_OUTDIR=${1:?outdir required}
  mkdir -p "$_METRICS_OUTDIR"

  local op_ns=${OPERATOR_NS:-varnish-gateway-system}
  local op_sel=${OPERATOR_SELECTOR:-app.kubernetes.io/component=operator}
  local gw_ns=${CHAOS_NS:-varnish-load}
  local gw_name=${GATEWAY_NAME:-load}

  local op_pod ch_pod
  op_pod=$(kubectl -n "$op_ns" get pod -l "$op_sel" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  ch_pod=$(kubectl -n "$gw_ns" get pod \
    -l "gateway.networking.k8s.io/gateway-name=$gw_name" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [[ -z "$op_pod" || -z "$ch_pod" ]]; then
    echo "metrics-scrape: missing pod (operator=$op_pod chaperone=$ch_pod); skipping" >&2
    return 1
  fi
  echo "$ch_pod" >"$_METRICS_OUTDIR/.chaperone-pod"

  kubectl -n "$op_ns" port-forward "pod/$op_pod" \
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
