#!/usr/bin/env bash
# Parse Prometheus text-format snapshots into a JSON summary of resource
# leak and reconcile-storm indicators. Reads .prom files produced by
# metrics-scrape.sh and writes a JSON summary to stdout.
#
# Usage:
#   metrics-summary.sh <snapshot_dir>
#
# Expects in <snapshot_dir>:
#   fault_start-operator.prom    fault_start-chaperone.prom
#   fault_end-operator.prom      fault_end-chaperone.prom
#   scenario_end-operator.prom   scenario_end-chaperone.prom
#
# Missing files are non-fatal — their values come out as 0.

set -euo pipefail

dir=${1:?snapshot dir required}

# sum_metric <file> <metric_name>
# Sum all samples whose series name matches exactly (ignores labels).
sum_metric() {
  local f=$1 name=$2
  [[ -f "$f" ]] || { echo 0; return; }
  awk -v name="$name" '
    $0 ~ "^"name"($|{| )" {
      # value is the last field
      v = $NF
      s += v + 0
    }
    END { printf "%.0f", s+0 }
  ' "$f"
}

# As sum_metric, but preserves float precision (for histogram _sum series
# measured in seconds).
sum_metric_float() {
  local f=$1 name=$2
  [[ -f "$f" ]] || { echo 0; return; }
  awk -v name="$name" '
    $0 ~ "^"name"($|{| )" { s += $NF + 0 }
    END { printf "%.6f", s+0 }
  ' "$f"
}

single_metric() {
  local f=$1 name=$2
  [[ -f "$f" ]] || { echo 0; return; }
  awk -v name="$name" '
    $0 ~ "^"name"($| )" { v = $NF + 0; found = 1; exit }
    END { if (!found) v = 0; printf "%.0f", v }
  ' "$f"
}

op_goroutines_start=$(single_metric "$dir/fault_start-operator.prom" go_goroutines)
op_goroutines_end=$(single_metric "$dir/scenario_end-operator.prom" go_goroutines)
op_fds_start=$(single_metric "$dir/fault_start-operator.prom" process_open_fds)
op_fds_end=$(single_metric "$dir/scenario_end-operator.prom" process_open_fds)
op_rss_start=$(single_metric "$dir/fault_start-operator.prom" process_resident_memory_bytes)
op_rss_end=$(single_metric "$dir/scenario_end-operator.prom" process_resident_memory_bytes)

ch_goroutines_start=$(single_metric "$dir/fault_start-chaperone.prom" go_goroutines)
ch_goroutines_end=$(single_metric "$dir/scenario_end-chaperone.prom" go_goroutines)
ch_fds_start=$(single_metric "$dir/fault_start-chaperone.prom" process_open_fds)
ch_fds_end=$(single_metric "$dir/scenario_end-chaperone.prom" process_open_fds)

op_wq_depth_end=$(sum_metric "$dir/scenario_end-operator.prom" workqueue_depth)

op_reconciles_start=$(sum_metric "$dir/fault_start-operator.prom" controller_runtime_reconcile_total)
op_reconciles_end=$(sum_metric "$dir/fault_end-operator.prom"   controller_runtime_reconcile_total)
op_reconcile_errors_start=$(sum_metric "$dir/fault_start-operator.prom" controller_runtime_reconcile_errors_total)
op_reconcile_errors_end=$(sum_metric "$dir/fault_end-operator.prom"     controller_runtime_reconcile_errors_total)

# Histogram: _sum is total seconds spent reconciling, _count is the number of
# reconciles. Δsum / Δcount over the fault window gives mean latency.
op_rtime_sum_start=$(sum_metric_float "$dir/fault_start-operator.prom" controller_runtime_reconcile_time_seconds_sum)
op_rtime_sum_end=$(sum_metric_float   "$dir/fault_end-operator.prom"   controller_runtime_reconcile_time_seconds_sum)
op_rtime_count_start=$(sum_metric "$dir/fault_start-operator.prom" controller_runtime_reconcile_time_seconds_count)
op_rtime_count_end=$(sum_metric   "$dir/fault_end-operator.prom"   controller_runtime_reconcile_time_seconds_count)
op_reconcile_avg_ms=$(awk -v s0="$op_rtime_sum_start" -v s1="$op_rtime_sum_end" \
  -v c0="$op_rtime_count_start" -v c1="$op_rtime_count_end" 'BEGIN{
    dc = c1 - c0
    if (dc <= 0) { print 0; exit }
    printf "%.2f", (s1 - s0) / dc * 1000
  }')

ch_ghost_reloads_start=$(sum_metric "$dir/fault_start-chaperone.prom" chaperone_ghost_reloads_total)
ch_ghost_reloads_end=$(sum_metric "$dir/fault_end-chaperone.prom"     chaperone_ghost_reloads_total)
ch_ghost_reload_errors_start=$(sum_metric "$dir/fault_start-chaperone.prom" chaperone_ghost_reload_errors_total)
ch_ghost_reload_errors_end=$(sum_metric "$dir/fault_end-chaperone.prom"     chaperone_ghost_reload_errors_total)

# Fault-window timestamps: the ledger-based markers (second column of the
# chaos record) aren't available here; instead use wall clock between the
# two snapshots. run.sh writes .start_ts / .end_ts next to the .prom files.
start_ts=$(cat "$dir/.fault_start_ts" 2>/dev/null || echo 0)
end_ts=$(cat   "$dir/.fault_end_ts"   2>/dev/null || echo 0)
fault_seconds=$(awk -v a="$end_ts" -v b="$start_ts" 'BEGIN{d=(a-b)/1000; if(d<0)d=0; printf "%.3f", d}')

rate() {
  local delta=$1
  awk -v d="$delta" -v s="$fault_seconds" 'BEGIN{
    if (s+0 <= 0) { print 0; exit }
    printf "%.2f", d / s
  }'
}

op_reconcile_rate_hz=$(rate $((op_reconciles_end - op_reconciles_start)))
op_reconcile_errors_delta=$((op_reconcile_errors_end - op_reconcile_errors_start))
ch_ghost_reload_rate_hz=$(rate $((ch_ghost_reloads_end - ch_ghost_reloads_start)))
ch_ghost_reload_errors_delta=$((ch_ghost_reload_errors_end - ch_ghost_reload_errors_start))

jq -n \
  --argjson op_goroutines_start "$op_goroutines_start" \
  --argjson op_goroutines_end   "$op_goroutines_end" \
  --argjson op_goroutines_delta "$((op_goroutines_end - op_goroutines_start))" \
  --argjson op_fds_start "$op_fds_start" \
  --argjson op_fds_end   "$op_fds_end" \
  --argjson op_fds_delta "$((op_fds_end - op_fds_start))" \
  --argjson op_rss_start "$op_rss_start" \
  --argjson op_rss_end   "$op_rss_end" \
  --argjson op_rss_delta "$((op_rss_end - op_rss_start))" \
  --argjson op_wq_depth_end "$op_wq_depth_end" \
  --argjson op_reconcile_rate_hz "$op_reconcile_rate_hz" \
  --argjson op_reconcile_avg_ms "$op_reconcile_avg_ms" \
  --argjson op_reconcile_errors_delta "$op_reconcile_errors_delta" \
  --argjson ch_goroutines_start "$ch_goroutines_start" \
  --argjson ch_goroutines_end   "$ch_goroutines_end" \
  --argjson ch_goroutines_delta "$((ch_goroutines_end - ch_goroutines_start))" \
  --argjson ch_fds_start "$ch_fds_start" \
  --argjson ch_fds_end   "$ch_fds_end" \
  --argjson ch_fds_delta "$((ch_fds_end - ch_fds_start))" \
  --argjson ch_ghost_reload_rate_hz "$ch_ghost_reload_rate_hz" \
  --argjson ch_ghost_reload_errors_delta "$ch_ghost_reload_errors_delta" \
  --argjson fault_seconds "$fault_seconds" \
'{
  fault_seconds: $fault_seconds,
  operator: {
    goroutines:        {start: $op_goroutines_start, end: $op_goroutines_end, delta: $op_goroutines_delta},
    open_fds:          {start: $op_fds_start,        end: $op_fds_end,        delta: $op_fds_delta},
    rss_bytes:         {start: $op_rss_start,        end: $op_rss_end,        delta: $op_rss_delta},
    workqueue_depth_end: $op_wq_depth_end,
    reconcile_rate_hz:   $op_reconcile_rate_hz,
    reconcile_avg_ms:    $op_reconcile_avg_ms,
    reconcile_errors:    $op_reconcile_errors_delta
  },
  chaperone: {
    goroutines:        {start: $ch_goroutines_start, end: $ch_goroutines_end, delta: $ch_goroutines_delta},
    open_fds:          {start: $ch_fds_start,        end: $ch_fds_end,        delta: $ch_fds_delta},
    ghost_reload_rate_hz: $ch_ghost_reload_rate_hz,
    ghost_reload_errors:  $ch_ghost_reload_errors_delta
  }
}'
