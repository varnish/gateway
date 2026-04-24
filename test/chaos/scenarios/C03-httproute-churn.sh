#!/usr/bin/env bash
# C03: HTTPRoute churn storm.
#
# Create N synthetic HTTPRoutes in a tight burst, let them settle, then
# delete them in a tight burst. Repeat CYCLES times. The fixture routes
# (a-route, b-route, mixed-a-route, mixed-b-route) must keep serving
# k6 traffic throughout — any data loss on existing routes shows up as
# drops or misroutes in the analyzer.
#
# Expected behaviour:
#   - Fixture routes continue to serve 2xx during the churn.
#   - ConfigMap (routing.json) always contains all fixture routes.
#   - After the final burst + convergence window, zero churn HTTPRoutes
#     remain and routing.json is stable.
set -euo pipefail

ns=${ROUTE_NAMESPACE:-varnish-load}
gw_name=${GATEWAY_NAME:-load}
gw_ns=${GATEWAY_NAMESPACE:-varnish-load}
count=${CHURN_COUNT:-50}
cycles=${CHURN_CYCLES:-3}
between_s=${CHURN_BETWEEN_S:-5}
converge_s=${CHURN_CONVERGE_S:-20}

prefix=c03-churn
manifest=$(mktemp)
trap 'rm -f "$manifest"' EXIT

echo "C03: generating $count HTTPRoutes into $manifest"
: >"$manifest"
for ((i = 0; i < count; i++)); do
  # Alternate backend between echo-a and echo-b so the operator sees
  # heterogeneous routes, not one repeated template.
  if (( i % 2 == 0 )); then svc=echo-a; else svc=echo-b; fi
  cat >>"$manifest" <<EOF
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: ${prefix}-${i}
  namespace: ${ns}
  labels:
    chaos-scenario: C03
spec:
  parentRefs: [{ name: ${gw_name} }]
  hostnames: ["churn-${i}.load.local"]
  rules:
    - matches: [{ path: { type: PathPrefix, value: "/" } }]
      backendRefs:
        - name: ${svc}
          port: 8080
EOF
done

for ((c = 1; c <= cycles; c++)); do
  echo "C03: cycle $c/$cycles — apply burst"
  kubectl apply -f "$manifest" >/dev/null
  sleep "$between_s"
  echo "C03: cycle $c/$cycles — delete burst"
  kubectl delete -f "$manifest" --ignore-not-found >/dev/null
  sleep "$between_s"
done

# Safety net: anything labelled for this scenario must be gone.
kubectl -n "$ns" delete httproute -l chaos-scenario=C03 --ignore-not-found >/dev/null

echo "C03: waiting ${converge_s}s for routing to stabilise"
sleep "$converge_s"

# Assert no leftover churn routes.
leftover=$(kubectl -n "$ns" get httproute -l chaos-scenario=C03 \
  -o jsonpath='{.items[*].metadata.name}')
if [[ -n "$leftover" ]]; then
  echo "C03: leftover churn routes after cleanup: $leftover" >&2
  exit 1
fi

# Assert fixture routes survived the churn by inspecting the gateway's
# ConfigMap directly.
cm_name="${gw_name}-vcl"
routing=$(kubectl -n "$gw_ns" get configmap "$cm_name" \
  -o jsonpath='{.data.routing\.json}')
if [[ -z "$routing" ]]; then
  echo "C03: ConfigMap $cm_name has no routing.json" >&2
  exit 1
fi

missing=()
for host in a.load.local b.load.local mixed.load.local; do
  if ! echo "$routing" | jq -e --arg h "$host" '.vhosts[$h]' >/dev/null; then
    missing+=("$host")
  fi
done
if (( ${#missing[@]} > 0 )); then
  echo "C03: fixture vhosts missing from routing.json: ${missing[*]}" >&2
  echo "      (operator lost data during churn — investigate)" >&2
  exit 1
fi

echo "C03: fixture vhosts present, no leftover churn routes"
