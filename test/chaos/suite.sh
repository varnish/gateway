#!/usr/bin/env bash
# Run the chaos scenarios end-to-end as one unattended batch.
#
# Designed to be left running overnight: submit, go to bed, come back
# to an aggregated report + per-scenario diagnostic bundles for any
# failures.
#
# Usage:
#   ./test/chaos/suite.sh [--scenarios C01,C04] [--skip C06,C07] [--bail] \
#                         [--out dist/suite-<ts>] [--full]
#
# Defaults:
#   --scenarios  C01,C03,C04,C08,C02,C05,C06,C07   (ordered by blast radius)
#   --skip       none explicit; C06 auto-skips on single-node, C07
#                auto-skips if the TLS fixture isn't present
#   --bail       off; run all scenarios even if one fails
#   --out        dist/suite-<timestamp>
#   --full       include C02 and C05 on single-node kind. Default is to
#                skip them — they're known to stress kind hard.
#
# Env overrides (passed through to run.sh):
#   SCENARIO_TIMEOUT_S  per-scenario wall clock cap (default 900)
#   SUITE_TIMEOUT_S     total cap (default 43200 = 12h)
#   COOLDOWN_S          sleep between scenarios (default 30)
set -euo pipefail

root=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib/common.sh
source "$root/lib/common.sh"
# shellcheck source=lib/health-gate.sh
source "$root/lib/health-gate.sh"
# shellcheck source=lib/bundle.sh
source "$root/lib/bundle.sh"

default_scenarios="C01,C03,C04,C08,C02,C05,C06,C07"
scenarios="$default_scenarios"
skip=""
bail=0
full=0
out_default="dist/suite-$(date -u +%Y%m%dT%H%M%SZ)"
out="$out_default"

while (( $# )); do
  case "$1" in
    --scenarios) scenarios=$2; shift 2 ;;
    --skip)      skip=$2; shift 2 ;;
    --bail)      bail=1; shift ;;
    --full)      full=1; shift ;;
    --out)       out=$2; shift 2 ;;
    -h|--help)   sed -n '2,25p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

: "${SCENARIO_TIMEOUT_S:=900}"
: "${SUITE_TIMEOUT_S:=43200}"
: "${COOLDOWN_S:=30}"

mkdir -p "$out"
suite_log="$out/suite.log"
exec > >(tee -a "$suite_log") 2>&1

echo "==> chaos suite starting at $(date -u +%FT%TZ)"
echo "    scenarios: $scenarios"
echo "    out: $out"

# Detect single-node kind for C06 auto-skip.
node_count=$(kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')
echo "    nodes: $node_count"

has_tls_fixture=0
if kubectl -n "$CHAOS_NS" get certificate load-tls >/dev/null 2>&1; then
  has_tls_fixture=1
fi

should_skip() {
  local id=$1
  # Explicit --skip list wins.
  if [[ ",${skip}," == *",${id},"* ]]; then
    echo "explicit --skip"; return 0
  fi
  case "$id" in
    C06) if (( node_count < 2 )); then echo "single-node cluster"; return 0; fi ;;
    C07) if (( has_tls_fixture == 0 )); then echo "TLS fixture (load-tls) absent"; return 0; fi ;;
    C02|C05)
      if (( full == 0 && node_count < 2 )); then
        echo "oversized for single-node kind (pass --full to include)"
        return 0
      fi
      ;;
  esac
  return 1
}

# Cleanup: kill in-flight chaos CRs and any leftover k6 Jobs if the suite
# is interrupted (or SCENARIO_TIMEOUT_S fires mid-scenario).
cleanup_suite() {
  echo "==> suite cleanup"
  kubectl delete "$CHAOS_KINDS" --all -A --ignore-not-found \
    --wait=false >/dev/null 2>&1 || true
  kubectl -n "$CHAOS_NS" delete job -l app=k6 --ignore-not-found \
    --wait=false >/dev/null 2>&1 || true
}
trap cleanup_suite EXIT

# Initial health gate — don't even start if the cluster is sick.
if ! health_gate; then
  echo "==> initial health gate failed; aborting suite"
  exit 2
fi

IFS=',' read -r -a scenario_list <<<"$scenarios"

# Results accumulator (JSON lines, flattened into suite-report.json at end).
results_jsonl="$out/results.jsonl"
: >"$results_jsonl"

suite_start=$(date +%s)
overall_fail=0

for id in "${scenario_list[@]}"; do
  id=$(echo "$id" | tr -d ' ')
  [[ -z "$id" ]] && continue

  now=$(date +%s)
  if (( now - suite_start > SUITE_TIMEOUT_S )); then
    echo "==> SUITE_TIMEOUT_S reached; aborting remaining scenarios"
    break
  fi

  echo
  echo "================================================================"
  echo "== $id ($(date -u +%FT%TZ))"
  echo "================================================================"

  if reason=$(should_skip "$id"); then
    echo "   SKIP: $reason"
    jq -c -n --arg id "$id" --arg reason "$reason" \
      '{id:$id, result:"SKIP", reason:$reason}' >>"$results_jsonl"
    continue
  fi

  scen_dir="$out/$id"
  mkdir -p "$scen_dir"

  scen_start=$(date +%s)
  set +e
  timeout "$SCENARIO_TIMEOUT_S" bash "$root/run.sh" "$id" \
    >"$scen_dir/run.log" 2>&1
  rc=$?
  set -e
  scen_end=$(date +%s)
  dur=$(( scen_end - scen_start ))

  # Copy per-scenario artifacts produced by run.sh into the suite dir.
  for f in "dist/${id}-report.json" "dist/${id}-ledger.ndjson" "dist/${id}-k6.log"; do
    [[ -f "$f" ]] && cp "$f" "$scen_dir/" || true
  done

  result="PASS"
  reason=""
  if (( rc == 124 )); then
    result="TIMEOUT"
    reason="exceeded SCENARIO_TIMEOUT_S=${SCENARIO_TIMEOUT_S}s"
  elif (( rc != 0 )); then
    result="FAIL"
    reason=$(grep -E '^FAIL:' "$scen_dir/run.log" | head -1 | sed 's/^FAIL: //')
    [[ -z "$reason" ]] && reason="run.sh exit $rc"
  fi

  echo "   $result ($dur s)${reason:+ — $reason}"

  metrics_json='{}'
  if [[ -f "$scen_dir/${id}-report.json" ]]; then
    metrics_json=$(jq -c '{total, drop_ratio, non_2xx, misroutes, convergence}' \
                     "$scen_dir/${id}-report.json" 2>/dev/null || echo '{}')
  fi

  jq -c -n \
    --arg id "$id" \
    --arg result "$result" \
    --arg reason "$reason" \
    --argjson duration_s "$dur" \
    --argjson metrics "$metrics_json" \
    '{id:$id, result:$result, reason:$reason, duration_s:$duration_s, metrics:$metrics}' \
    >>"$results_jsonl"

  if [[ "$result" != "PASS" ]]; then
    overall_fail=1
    bundle_diagnostics "$scen_dir/bundle" "$id"
    if (( bail )); then
      echo "==> --bail set; aborting suite on first failure"
      break
    fi
  fi

  # Post-scenario cooldown + health gate. If the cluster didn't recover,
  # don't trust further results.
  echo "   cooldown ${COOLDOWN_S}s"
  sleep "$COOLDOWN_S"
  if ! health_gate; then
    echo "==> health gate failed after $id; aborting remaining scenarios"
    bundle_diagnostics "$out/post-${id}-bundle" "post-${id}"
    overall_fail=1
    break
  fi
done

# Aggregate final report. jq reads the per-scenario JSONL via --slurpfile
# so we avoid hand-composing JSON (prior versions emitted trailing
# newlines from `date` into string values).
suite_end=$(date +%s)
started_at=$(date -u -r "$suite_start" +%FT%TZ 2>/dev/null || date -u +%FT%TZ)
finished_at=$(date -u +%FT%TZ)

jq -n \
  --arg started_at "$started_at" \
  --arg finished_at "$finished_at" \
  --argjson duration_s "$(( suite_end - suite_start ))" \
  --argjson nodes "$node_count" \
  --slurpfile scenarios "$results_jsonl" \
  '{started_at:$started_at, finished_at:$finished_at, duration_s:$duration_s, nodes:$nodes, scenarios:$scenarios}' \
  >"$out/suite-report.json"

# Human-readable summary.
{
  echo "# Chaos suite: $started_at"
  echo
  printf '%-6s %-8s %8s  %s\n' ID Result Dur Notes
  jq -r '
    . as $s
    | ($s.metrics.total // "") as $total
    | ($s.metrics.drop_ratio // "") as $dr
    | (if ($total|tostring) != "" and ($dr|tostring) != ""
         then "total=\($total) drop_ratio=\($dr)" + (if $s.reason != "" then "; \($s.reason)" else "" end)
         else $s.reason
       end) as $notes
    | "\($s.id)\t\($s.result)\t\($s.duration_s)\t\($notes)"
  ' "$results_jsonl" | while IFS=$'\t' read -r id_ res dur notes; do
    printf '%-6s %-8s %7ss  %s\n' "$id_" "$res" "$dur" "$notes"
  done
  echo
  echo "Artifacts: $out/"
} >"$out/suite-report.md"

echo
echo "==> suite done. Report: $out/suite-report.md"
cat "$out/suite-report.md"

exit $overall_fail
