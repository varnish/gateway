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
# shellcheck source=lib/prom-extract.sh
source "$root/lib/prom-extract.sh"
# shellcheck source=lib/pprof-capture.sh
source "$root/lib/pprof-capture.sh"

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

# Track previous pod names so a mid-run rename emits a podchange marker.
op_pod_prev=""
ch_pod_prev=""

# resolve_pod NS LABEL_SELECTOR LABEL_NAME — print the chosen pod name on
# stdout. With more than one match, picks the lexically first and warns to
# stderr (single-replica is the assumed deployment for both sides).
resolve_pod() {
  local ns=$1 sel=$2 label=$3
  local names
  names=$(kubectl -n "$ns" get pod -l "$sel" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
    | sort)
  local count
  count=$(printf '%s\n' "$names" | grep -c .)
  if (( count > 1 )); then
    echo "soak: warning: $count $label pods match selector '$sel'; picking first" >&2
  fi
  printf '%s\n' "$names" | head -n1
}

# null-or-value: emit the metric JSON literal when scrape ok, JSON null
# when not. Used inside scrape() to keep the per-field block readable.
nv() {
  local ok=$1 file=$2 fn=$3 metric=$4
  if (( ok )); then "$fn" "$file" "$metric"; else echo "null"; fi
}

# Snapshot one cycle. Re-resolves pod names per call; writes raw .prom files
# to snapshots/, extracts the metrics we fit on, and appends one NDJSON line.
# Fields are emitted as JSON null when a scrape fails (curl error or empty
# .prom file), so soak-fit.sh can skip rather than treat as zero.
scrape() {
  local phase=$1
  local ts op_file ch_file
  ts=$(date +%s%3N)

  local op_pod ch_pod
  op_pod=$(resolve_pod "$op_ns" "$op_sel" operator || true)
  ch_pod=$(resolve_pod "$gw_ns" "gateway.networking.k8s.io/gateway-name=$gw_name" chaperone || true)

  # Mid-run pod rename → emit a marker, then update prev. (Restart in place,
  # without rename, is detected separately via process_start_time in fit.)
  if [[ -n "$op_pod_prev" && "$op_pod" != "$op_pod_prev" ]]; then
    jq -cn --argjson ts "$ts" --arg side op --arg from "$op_pod_prev" --arg to "$op_pod" \
      '{ts:$ts, phase:"podchange", side:$side, from:$from, to:$to}' \
      >>"$outdir/metrics.ndjson"
  fi
  if [[ -n "$ch_pod_prev" && "$ch_pod" != "$ch_pod_prev" ]]; then
    jq -cn --argjson ts "$ts" --arg side ch --arg from "$ch_pod_prev" --arg to "$ch_pod" \
      '{ts:$ts, phase:"podchange", side:$side, from:$from, to:$to}' \
      >>"$outdir/metrics.ndjson"
  fi
  op_pod_prev=$op_pod
  ch_pod_prev=$ch_pod

  op_file="$outdir/snapshots/op-${ts}.prom"
  ch_file="$outdir/snapshots/ch-${ts}.prom"
  : >"$op_file"; : >"$ch_file"

  # Fetch both in parallel — slow proxy connections otherwise serialize
  # two 5s timeouts back-to-back per snapshot.
  local op_pid="" ch_pid=""
  if [[ -n "$op_pod" ]]; then
    local op_url="http://127.0.0.1:$proxy_port/api/v1/namespaces/$op_ns/pods/$op_pod:metrics/proxy/metrics"
    curl -sSf -m 5 "$op_url" -o "$op_file" 2>/dev/null & op_pid=$!
  fi
  if [[ -n "$ch_pod" ]]; then
    local ch_url="http://127.0.0.1:$proxy_port/api/v1/namespaces/$gw_ns/pods/$ch_pod:metrics/proxy/metrics"
    curl -sSf -m 5 "$ch_url" -o "$ch_file" 2>/dev/null & ch_pid=$!
  fi
  local op_ok=0 ch_ok=0
  [[ -n "$op_pid" ]] && wait "$op_pid" 2>/dev/null && [[ -s "$op_file" ]] && op_ok=1
  [[ -n "$ch_pid" ]] && wait "$ch_pid" 2>/dev/null && [[ -s "$ch_file" ]] && ch_ok=1
  (( op_ok )) || echo "soak: scrape failed: operator (pod=${op_pod:-<none>})" >&2
  (( ch_ok )) || echo "soak: scrape failed: chaperone (pod=${ch_pod:-<none>})" >&2

  local og of ors oh owq orec orsum orcnt oerr ostart
  og=$(nv  $op_ok "$op_file" prom_single     go_goroutines)
  of=$(nv  $op_ok "$op_file" prom_single     process_open_fds)
  ors=$(nv $op_ok "$op_file" prom_single     process_resident_memory_bytes)
  oh=$(nv  $op_ok "$op_file" prom_single     go_memstats_heap_inuse_bytes)
  owq=$(nv $op_ok "$op_file" prom_sum        workqueue_depth)
  orec=$(nv  $op_ok "$op_file" prom_sum        controller_runtime_reconcile_total)
  orsum=$(nv $op_ok "$op_file" prom_sum_float controller_runtime_reconcile_time_seconds_sum)
  orcnt=$(nv $op_ok "$op_file" prom_sum        controller_runtime_reconcile_time_seconds_count)
  oerr=$(nv  $op_ok "$op_file" prom_sum        controller_runtime_reconcile_errors_total)
  ostart=$(nv $op_ok "$op_file" prom_single     process_start_time_seconds)

  local cg cf crss ch_h cgr cgrerr cstart
  cg=$(nv     $ch_ok "$ch_file" prom_single     go_goroutines)
  cf=$(nv     $ch_ok "$ch_file" prom_single     process_open_fds)
  crss=$(nv   $ch_ok "$ch_file" prom_single     process_resident_memory_bytes)
  ch_h=$(nv   $ch_ok "$ch_file" prom_single     go_memstats_heap_inuse_bytes)
  cgr=$(nv    $ch_ok "$ch_file" prom_sum        chaperone_ghost_reloads_total)
  cgrerr=$(nv $ch_ok "$ch_file" prom_sum        chaperone_ghost_reload_errors_total)
  cstart=$(nv $ch_ok "$ch_file" prom_single     process_start_time_seconds)

  jq -cn --argjson ts "$ts" --arg phase "$phase" \
    --argjson og "$og" --argjson of "$of" --argjson ors "$ors" \
    --argjson oh "$oh" --argjson owq "$owq" --argjson orec "$orec" \
    --argjson orsum "$orsum" --argjson orcnt "$orcnt" --argjson oerr "$oerr" \
    --argjson ostart "$ostart" \
    --argjson cg "$cg" --argjson cf "$cf" --argjson crss "$crss" \
    --argjson ch_h "$ch_h" --argjson cgr "$cgr" --argjson cgrerr "$cgrerr" \
    --argjson cstart "$cstart" \
    '{ts:$ts, phase:$phase,
      op_goroutines:$og, op_open_fds:$of, op_rss_bytes:$ors,
      op_heap_inuse:$oh, op_workqueue_depth:$owq,
      op_reconcile_total:$orec,
      op_reconcile_time_sum:$orsum, op_reconcile_time_count:$orcnt,
      op_reconcile_errors:$oerr,
      op_process_start_time:$ostart,
      ch_goroutines:$cg, ch_open_fds:$cf, ch_rss_bytes:$crss,
      ch_heap_inuse:$ch_h,
      ch_ghost_reloads:$cgr, ch_ghost_reload_errors:$cgrerr,
      ch_process_start_time:$cstart}' >>"$outdir/metrics.ndjson"
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

# Mid-run restart of either side level-shifts the series; slopes are
# meaningless. Always fail (regardless of threshold knobs) and tell the
# operator to look at the .prom snapshots around the restart.
op_restart=$(jq -r '.restart_detected.op // false' "$report")
ch_restart=$(jq -r '.restart_detected.ch // false' "$report")
if [[ "$op_restart" == "true" ]]; then
  echo "FAIL: operator restarted mid-soak (process_start_time changed); slopes inconclusive"
  fail=1
fi
if [[ "$ch_restart" == "true" ]]; then
  echo "FAIL: chaperone restarted mid-soak (process_start_time changed); slopes inconclusive"
  fail=1
fi

check_slope() {
  local label=$1 field=$2 limit=$3
  [[ -z "$limit" ]] && return 0
  local v; v=$(jq -r ".${field}.slope_per_min // 0" "$report")
  if awk -v a="$v" -v b="$limit" 'BEGIN{exit !(a+0 > b+0)}'; then
    echo "FAIL: ${label}=${v}/min > ${limit}/min"; fail=1
  fi
}
# Memory / leak indicators
check_slope "op.goroutines"      op_goroutines      "${MAX_OP_GOROUTINE_SLOPE_PER_MIN:-}"
check_slope "op.heap_inuse"      op_heap_inuse      "${MAX_OP_HEAP_SLOPE_PER_MIN:-}"
check_slope "op.rss_bytes"       op_rss_bytes       "${MAX_OP_RSS_SLOPE_PER_MIN:-}"
check_slope "op.open_fds"        op_open_fds        "${MAX_OP_FDS_SLOPE_PER_MIN:-}"
check_slope "ch.goroutines"      ch_goroutines      "${MAX_CH_GOROUTINE_SLOPE_PER_MIN:-}"
check_slope "ch.heap_inuse"      ch_heap_inuse      "${MAX_CH_HEAP_SLOPE_PER_MIN:-}"
check_slope "ch.open_fds"        ch_open_fds        "${MAX_CH_FDS_SLOPE_PER_MIN:-}"
check_slope "ch.rss_bytes"       ch_rss_bytes       "${MAX_CH_RSS_SLOPE_PER_MIN:-}"
# Reconcile health
check_slope "op.workqueue_depth"            op_workqueue_depth          "${MAX_OP_WORKQUEUE_DEPTH_SLOPE_PER_MIN:-}"
check_slope "op.reconcile_errors_per_min"   op_reconcile_errors_per_min "${MAX_OP_RECONCILE_ERROR_RATE_PER_MIN:-}"
check_slope "op.reconcile_rate_trend"       op_reconcile_rate_trend     "${MAX_OP_RECONCILE_RATE_TREND:-}"
check_slope "op.reconcile_avg_ms_trend"     op_reconcile_avg_ms_trend   "${MAX_OP_RECONCILE_AVG_MS_TREND:-}"
check_slope "ch.ghost_reload_errors_per_min" ch_ghost_reload_errors_per_min "${MAX_CH_GHOST_RELOAD_ERROR_RATE_PER_MIN:-}"

capture_pprof() {
  local out=$1
  local op_base="http://127.0.0.1:${proxy_port}/api/v1/namespaces/${op_ns}/pods/${op_pod_prev}:metrics/proxy"
  local ch_base="http://127.0.0.1:${proxy_port}/api/v1/namespaces/${gw_ns}/pods/${ch_pod_prev}:metrics/proxy"
  [[ -n "$op_pod_prev" ]] && pprof_fetch_set "$op_base" "$out" operator
  [[ -n "$ch_pod_prev" ]] && pprof_fetch_set "$ch_base" "$out" chaperone
}

if (( fail )); then
  capture_pprof "$outdir/pprof-fail"
  echo "C09-soak FAILED"
  exit 1
fi

# Unconditional capture on success — gives a baseline pprof to diff against
# future failures.
capture_pprof "$outdir/pprof-final"
echo "C09-soak PASSED"
