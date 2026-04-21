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

### Prometheus scraping

The Helm chart can create `ServiceMonitor` (operator) and `PodMonitor`
(chaperone) resources for auto-discovery by the
[prometheus-operator](https://github.com/prometheus-operator/prometheus-operator).
This requires the `monitoring.coreos.com` CRDs to be installed — for
example, via kube-prometheus-stack.

```yaml
# values.yaml
serviceMonitor:
  enabled: true
  interval: 30s
  scrapeTimeout: 10s
  # Match your Prometheus instance's serviceMonitorSelector / podMonitorSelector.
  # For kube-prometheus-stack the default selector is `release: <helm-release>`.
  labels:
    release: kube-prometheus-stack
```

The operator metrics Service (`{release}-varnish-gateway-operator-metrics`)
is created unconditionally so you can port-forward it without enabling the
monitor objects.

The chaperone PodMonitor selects all Gateway pods across namespaces by the
`app.kubernetes.io/managed-by: varnish-gateway-operator` label, so a single
monitor covers every Gateway the operator manages.

### Grafana dashboards

The chart ships four Grafana dashboards under
`charts/varnish-gateway/dashboards/`:

| Dashboard       | UID                                | Focus                                                                           |
| --------------- | ---------------------------------- | ------------------------------------------------------------------------------- |
| Varnish         | `varnish-gateway-varnish`          | Client rate, hit ratio, backend errors, threads, storage, bans                  |
| Chaperone       | `varnish-gateway-chaperone`        | Ready/draining state, reload rates and errors, endpoint churn                   |
| Operator        | `varnish-gateway-operator`         | Reconcile latency, workqueue queue/work duration, client-go codes               |
| Soak — Resources | `varnish-gateway-soak-resources`  | Heap, RSS, goroutines, FDs, GC, CPU, restarts — side-by-side leak detector      |

The Varnish and Chaperone dashboards target day-to-day operations; the
Operator and Soak dashboards target regression detection during soak
tests and upgrades.

Two packaging options:

**Helm (auto-discovery via the Grafana sidecar).** Set
`dashboards.enabled=true`. Each JSON file becomes a ConfigMap carrying
the `grafana_dashboard: "1"` label, which kube-prometheus-stack's
Grafana sidecar watches by default:

```yaml
# values.yaml
dashboards:
  enabled: true
  # Optional folder hint for grafana-operator-style sidecars.
  # annotations:
  #   grafana_folder: Varnish Gateway
```

**Manual import.** Open Grafana → Dashboards → Import → paste the JSON
from `charts/varnish-gateway/dashboards/*.json`.

Dashboards target Grafana v12 (`schemaVersion: 41`).

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
