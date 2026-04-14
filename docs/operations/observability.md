# Observability

This page covers the signals available from a running Varnish Gateway:
health endpoints, Prometheus metrics, the built-in dashboard, and log
access.

## Health endpoints

Chaperone exposes HTTP endpoints on the health address, port 8080 by
default via the `HEALTH_ADDR` environment variable.

| Path              | Method | Description                                                                                           |
| ----------------- | ------ | ----------------------------------------------------------------------------------------------------- |
| `/health`         | GET    | Returns 200 when the pod is ready, 503 when not ready or draining.                                    |
| `/drain`          | GET    | Initiates graceful shutdown. The pod stops accepting new connections.                                 |
| `/debug/backends` | GET    | Exposes `varnishadm backend.list` output. Accepts `format=json` and `detailed=true` query parameters. |
| `/metrics`        | GET    | Prometheus metrics endpoint.                                                                          |

The `/health` endpoint is used by the Kubernetes readiness probe. A pod
becomes ready after the initial VCL load and the first ghost reload both
complete. On `SIGTERM` the pod enters draining state and `/health`
returns 503, giving the Service time to remove the pod from the
endpoint list before connections close.

## Prometheus metrics

### Chaperone metrics

Chaperone registers its own metrics on the `/metrics` endpoint alongside
Go runtime and process collectors.

#### Counters

| Metric                                | Description                     |
| ------------------------------------- | ------------------------------- |
| `chaperone_ghost_reloads_total`       | Ghost reload attempts           |
| `chaperone_ghost_reload_errors_total` | Failed ghost reloads            |
| `chaperone_vcl_reloads_total`         | VCL hot-reload attempts         |
| `chaperone_vcl_reload_errors_total`   | Failed VCL reloads              |
| `chaperone_tls_reloads_total`         | TLS certificate reload attempts |
| `chaperone_tls_reload_errors_total`   | Failed TLS reloads              |
| `chaperone_endpoint_changes_total`    | EndpointSlice change events     |

#### Gauges

| Metric               | Description                          |
| -------------------- | ------------------------------------ |
| `chaperone_ready`    | 1 when the pod is ready, 0 otherwise |
| `chaperone_draining` | 1 when the pod is draining           |

### Varnish metrics

Chaperone runs `varnishstat -1 -j` periodically and exposes each counter
as a Prometheus metric. Counter names are lowercased with dots replaced
by underscores — for example, `MAIN.cache_hit` becomes
`varnish_main_cache_hit`. Varnishstat counters flagged as cumulative
are exposed as Prometheus counters; all others as gauges.

### Operator metrics

The operator exposes controller-runtime metrics on its own metrics
address, port 8080 by default. These include reconciliation latency,
work queue depth, and error counts — standard controller-runtime metrics
documented upstream.

## Dashboard

Chaperone includes an embedded dashboard, enabled by default on port
9000 inside the container. It is not exposed outside the pod via the
Service, but you can reach it with `kubectl port-forward`:

```bash
kubectl port-forward <gateway-pod> 9000:9000
```

Then open `http://localhost:9000` in a browser.

The dashboard exposes:

| Path              | Description                                                                              |
| ----------------- | ---------------------------------------------------------------------------------------- |
| `/`               | HTML dashboard UI                                                                        |
| `/api/state`      | JSON snapshot: ready/draining state, uptime, vhosts, services, recent events             |
| `/api/events`     | Server-Sent Events stream of reload, endpoint, and state-change events                   |
| `/api/varnishlog` | Server-Sent Events stream of live varnishlog-json output, with query parameter filtering |

The `/api/varnishlog` endpoint accepts query parameters for filtering:

| Parameter | Description                                               |
| --------- | --------------------------------------------------------- |
| `q`       | VSL query filter                                          |
| `g`       | Grouping: `request`, `vxid`, `session`, `raw`             |
| `R`       | Rate limit, e.g. `10/s`                                   |
| `mode`    | `b` for backend only, `c` for client only, empty for both |
| `i`       | Include tags, comma-separated                             |
| `x`       | Exclude tags                                              |

There is a limit of two concurrent varnishlog sessions per pod.

## Logs

### Operator logs

The operator writes structured logs to stdout via `log/slog`:

```bash
kubectl logs -n varnish-gateway-system -l app.kubernetes.io/component=operator -f
```

### Chaperone logs

Chaperone also writes structured logs to stdout. These cover varnishd
lifecycle events, reload successes and failures, and endpoint changes:

```bash
kubectl logs -f <gateway-pod> -c varnish-gateway
```

### Varnish request logs

Varnish request logs are not emitted by default. There are three ways
to access them:

**Logging sidecar** — enable `spec.logging` on your GatewayClassParameters
to run a sidecar that streams varnishlog, varnishlog-json, or varnishncsa
to stdout. See [guides/logging.md](../guides/logging.md) for configuration
and examples.

```bash
kubectl logs -f <gateway-pod> -c varnish-log
```

**kubectl exec** — for ad-hoc debugging, run varnishlog directly inside
the chaperone container:

```bash
kubectl exec -it <gateway-pod> -c varnish-gateway -- \
  varnishlog -n /var/run/varnish/vsm -g request
```

**Dashboard** — if the dashboard is enabled, the `/api/varnishlog`
endpoint streams filtered varnishlog-json output over Server-Sent Events.

## See also

- [Logging guide](../guides/logging.md) — sidecar configuration and varnishlog query examples
- [Troubleshooting](troubleshooting.md)
- [GatewayClassParameters reference](../reference/gatewayclassparameters.md)
