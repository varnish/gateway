#!/usr/bin/env bash
# Launch the C09 long-horizon soak as an in-cluster Job, plus a parallel
# k6 traffic Job. Survives the launcher's host going offline — both Jobs
# run entirely in the cluster.
#
# Usage:
#   SOAK_HOURS=24 ./test/chaos/launch-soak-cluster.sh
#
# Both Jobs run for SOAK_HOURS. The soak Job's pod stays alive for 24h
# after the run completes (sleep at end of the entrypoint) so kubectl cp
# can retrieve results.
set -euo pipefail

root=$(cd "$(dirname "$0")" && pwd)

# shellcheck source=lib/common.sh
source "$root/lib/common.sh"
# shellcheck source=scenarios/C09.env
source "$root/scenarios/C09.env"
# shellcheck source=lib/k6-job.sh
source "$root/lib/k6-job.sh"

# Public image with bash + kubectl + jq + curl pre-installed (no custom
# image build needed). Override SOAK_IMAGE if you want a different tag.
: "${SOAK_IMAGE:=alpine/k8s:1.31.0}"
: "${SOAK_NAMESPACE:=$CHAOS_NS}"
: "${GATEWAY_URL:=http://${GATEWAY_NAME}.${CHAOS_NS}.svc.cluster.local}"
: "${COLLECTOR_URL:=http://ledger-collector.${CHAOS_NS}.svc.cluster.local:8080}"

duration_s=$(( SOAK_HOURS * 3600 ))
# Deadline = soak duration + post-run cp window + 10min slack. The Job's
# entrypoint sleeps 86400 after the soak so a debug-pod-free `kubectl cp`
# has ~24h. activeDeadlineSeconds <= duration+slack would kill the pod
# right as it enters that sleep, dropping the cp window. (We can still
# recover from the PVC via a transient pod, but the in-place path is
# what `launch-soak-cluster.sh` documents.)
deadline_s=$(( duration_s + 86400 + 600 ))

soak_job_name="soak-$(date +%s)"

echo "==> Building scripts ConfigMap (soak-scripts in $SOAK_NAMESPACE)"
# ConfigMap can't have "/" in keys, so lib/*.sh files become "lib__name.sh"
# and the Job's entrypoint fans them back out into lib/. Same encoding
# trick lib/k6-job.sh uses for the k6 lib/*.js files.
cm_args=(
  --from-file=soak.sh="$root/soak.sh"
  --from-file=C09.env="$root/scenarios/C09.env"
)
for f in "$root"/lib/*.sh; do
  cm_args+=(--from-file="lib__$(basename "$f")=$f")
done
kubectl -n "$SOAK_NAMESPACE" create configmap soak-scripts \
  "${cm_args[@]}" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

echo "==> Submitting soak Job: $soak_job_name"
export SOAK_JOB_NAME=$soak_job_name SOAK_IMAGE SOAK_NAMESPACE
export SOAK_HOURS SNAPSHOT_INTERVAL_S CHURN_INTERVAL_S CHURN_ROUTES
export OPERATOR_NS OPERATOR_SELECTOR CHAOS_NS GATEWAY_NAME
export ACTIVE_DEADLINE_S=$deadline_s

envsubst '$SOAK_JOB_NAME $SOAK_IMAGE $SOAK_NAMESPACE $SOAK_HOURS $SNAPSHOT_INTERVAL_S $CHURN_INTERVAL_S $CHURN_ROUTES $OPERATOR_NS $OPERATOR_SELECTOR $CHAOS_NS $GATEWAY_NAME $ACTIVE_DEADLINE_S' \
  < "$root/manifests/soak-job.yaml" \
  | kubectl apply -f - >/dev/null

echo "==> Submitting k6 traffic Job (${SOAK_HOURS}h continuous load)"
k6_apply_script
k6_job_name=$(k6_job_start "C09" "${SOAK_HOURS}h" 50 20 "$deadline_s")

cat <<EOM

Soak Job:  $soak_job_name (namespace $SOAK_NAMESPACE)
k6 Job:    $k6_job_name (namespace $CHAOS_NS)

Monitor:
  kubectl -n $SOAK_NAMESPACE logs -f job/$soak_job_name
  kubectl -n $CHAOS_NS       logs -f job/$k6_job_name

When the soak completes (~${SOAK_HOURS}h), retrieve results:
  pod=\$(kubectl -n $SOAK_NAMESPACE get pod -l job-name=$soak_job_name -o jsonpath='{.items[0].metadata.name}')
  kubectl -n $SOAK_NAMESPACE cp "\$pod:/data/C09-soak" dist/C09-soak/
  cat dist/C09-soak/soak-report.json | jq

Cleanup when done:
  kubectl -n $SOAK_NAMESPACE delete job/$soak_job_name
  kubectl -n $CHAOS_NS       delete job/$k6_job_name
  kubectl -n $SOAK_NAMESPACE delete pvc/soak-data    # only if you don't want to re-run
EOM
