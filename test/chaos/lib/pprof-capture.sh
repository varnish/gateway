#!/usr/bin/env bash
# pprof_fetch_set BASE_URL OUTDIR SIDE
#
# Fetch the four leak-detection profiles for one process and write them
# under OUTDIR with SIDE-prefixed names. Used by run.sh (per-scenario)
# and soak.sh (long-horizon) when a metric threshold trips, and by
# soak.sh unconditionally on success to give us a baseline.
#
# Profiles fetched:
#   goroutine?debug=2  → <side>-goroutine.txt   (human-readable stacks)
#   goroutine          → <side>-goroutine.pb.gz (binary, for `go tool pprof`)
#   heap               → <side>-heap.pb.gz
#   allocs             → <side>-allocs.pb.gz
#
# Skipped on purpose: profile (30s CPU profile — not a leak signal) and
# trace (large, niche).
#
# Errors are logged but don't abort — partial captures beat nothing.
pprof_fetch_set() {
  local base=$1 outdir=$2 side=$3
  mkdir -p "$outdir"
  local f
  for prof in goroutine heap allocs; do
    f="$outdir/${side}-${prof}.pb.gz"
    if ! curl -sSf -m 15 "${base}/debug/pprof/${prof}" -o "$f" 2>/dev/null; then
      echo "pprof: ${side} ${prof} fetch failed" >&2
      rm -f "$f"
    fi
  done
  f="$outdir/${side}-goroutine.txt"
  if ! curl -sSf -m 15 "${base}/debug/pprof/goroutine?debug=2" -o "$f" 2>/dev/null; then
    echo "pprof: ${side} goroutine?debug=2 fetch failed" >&2
    rm -f "$f"
  fi
}
