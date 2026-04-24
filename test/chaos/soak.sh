#!/usr/bin/env bash
# C09-soak: long-running leak detector.
#
# Runs a light continuous driver (HTTPRoute churn) for SOAK_HOURS, takes
# metric snapshots every SNAPSHOT_INTERVAL_S, and fits a linear slope
# through each resource metric. Fails if the slope exceeds the scenario's
# thresholds (goroutines/min, RSS bytes/min, etc.).
#
# Differs from run.sh in three ways:
#   1. No fault window — the "fault" is sustained churn for the whole run.
#   2. Many snapshots, not three. Each is summarised into one NDJSON line.
#   3. Analysis is a slope, not a delta. A 3-hour positive trend is the
#      signal, not a big delta at endpoints.
#
# Uses `kubectl proxy` rather than `port-forward` — survives pod
# reschedules and holds up for hours. Requires pods/proxy RBAC (granted
# via the normal user kubeconfig; not via the operator's SA).
#
# Usage:
#   SOAK_HOURS=3 ./test/chaos/soak.sh
set -euo pipefail

root=$(cd "$(dirname "$0")" && pwd)
env_file="$root/scenarios/C09.env"
[[ -f "$env_file" ]] || { echo "C09.env missing at $env_file" >&2; exit 2; }
# shellcheck source=/dev/null
source "$env_file"

: "${SOAK_HOURS:?set in C09.env}"
: "${SNAPSHOT_INTERVAL_S:?}"
: "${CHURN_INTERVAL_S:?}"
: "${CHURN_ROUTES:?}"
: "${GATEWAY_URL:?}"
: "${COLLECTOR_URL:?}"

# shellcheck source=lib/common.sh
source "$root/lib/common.sh"

op_ns=${OPERATOR_NS:-varnish-gateway-system}
op_sel=${OPERATOR_SELECTOR:-app.kubernetes.io/component=operator}
gw_ns=${CHAOS_NS:-varnish-load}
gw_name=${GATEWAY_NAME:-load}
route_ns=${CHAOS_NS:-varnish-load}

outdir="dist/C09-soak"
mkdir -p "$outdir" "$outdir/snapshots"
: >"$outdir/metrics.ndjson"

proxy_port=18001
kubectl proxy --port="$proxy_port" >/dev/null 2>&1 &
proxy_pid=$!

op_pod=$(kubectl -n "$op_ns" get pod -l "$op_sel" \
  -o jsonpath='{.items[0].metadata.name}')
ch_pod=$(kubectl -n "$gw_ns" get pod -l "gateway.networking.k8s.io/gateway-name=$gw_name" \
  -o jsonpath='{.items[0].metadata.name}')

# Track the chaperone pod name — reschedules change it; we re-resolve each snapshot.
scrape() {
  local phase=$1
  local op_url="http://127.0.0.1:$proxy_port/api/v1/namespaces/$op_ns/pods/$op_pod:metrics/proxy/metrics"
  local ch_url
  ch_pod=$(kubectl -n "$gw_ns" get pod -l "gateway.networking.k8s.io/gateway-name=$gw_name" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
  [[ -n "$ch_pod" ]] && ch_url="http://127.0.0.1:$proxy_port/api/v1/namespaces/$gw_ns/pods/$ch_pod:metrics/proxy/metrics"

  local ts op_file ch_file
  ts=$(date +%s%3N)
  op_file="$outdir/snapshots/op-${ts}.prom"
  ch_file="$outdir/snapshots/ch-${ts}.prom"
  curl -sS -m 5 "$op_url" -o "$op_file" || : >"$op_file"
  [[ -n "${ch_url:-}" ]] && curl -sS -m 5 "$ch_url" -o "$ch_file" || : >"$ch_file"

  # One-line extract. Reuses the single_metric awk pattern from metrics-summary.sh.
  local og of ors cg cf
  og=$(awk '$0 ~ /^go_goroutines($| )/ { print $NF+0; exit }' "$op_file")
  of=$(awk '$0 ~ /^process_open_fds($| )/ { print $NF+0; exit }' "$op_file")
  ors=$(awk '$0 ~ /^process_resident_memory_bytes($| )/ { print $NF+0; exit }' "$op_file")
  cg=$(awk '$0 ~ /^go_goroutines($| )/ { print $NF+0; exit }' "$ch_file")
  cf=$(awk '$0 ~ /^process_open_fds($| )/ { print $NF+0; exit }' "$ch_file")
  jq -cn --argjson ts "$ts" --arg phase "$phase" \
    --argjson og "${og:-0}" --argjson of "${of:-0}" --argjson ors "${ors:-0}" \
    --argjson cg "${cg:-0}" --argjson cf "${cf:-0}" \
    '{ts:$ts, phase:$phase, op_goroutines:$og, op_open_fds:$of, op_rss_bytes:$ors,
      ch_goroutines:$cg, ch_open_fds:$cf}' >>"$outdir/metrics.ndjson"
}

# Continuous light traffic — just enough for correctness spot-checks.
k6 run -e GATEWAY_URL="$GATEWAY_URL" -e COLLECTOR_URL="$COLLECTOR_URL" \
  -e RPS="${SOAK_RPS:-20}" -e DURATION="${SOAK_HOURS}h" \
  "$root/../load/k6/run.js" >"$outdir/k6.log" 2>&1 &
k6_pid=$!

# Churn driver in background. Generates and applies/deletes CHURN_ROUTES
# HTTPRoutes every CHURN_INTERVAL_S. Touches both controllers per cycle.
churn_manifest=$(mktemp)
for ((i = 0; i < CHURN_ROUTES; i++)); do
  cat >>"$churn_manifest" <<EOF
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: soak-churn-${i}
  namespace: ${route_ns}
  labels: { chaos-scenario: C09 }
spec:
  parentRefs: [{ name: ${gw_name} }]
  hostnames: ["soak-${i}.load.local"]
  rules:
    - matches: [{ path: { type: PathPrefix, value: "/" } }]
      backendRefs: [{ name: echo-a, port: 8080 }]
EOF
done

churn_loop() {
  while :; do
    kubectl apply -f "$churn_manifest" >/dev/null 2>&1 || true
    sleep "$((CHURN_INTERVAL_S / 2))"
    kubectl delete -f "$churn_manifest" --ignore-not-found >/dev/null 2>&1 || true
    sleep "$((CHURN_INTERVAL_S / 2))"
  done
}
churn_loop &
churn_pid=$!

cleanup() {
  kill "$churn_pid" "$k6_pid" "$proxy_pid" 2>/dev/null || true
  wait 2>/dev/null || true
  kubectl -n "$route_ns" delete httproute -l chaos-scenario=C09 --ignore-not-found >/dev/null 2>&1 || true
  rm -f "$churn_manifest"
}
trap cleanup EXIT

# Snapshot loop — the primary observation path. Everything else is the driver.
echo "C09-soak: running for ${SOAK_HOURS}h, snapshot every ${SNAPSHOT_INTERVAL_S}s"
end_ts=$(( $(date +%s) + SOAK_HOURS * 3600 ))
scrape baseline
while (( $(date +%s) < end_ts )); do
  sleep "$SNAPSHOT_INTERVAL_S"
  scrape soak
done
scrape final

# Fit and threshold-check.
report="$outdir/soak-report.json"
"$root/lib/soak-fit.sh" "$outdir/metrics.ndjson" >"$report"
cat "$report"

fail=0
check_slope() {
  local label=$1 field=$2 limit=$3
  [[ -z "$limit" ]] && return 0
  local v; v=$(jq -r ".${field}.slope_per_min // 0" "$report")
  if awk -v a="$v" -v b="$limit" 'BEGIN{exit !(a+0 > b+0)}'; then
    echo "FAIL: ${label}=${v}/min > ${limit}/min"; fail=1
  fi
}
check_slope "op.goroutines" op_goroutines "${MAX_OP_GOROUTINE_SLOPE_PER_MIN:-}"
check_slope "op.rss_bytes"  op_rss_bytes  "${MAX_OP_RSS_SLOPE_PER_MIN:-}"
check_slope "op.open_fds"   op_open_fds   "${MAX_OP_FDS_SLOPE_PER_MIN:-}"
check_slope "ch.goroutines" ch_goroutines "${MAX_CH_GOROUTINE_SLOPE_PER_MIN:-}"

(( fail )) && { echo "C09-soak FAILED"; exit 1; }
echo "C09-soak PASSED"
