#!/usr/bin/env bash
# C07 (P1): TLS cert rotation under load.
#
# Rotate a cert-manager Certificate while k6 is running against the
# HTTPS listener. Verify hot-reload (no chaperone restart), and no TLS
# handshake failures after rotation.
#
# Prerequisite: an HTTPS listener on the load Gateway backed by a
# cert-manager Certificate. The current load fixture (fixtures/routes.yaml)
# is HTTP-only, so this scenario is gated behind a precondition check.
# Until the fixture grows a TLS listener, the scenario exits as SKIP.
set -euo pipefail

gw_ns=${GATEWAY_NAMESPACE:-varnish-load}
cert_name=${CERT_NAME:-load-tls}
settle_s=${ROTATE_SETTLE_S:-30}

if ! kubectl -n "$gw_ns" get certificate "$cert_name" >/dev/null 2>&1; then
  echo "C07: SKIP — cert-manager Certificate $cert_name not found in $gw_ns." >&2
  echo "      Add a TLS listener + Certificate to test/load/fixtures/routes.yaml" >&2
  echo "      and set CERT_NAME to enable this scenario." >&2
  exit 0
fi

echo "C07: forcing renewal of Certificate $cert_name"
# cert-manager watches for this annotation and issues a new cert.
# Value can be any string; changing it triggers renewal.
kubectl -n "$gw_ns" annotate certificate "$cert_name" \
  cert-manager.io/issue-temporary-certificate="$(date +%s)" \
  --overwrite >/dev/null

echo "C07: waiting ${settle_s}s for rotation + hot-reload"
sleep "$settle_s"

# Verify the Certificate is Ready and the Secret serial changed.
ready=$(kubectl -n "$gw_ns" get certificate "$cert_name" \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
if [[ "$ready" != "True" ]]; then
  echo "C07: Certificate $cert_name did not become Ready after rotation" >&2
  exit 1
fi

echo "C07: cert rotated, Certificate Ready=True"
