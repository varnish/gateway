#!/usr/bin/env bash
# POST a ledger record with source=chaos to the collector.
# Usage: mark.sh <event> [target]
set -euo pipefail

event=${1:?event required}
target=${2:-}
collector=${COLLECTOR_URL:-http://127.0.0.1:9090}
ts=$(($(date +%s%N) / 1000000))

payload=$(printf '{"source":"chaos","event":"%s","target":"%s","ts":%d}' \
  "$event" "$target" "$ts")

curl -fsS -X POST "$collector/ingest" \
  -H 'Content-Type: application/x-ndjson' \
  --data-binary "$payload"$'\n' >/dev/null
echo "chaos-mark: $event target=$target ts=$ts"
