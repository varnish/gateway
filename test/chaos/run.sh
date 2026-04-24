#!/usr/bin/env bash
# Run a single chaos scenario end-to-end:
#   1. start k6 in the background
#   2. wait PRE_FAULT_S, mark fault_start
#   3. inject the fault — either a Chaos Mesh CR (scenarios/<ID>-*.yaml)
#      or an action script (scenarios/<ID>-*.sh). The runner picks
#      whichever exists; if both exist, the script wins.
#   4. mark fault_end, wait POST_FAULT_S for convergence
#   5. stop k6, download ledger, run analyzer, check thresholds
#
# Usage: run.sh <SCENARIO_ID>
set -euo pipefail

id=${1:?scenario id required, e.g. C01}
root=$(cd "$(dirname "$0")" && pwd)
env_file=$(ls "$root/scenarios/${id}".env 2>/dev/null || true)
action_script=$(ls "$root/scenarios/${id}"-*.sh 2>/dev/null | head -n1 || true)
cr_file=$(ls "$root/scenarios/${id}"-*.yaml 2>/dev/null | head -n1 || true)

if [[ -z "$env_file" ]]; then
  echo "scenario $id not found (need scenarios/${id}.env)" >&2
  exit 2
fi
if [[ -z "$action_script" && -z "$cr_file" ]]; then
  echo "scenario $id needs either scenarios/${id}-*.sh or scenarios/${id}-*.yaml" >&2
  exit 2
fi

# shellcheck source=/dev/null
source "$env_file"

: "${GATEWAY_URL:?must be set}"
: "${COLLECTOR_URL:?must be set}"
: "${DURATION:?set in scenario env}"
: "${PRE_FAULT_S:?set in scenario env}"
: "${POST_FAULT_S:?set in scenario env}"

mark() { "$root/lib/mark.sh" "$@"; }

# shellcheck source=lib/metrics-scrape.sh
source "$root/lib/metrics-scrape.sh"

outdir=$(mktemp -d)
metrics_dir="$outdir/metrics"

# 1. Start k6 and metric port-forwards.
k6 run \
  -e GATEWAY_URL="$GATEWAY_URL" \
  -e COLLECTOR_URL="$COLLECTOR_URL" \
  -e DURATION="$DURATION" \
  "$root/../load/k6/run.js" >"$outdir/k6.log" 2>&1 &
k6_pid=$!
echo "k6 started (pid=$k6_pid), log=$outdir/k6.log"

metrics_ok=1
metrics_scrape_start "$metrics_dir" || metrics_ok=0

cleanup() {
  metrics_scrape_stop
  if [[ -n "$cr_file" ]]; then
    kubectl delete -f "$cr_file" --ignore-not-found >/dev/null 2>&1 || true
  fi
  kill "$k6_pid" 2>/dev/null || true
  wait "$k6_pid" 2>/dev/null || true
  rm -rf "$outdir"
}
trap cleanup EXIT

# 2. Warm-up, then inject fault.
sleep "$PRE_FAULT_S"

if (( metrics_ok )); then
  metrics_snapshot fault_start
  date +%s%3N >"$metrics_dir/.fault_start_ts"
fi

if [[ -n "$action_script" ]]; then
  mark "${id}_fault_start" "$(basename "$action_script")"
  # Action scripts run synchronously — they own the fault duration.
  # Env and cwd are inherited so they can call kubectl directly.
  bash "$action_script"
  mark "${id}_fault_end" "$(basename "$action_script")"
else
  mark "${id}_fault_start" "$(basename "$cr_file")"
  kubectl apply -f "$cr_file"
  # For CR-based scenarios, the scenario env controls how long the fault
  # stays applied. Default to 5s — fine for one-shot pod-kill.
  sleep "${WAIT_S:-5}"
  kubectl delete -f "$cr_file" --ignore-not-found
  mark "${id}_fault_end" "$(basename "$cr_file")"
fi

if (( metrics_ok )); then
  metrics_snapshot fault_end
  date +%s%3N >"$metrics_dir/.fault_end_ts"
fi

# 3. Let things converge.
sleep "$POST_FAULT_S"
# The scenario_end marker bounds the analyzer's window for this run.
# Its timestamp is the upper bound of [fault_start, scenario_end]; fault_end
# is preserved in place so convergence still measures "first correct response
# after the fault was reverted".
mark "${id}_scenario_end" "end-of-window"

if (( metrics_ok )); then
  metrics_snapshot scenario_end
fi

# 4. Stop k6 and collect.
kill "$k6_pid" 2>/dev/null || true
wait "$k6_pid" 2>/dev/null || true

curl -fsS "$COLLECTOR_URL/download" -o "$outdir/ledger.ndjson"
go run "$root/../load/analyze" -f "$outdir/ledger.ndjson" -scenario "$id" -json >"$outdir/report.json"

# 5. Check thresholds.
drop_ratio=$(jq -r '.drop_ratio // 0' "$outdir/report.json")
misroutes=$(jq -r '.misroutes // 0' "$outdir/report.json")
converge_ms=$(jq -r --arg id "$id" '.convergence[$id + "_fault_end"] // 0' "$outdir/report.json")

fail=0
if awk -v a="$drop_ratio" -v b="${MAX_DROP_RATIO:-1}" 'BEGIN{exit !(a+0 > b+0)}'; then
  echo "FAIL: drop_ratio=$drop_ratio > $MAX_DROP_RATIO"; fail=1
fi
if (( misroutes > ${MAX_MISROUTES:-0} )); then
  echo "FAIL: misroutes=$misroutes > ${MAX_MISROUTES:-0}"; fail=1
fi
if (( converge_ms > ${MAX_CONVERGE_MS:-0} )); then
  echo "FAIL: converge_ms=$converge_ms > ${MAX_CONVERGE_MS:-0}"; fail=1
fi

metrics_summary=
if (( metrics_ok )); then
  metrics_summary=$("$root/lib/metrics-summary.sh" "$metrics_dir" 2>/dev/null || echo "")
fi

# Metrics-based thresholds are opt-in. An unset env var skips the check.
check_metric() {
  local label=$1 path=$2 limit=$3
  [[ -z "$limit" ]] && return 0
  [[ -z "$metrics_summary" ]] && return 0
  local v
  v=$(echo "$metrics_summary" | jq -r "$path // 0")
  if awk -v a="$v" -v b="$limit" 'BEGIN{exit !(a+0 > b+0)}'; then
    echo "FAIL: $label=$v > $limit"; fail=1
  fi
}

check_metric "operator.goroutines_delta"   ".operator.goroutines.delta"  "${MAX_OPERATOR_GOROUTINE_DELTA:-}"
check_metric "operator.workqueue_depth_end" ".operator.workqueue_depth_end" "${MAX_OPERATOR_WORKQUEUE_DEPTH_END:-}"
check_metric "operator.reconcile_rate_hz"   ".operator.reconcile_rate_hz"   "${MAX_OPERATOR_RECONCILE_RATE_HZ:-}"
check_metric "operator.reconcile_avg_ms"    ".operator.reconcile_avg_ms"    "${MAX_OPERATOR_RECONCILE_AVG_MS:-}"
check_metric "operator.reconcile_errors"    ".operator.reconcile_errors"    "${MAX_OPERATOR_RECONCILE_ERRORS:-}"
check_metric "operator.open_fds_delta"      ".operator.open_fds.delta"      "${MAX_OPERATOR_FDS_DELTA:-}"
check_metric "chaperone.goroutines_delta"   ".chaperone.goroutines.delta"   "${MAX_CHAPERONE_GOROUTINE_DELTA:-}"
check_metric "chaperone.ghost_reload_errors" ".chaperone.ghost_reload_errors" "${MAX_CHAPERONE_RELOAD_ERRORS:-}"

mkdir -p dist
cp "$outdir/report.json" "dist/${id}-report.json"
if [[ -n "$metrics_summary" ]]; then
  echo "$metrics_summary" >"dist/${id}-metrics.json"
  mkdir -p "dist/${id}-metrics"
  cp "$metrics_dir"/*.prom "dist/${id}-metrics/" 2>/dev/null || true
fi

if (( fail )); then
  echo "scenario $id FAILED (report: dist/${id}-report.json)"
  exit 1
fi
echo "scenario $id PASSED (drop_ratio=$drop_ratio misroutes=$misroutes converge_ms=$converge_ms)"
