#!/usr/bin/env bash
# Shared helpers for parsing Prometheus text-format snapshots.
# Sourced by metrics-summary.sh (per-scenario delta path) and soak.sh
# (long-horizon NDJSON path). All three functions accept (file, metric_name)
# and emit the result on stdout; missing file or no match → 0.
#
# Usage:
#   source lib/prom-extract.sh
#   v=$(prom_single foo.prom go_goroutines)
#   v=$(prom_sum    foo.prom controller_runtime_reconcile_total)
#   v=$(prom_sum_float foo.prom controller_runtime_reconcile_time_seconds_sum)

# prom_single: first sample whose series name matches exactly (no labels).
# Use for unlabelled series like go_goroutines, process_open_fds.
prom_single() {
  local f=$1 name=$2
  [[ -f "$f" ]] || { echo 0; return; }
  awk -v name="$name" '
    $0 ~ "^"name"($| )" { v = $NF + 0; found = 1; exit }
    END { if (!found) v = 0; printf "%.0f", v }
  ' "$f"
}

# prom_sum: sum the value field across every sample whose series name
# matches, ignoring labels. Use for labelled counters/gauges like
# controller_runtime_reconcile_total{controller=...,result=...} or
# workqueue_depth{name=...}.
prom_sum() {
  local f=$1 name=$2
  [[ -f "$f" ]] || { echo 0; return; }
  awk -v name="$name" '
    $0 ~ "^"name"($|{| )" {
      v = $NF
      s += v + 0
    }
    END { printf "%.0f", s+0 }
  ' "$f"
}

# prom_sum_float: same as prom_sum but preserves float precision. Use for
# histogram _sum series measured in seconds/bytes.
prom_sum_float() {
  local f=$1 name=$2
  [[ -f "$f" ]] || { echo 0; return; }
  awk -v name="$name" '
    $0 ~ "^"name"($|{| )" { s += $NF + 0 }
    END { printf "%.6f", s+0 }
  ' "$f"
}
