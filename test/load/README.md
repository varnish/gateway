# Load and correctness testing

Primary correctness oracle for the gateway. For each request we log the
intended route and compare it to the backend that actually served it.

## Components

- `echo/` — reflector backend. Returns request details as JSON, sets
  `X-Echo-Service` and `X-Echo-Pod` headers, and POSTs a record to the
  collector.
- `collector/` — receives NDJSON records, appends to a PVC-backed file,
  exposes `/download` for the analyzer and `/metrics` for Prometheus.
- `k6/` — k6 script that generates `X-Trace-ID` per request, records the
  expected backend, and batches ledger POSTs.
- `analyze/` — joins k6 and echo records by trace-ID and reports drops,
  misroutes, duplicates, and convergence latency around chaos events.
- `fixtures/routes.yaml` — minimal `Gateway` + `HTTPRoute` set. Keep in
  sync with `k6/lib/routes.js`.
- `deploy/` — echo and collector manifests.

## Smoke flow

```bash
# Build images and push to your registry (or load into kind/rancher-desktop).
make load-docker

# Apply fixtures, echo backends, and the collector.
make load-up

# Port-forward the gateway and the collector.
kubectl -n varnish-load port-forward svc/ledger-collector 9090:8080 &
kubectl -n varnish-gateway-system port-forward svc/... 8080:80 &

# Run k6 and analyze.
make load-run
make load-analyze
```

`load-analyze` exits non-zero if any drops, misroutes, or duplicates were
detected — CI can gate on it.

## Ledger schema

See `test/load/ledger/record.go`. All sources (`k6`, `echo`, `chaos`)
write the same record type with a `source` tag.

## Known limitations

- k6's per-VU ledger flushes on batch boundaries; the last records of a
  run may be lost if the VU exits mid-batch. Tune `LEDGER_BATCH_SIZE` down
  for short runs.
- Weighted-split correctness is not yet validated. Route → expected
  service is 1:1 in `lib/routes.js`; distributional checks are a follow-up.
- The analyzer classifies a k6 2xx with no echo record as a drop. If the
  echo → collector POST was lost but the request actually hit the backend,
  it will be miscounted. The header-based check (`X-Echo-Service`) catches
  the misroute case independently.
