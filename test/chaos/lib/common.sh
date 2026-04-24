#!/usr/bin/env bash
# Shared constants and helpers for the chaos harness. Sourced by run.sh,
# suite.sh, and the other lib/*.sh files. Must not call `set -e` etc —
# that's the caller's decision.

# Cluster topology — override via env if you stand the load suite up
# somewhere else.
: "${CHAOS_NS:=varnish-load}"
: "${GATEWAY_NAME:=load}"
: "${OPERATOR_NS:=varnish-gateway-system}"
: "${OPERATOR_SELECTOR:=app.kubernetes.io/component=operator}"
export CHAOS_NS GATEWAY_NAME OPERATOR_NS OPERATOR_SELECTOR

# Comma-separated list of Chaos Mesh kinds we enumerate for cleanup /
# residual checks. Keeping this in one place means adding a new kind is
# a single-file change. Use with kubectl as "kubectl get $CHAOS_KINDS ...".
CHAOS_KINDS=podchaos,networkchaos,stresschaos,iochaos,timechaos,httpchaos
export CHAOS_KINDS

# duration_to_seconds <string> -> seconds (echo to stdout)
# Accepts 2h / 90m / 30s / plain number. Rejects anything else.
duration_to_seconds() {
  local d=$1 body suffix=${1: -1}
  case "$suffix" in
    h|m|s) body=${d%?} ;;
    *)     body=$d suffix= ;;
  esac
  if [[ -z "$body" || "$body" =~ [^0-9] ]]; then
    echo "duration_to_seconds: cannot parse '$d'" >&2
    return 2
  fi
  case "$suffix" in
    h) echo $(( body * 3600 )) ;;
    m) echo $(( body * 60 )) ;;
    *) echo "$body" ;;
  esac
}
