#!/usr/bin/env bash
# Linear fit (slope + intercept) through an NDJSON time-series for each
# resource metric in the soak snapshot file. One JSON object to stdout:
#
#   {
#     "samples": 37,
#     "duration_min": 180.0,
#     "restart_detected": {"op": false, "ch": false},
#     "op_goroutines":  {"start": 120, "end": 124, "slope_per_min": 0.02, "r2": 0.31},
#     "op_heap_inuse":  {...},
#     "op_workqueue_depth": {...},
#     "op_reconcile_rate_per_min": {...},      # slope of cumulative = rate
#     "op_reconcile_rate_trend":   {...},      # d/dt of per-snapshot rate
#     "op_reconcile_avg_ms_trend": {...},      # d/dt of mean reconcile ms
#     "op_reconcile_errors_per_min": {...},    # slope of cumulative errors
#     ...
#   }
#
# Records with phase="podchange" are skipped. Samples where the requested
# field is null are skipped before fitting (operator/chaperone scrape
# failures don't get coerced to 0).
#
# slope_per_min > 0 with modest r² = real trend. slope near 0 or r² low =
# stable (possibly noisy). Threshold checks live in the caller (soak.sh).
#
# When restart_detected.op or .ch is true, the slopes for that side are
# unreliable (level-shift in the series); the caller should fail loudly
# rather than treat slope thresholds as authoritative.
set -euo pipefail

f=${1:?ndjson file required}
[[ -s "$f" ]] || { echo '{"error":"empty series"}'; exit 1; }

# Filter out marker records (phase="podchange") so they don't pollute fits.
data=$(jq -c 'select(.phase != "podchange")' "$f")

# fit_xy_stream START_END_FMT — read TSV (x_minutes, y) on stdin, emit one
# {start, end, slope_per_min, r2} JSON object. start/end are formatted with
# the supplied printf format (e.g. "%.0f" for raw integer-ish gauges,
# "%.4f" for derived rates).
fit_xy_stream() {
  local fmt=$1
  awk -v fmt="$fmt" '
    { x[NR] = $1 + 0; y[NR] = $2 + 0 }
    END {
      n = NR
      if (n < 2) { print "null"; exit }
      for (i = 1; i <= n; i++) {
        sx += x[i]; sy += y[i]; sxy += x[i]*y[i]
        sxx += x[i]*x[i]; syy += y[i]*y[i]
      }
      denom = sxx - sx*sx/n
      slope = (denom == 0) ? 0 : (sxy - sx*sy/n) / denom
      intercept = (sy/n) - slope * (sx/n)
      ss_tot = syy - sy*sy/n
      ss_res = 0
      for (i = 1; i <= n; i++) {
        yhat = intercept + slope*x[i]
        ss_res += (y[i] - yhat)^2
      }
      r2 = (ss_tot == 0) ? 1.0 : 1 - ss_res/ss_tot
      printf "{\"start\":" fmt ",\"end\":" fmt ",\"slope_per_min\":%.6f,\"r2\":%.3f}",
        y[1], y[n], slope, r2
    }
  '
}

# fit_field FIELD — fit ts vs <field> directly, skipping null samples.
fit_field() {
  local field=$1
  echo "$data" \
    | jq -r --arg f "$field" \
        'select(.[$f] != null) | [.ts, .[$f]] | @tsv' \
    | awk 'NR == 1 { t0 = $1 } { printf "%.6f\t%.6f\n", ($1 - t0)/60000, $2 }' \
    | fit_xy_stream "%.0f"
}

# fit_derived FIELD1 [FIELD2] — fit a per-interval derived series.
# One field: Δfield/Δt per interval (units: field/min).
# Two fields: Δfield1/Δfield2 per interval × 1000 (mean reconcile ms via
# Δsum/Δcount on the controller-runtime histogram).
fit_derived() {
  local field=$1 field2=${2:-}
  if [[ -z "$field2" ]]; then
    echo "$data" \
      | jq -r --arg f "$field" \
          'select(.[$f] != null) | [.ts, .[$f]] | @tsv' \
      | awk '
          { ts[NR] = $1 + 0; y[NR] = $2 + 0 }
          END {
            for (i = 2; i <= NR; i++) {
              dt = (ts[i] - ts[i-1]) / 60000
              if (dt <= 0) continue
              printf "%.6f\t%.6f\n",
                ((ts[i] + ts[i-1]) / 2.0 - ts[1]) / 60000,
                (y[i] - y[i-1]) / dt
            }
          }' \
      | fit_xy_stream "%.4f"
  else
    echo "$data" \
      | jq -r --arg f "$field" --arg g "$field2" \
          'select(.[$f] != null and .[$g] != null) | [.ts, .[$f], .[$g]] | @tsv' \
      | awk '
          { ts[NR] = $1 + 0; a[NR] = $2 + 0; b[NR] = $3 + 0 }
          END {
            for (i = 2; i <= NR; i++) {
              dc = b[i] - b[i-1]
              if (dc <= 0) continue
              printf "%.6f\t%.6f\n",
                ((ts[i] + ts[i-1]) / 2.0 - ts[1]) / 60000,
                (a[i] - a[i-1]) / dc * 1000
            }
          }' \
      | fit_xy_stream "%.4f"
  fi
}

# Restart detection: side restarted mid-soak iff process_start_time differs
# between any two non-null samples (first vs last is sufficient — soak.sh
# fails loudly either way and the .prom snapshots straddle the restart).
restart_detected() {
  local field=$1
  echo "$data" | jq -s --arg f "$field" '
    map(select(.[$f] != null) | .[$f]) as $vs
    | if ($vs | length) < 2 then false else $vs[0] != $vs[-1] end
  '
}

samples=$(echo "$data" | wc -l | tr -d ' ')
# Don't pipe `echo "$data" | jq | head -1` etc.: with set -o pipefail,
# head exiting after the first line closes the pipe and SIGPIPEs jq,
# which fails the assignment once $data exceeds the pipe buffer (~64KB,
# tripped around the 3h soak mark). Read once, slice with bash.
all_ts=$(echo "$data" | jq -r 'select(.ts) | .ts')
first_ts=${all_ts%%$'\n'*}
last_ts=${all_ts##*$'\n'}
duration_min=$(awk -v a="$last_ts" -v b="$first_ts" 'BEGIN{printf "%.1f", (a-b)/60000}')

op_restart=$(restart_detected op_process_start_time)
ch_restart=$(restart_detected ch_process_start_time)

jq -n \
  --argjson samples "$samples" \
  --argjson duration_min "$duration_min" \
  --argjson op_restart "$op_restart" \
  --argjson ch_restart "$ch_restart" \
  --argjson op_goroutines      "$(fit_field op_goroutines)" \
  --argjson op_rss_bytes       "$(fit_field op_rss_bytes)" \
  --argjson op_heap_inuse      "$(fit_field op_heap_inuse)" \
  --argjson op_open_fds        "$(fit_field op_open_fds)" \
  --argjson op_workqueue_depth "$(fit_field op_workqueue_depth)" \
  --argjson op_reconcile_rate_per_min   "$(fit_field op_reconcile_total)" \
  --argjson op_reconcile_rate_trend     "$(fit_derived op_reconcile_total)" \
  --argjson op_reconcile_avg_ms_trend   "$(fit_derived op_reconcile_time_sum op_reconcile_time_count)" \
  --argjson op_reconcile_errors_per_min "$(fit_derived op_reconcile_errors)" \
  --argjson ch_goroutines      "$(fit_field ch_goroutines)" \
  --argjson ch_open_fds        "$(fit_field ch_open_fds)" \
  --argjson ch_rss_bytes       "$(fit_field ch_rss_bytes)" \
  --argjson ch_heap_inuse      "$(fit_field ch_heap_inuse)" \
  --argjson ch_ghost_reload_errors_per_min "$(fit_derived ch_ghost_reload_errors)" \
  '{samples:$samples, duration_min:$duration_min,
    restart_detected:{op:$op_restart, ch:$ch_restart},
    op_goroutines:$op_goroutines,
    op_rss_bytes:$op_rss_bytes,
    op_heap_inuse:$op_heap_inuse,
    op_open_fds:$op_open_fds,
    op_workqueue_depth:$op_workqueue_depth,
    op_reconcile_rate_per_min:$op_reconcile_rate_per_min,
    op_reconcile_rate_trend:$op_reconcile_rate_trend,
    op_reconcile_avg_ms_trend:$op_reconcile_avg_ms_trend,
    op_reconcile_errors_per_min:$op_reconcile_errors_per_min,
    ch_goroutines:$ch_goroutines,
    ch_open_fds:$ch_open_fds,
    ch_rss_bytes:$ch_rss_bytes,
    ch_heap_inuse:$ch_heap_inuse,
    ch_ghost_reload_errors_per_min:$ch_ghost_reload_errors_per_min}'
