#!/usr/bin/env bash
# Linear fit (slope + intercept) through an NDJSON time-series for each
# resource metric in the soak snapshot file. One JSON object to stdout:
#
#   {
#     "samples": 37,
#     "duration_min": 180.0,
#     "op_goroutines":  {"start": 120, "end": 124, "slope_per_min": 0.02, "r2": 0.31},
#     "op_rss_bytes":   {...},
#     "op_open_fds":    {...},
#     "ch_goroutines":  {...},
#     "ch_open_fds":    {...}
#   }
#
# slope_per_min > 0 + modest r² = real trend. slope near 0 or r² low =
# stable (possibly noisy). Threshold checks live in the caller (soak.sh).
set -euo pipefail

f=${1:?ndjson file required}
[[ -s "$f" ]] || { echo '{"error":"empty series"}'; exit 1; }

fit_field() {
  local field=$1
  jq -r --arg field "$field" '[.ts, .[$field]] | @tsv' "$f" | awk '
    {
      ts[NR] = $1 + 0
      y[NR] = $2 + 0
    }
    END {
      if (NR < 2) { print "null"; exit }
      t0 = ts[1]
      n = NR
      for (i = 1; i <= n; i++) {
        x = (ts[i] - t0) / 60000   # minutes since first sample
        sx += x; sy += y[i]; sxy += x*y[i]; sxx += x*x; syy += y[i]*y[i]
      }
      mx = sx / n; my = sy / n
      denom = sxx - sx*sx/n
      slope = (denom == 0) ? 0 : (sxy - sx*sy/n) / denom
      intercept = my - slope * mx
      # R² via sum of squares.
      ss_tot = syy - sy*sy/n
      # predicted ŷ = intercept + slope*x; residuals squared:
      ss_res = 0
      for (i = 1; i <= n; i++) {
        x = (ts[i] - t0) / 60000
        yhat = intercept + slope*x
        ss_res += (y[i] - yhat)^2
      }
      r2 = (ss_tot == 0) ? 1.0 : 1 - ss_res/ss_tot
      printf "{\"start\":%.0f,\"end\":%.0f,\"slope_per_min\":%.6f,\"r2\":%.3f}",
        y[1], y[n], slope, r2
    }
  '
}

samples=$(wc -l <"$f")
first_ts=$(jq -r 'select(.ts) | .ts' "$f" | head -1)
last_ts=$(jq -r 'select(.ts) | .ts' "$f" | tail -1)
duration_min=$(awk -v a="$last_ts" -v b="$first_ts" 'BEGIN{printf "%.1f", (a-b)/60000}')

jq -n \
  --argjson samples "$samples" \
  --argjson duration_min "$duration_min" \
  --argjson op_goroutines "$(fit_field op_goroutines)" \
  --argjson op_rss_bytes  "$(fit_field op_rss_bytes)" \
  --argjson op_open_fds   "$(fit_field op_open_fds)" \
  --argjson ch_goroutines "$(fit_field ch_goroutines)" \
  --argjson ch_open_fds   "$(fit_field ch_open_fds)" \
  '{samples:$samples, duration_min:$duration_min,
    op_goroutines:$op_goroutines, op_rss_bytes:$op_rss_bytes,
    op_open_fds:$op_open_fds,
    ch_goroutines:$ch_goroutines, ch_open_fds:$ch_open_fds}'
