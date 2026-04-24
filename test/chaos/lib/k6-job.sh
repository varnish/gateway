#!/usr/bin/env bash
# Helpers for running k6 as an in-cluster Job instead of as a host
# process. Sourced by run.sh.
#
# kubectl port-forward is a single TCP stream through the apiserver and
# caps at a few tens of MB/s under load, so an external k6 limits the
# RPS we can push through the data plane. Running k6 as a Job on the
# cluster network hits the Varnish Service directly via ClusterIP.
#
# Public functions:
#   k6_apply_script   (re)create k6-script ConfigMap from test/load/k6/
#   k6_job_start      render manifests/k6-job.yaml, apply, echo Job name
#   k6_job_await_ready   wait for Job pod Ready; surface describe on fail
#   k6_job_wait       wait for Complete; surface describe+logs on fail
#   k6_job_logs       kubectl logs into a file
#   k6_job_cleanup    idempotent delete

: "${K6_IMAGE:=grafana/k6:0.49.0}"

_k6_script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../load/k6" && pwd)
_k6_manifest=$(cd "$(dirname "${BASH_SOURCE[0]}")/../manifests" && pwd)/k6-job.yaml

k6_apply_script() {
  kubectl -n "$CHAOS_NS" create configmap k6-script \
    --from-file=run.js="$_k6_script_dir/run.js" \
    --from-file=lib__ledger.js="$_k6_script_dir/lib/ledger.js" \
    --from-file=lib__routes.js="$_k6_script_dir/lib/routes.js" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
}

k6_job_start() {
  local scenario=$1 duration=$2 vus=${3:-50} rps=${4:-0} deadline=${5:-600}
  local lc_scenario
  lc_scenario=$(echo "$scenario" | tr '[:upper:]' '[:lower:]')
  local name="k6-${lc_scenario}-$(date +%s)"

  export K6_JOB_NAME="$name" K6_IMAGE SCENARIO="$scenario"
  export GATEWAY_URL COLLECTOR_URL
  export DURATION="$duration" VUS="$vus" RPS="$rps"
  export ACTIVE_DEADLINE_S="$deadline"

  # Allowlist the vars we substitute so stray $FOO references in the
  # manifest (or future ones) aren't silently expanded.
  envsubst '$K6_JOB_NAME $K6_IMAGE $SCENARIO $CHAOS_NS $GATEWAY_URL $COLLECTOR_URL $DURATION $VUS $RPS $ACTIVE_DEADLINE_S' \
    < "$_k6_manifest" | kubectl apply -f - >/dev/null
  echo "$name"
}

k6_job_await_ready() {
  local name=$1 timeout=${2:-60}
  if kubectl -n "$CHAOS_NS" wait --for=condition=Ready pod \
       -l "job-name=$name" --timeout="${timeout}s" >/dev/null 2>&1; then
    return 0
  fi
  echo "k6_job_await_ready: $name never Ready" >&2
  kubectl -n "$CHAOS_NS" describe pod -l "job-name=$name" >&2 || true
  return 1
}

k6_job_wait() {
  local name=$1 timeout=${2:-600}
  if kubectl -n "$CHAOS_NS" wait --for=condition=complete \
       "job/$name" --timeout="${timeout}s" >/dev/null 2>&1; then
    return 0
  fi
  local failed
  failed=$(kubectl -n "$CHAOS_NS" get "job/$name" \
    -o jsonpath='{.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null || true)
  {
    echo "---- k6 Job $name did not complete (failed=$failed) ----"
    kubectl -n "$CHAOS_NS" describe "job/$name" 2>&1 | tail -30
    echo "---- logs ----"
    kubectl -n "$CHAOS_NS" logs "job/$name" --tail=200 2>&1 || true
  } >&2
  return 1
}

k6_job_logs() {
  kubectl -n "$CHAOS_NS" logs "job/$1" >"$2" 2>&1 || true
}

k6_job_cleanup() {
  kubectl -n "$CHAOS_NS" delete "job/$1" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
