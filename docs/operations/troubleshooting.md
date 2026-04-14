# Troubleshooting

This page describes how to get the information you need to diagnose
problems with a Varnish Gateway installation. The list of known failure
modes will grow as the project matures.

## Getting logs

Most problems surface in one of three log streams. Start here before
digging deeper.

### Operator logs

The operator reconciles Gateway, HTTPRoute, and GatewayClassParameters
resources. Errors during reconciliation — missing references, invalid
VCL, failed resource creation — appear here:

```bash
kubectl logs -n varnish-gateway-system -l app.kubernetes.io/component=operator -f
```

### Chaperone logs

Chaperone manages the varnishd process inside each gateway pod. Reload
failures, endpoint resolution problems, and varnishd crashes appear
here:

```bash
kubectl logs -f <gateway-pod> -c varnish-gateway
```

### Varnish request logs

For request-level debugging — cache misses, backend errors, unexpected
routing — use varnishlog inside the pod:

```bash
kubectl exec -it <gateway-pod> -c varnish-gateway -- \
  varnishlog -n /var/run/varnish/vsm -g request
```

Filter to specific status codes, URLs, or backends with `-q`:

```bash
# 503 errors
kubectl exec -it <gateway-pod> -c varnish-gateway -- \
  varnishlog -n /var/run/varnish/vsm -q "RespStatus == 503" -g request

# Backend-side 5xx
kubectl exec -it <gateway-pod> -c varnish-gateway -- \
  varnishlog -n /var/run/varnish/vsm -q "BerespStatus >= 500" -g request
```

See [guides/logging.md](../guides/logging.md) for the full set of
varnishlog flags and query syntax.

### Dashboard

The built-in dashboard gives a live view of ghost's routing state,
recent reload events, and filtered varnishlog output — without needing
kubectl exec. Port-forward to reach it:

```bash
kubectl port-forward <gateway-pod> 9000:9000
```

Then open `http://localhost:9000`. The `/api/state` endpoint returns a
JSON snapshot of the current vhost configuration, backend health, and
recent events. The `/api/varnishlog` endpoint streams filtered
varnishlog-json over Server-Sent Events. See
[observability.md](observability.md#dashboard) for the full endpoint
list.

## Checking resource status

Gateway API resources carry status conditions that surface problems
without needing logs:

```bash
# Gateway status — look for Accepted and Programmed conditions
kubectl describe gateway <name>

# HTTPRoute status — look for Accepted and ResolvedRefs conditions
kubectl describe httproute <name>

# GatewayClass status
kubectl describe gatewayclass varnish
```

A Gateway stuck in `Programmed=False` or an HTTPRoute with
`ResolvedRefs=False` usually indicates a missing Secret, Service, or
ReferenceGrant. The status message includes specifics.

## Known failure modes

### Pods crashlooping

Check the chaperone logs for the crash reason. Possible causes:

- Invalid VCL syntax in user VCL — chaperone logs the `vcl.load` error from varnishadm.
- Missing or unreadable ConfigMap mount — the pod starts but varnishd cannot load its configuration.
- Insufficient memory — if a memory limit is set too close to the configured storage size, the kernel OOM-kills varnishd. Check `kubectl describe pod` for OOMKilled status.

### Reloads not taking effect

Both VCL and ghost reloads use content-based deduplication — if the
generated content is byte-identical to the previous version, no reload
is issued. Check the chaperone logs for reload events and errors. The
Prometheus counters `chaperone_ghost_reloads_total` and
`chaperone_vcl_reloads_total` confirm whether reloads are happening.

A failed VCL reload leaves the previous VCL active. The error is logged
and the gateway continues serving with the last good configuration.

### TLS handshake failures

Check that the referenced Secret exists, is of type `kubernetes.io/tls`,
and contains valid PEM in `tls.crt` and `tls.key`. For cross-namespace
certificate references, verify that a ReferenceGrant exists in the
Secret's namespace. See [guides/tls.md](../guides/tls.md) for the full
TLS setup.

## See also

- [Observability](observability.md) — health endpoints, metrics, dashboard, and log access
- [Logging guide](../guides/logging.md) — sidecar and ad-hoc varnishlog usage
- [TLS guide](../guides/tls.md)
