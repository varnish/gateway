# Varnish Gateway Logging

This document describes how to configure varnish logging for the Varnish Gateway operator.

## Overview

The Varnish Gateway supports streaming varnish logs via a sidecar container that runs alongside the main varnish-gateway container. The logging sidecar runs standard varnish logging tools (`varnishlog` or `varnishncsa`), streaming output to stdout where it's captured by Kubernetes' logging infrastructure.

## Architecture

```
┌─────────────────────────────────────────┐
│ Gateway Pod                             │
│                                         │
│ ┌─────────────────┐ ┌─────────────────┐│
│ │ varnish-gateway │ │  varnish-log    ││
│ │                 │ │  (sidecar)      ││
│ │ ┌─────────────┐ │ │                 ││
│ │ │  varnishd   │─┼─┼─► varnishlog/  ││
│ │ │ (via chaper)│ │ │   varnishncsa   ││
│ │ └─────────────┘ │ │        │        ││
│ │                 │ │        ▼        ││
│ └─────────────────┘ │     stdout      ││
│                     │        │        ││
│                     └────────┼────────┘│
└──────────────────────────────┼─────────┘
                               │
                               ▼
                    Kubernetes logging system
                    (kubectl logs, log aggregators)
```

### Key Design Points

1. **Sidecar pattern**: Logging runs in a separate container following Kubernetes best practices
2. **Shared varnish working directory**: Both containers mount the same `/var/run/varnish` volume
3. **Stdout streaming**: Logs go to stdout for automatic Kubernetes capture
4. **Standard kubectl logs**: Access logs via `kubectl logs <pod-name> -c varnish-log`

## Configuration

Logging is configured via the `GatewayClassParameters` CRD:

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-with-logging
spec:
  logging:
    mode: varnishlog          # Required: "varnishlog" or "varnishncsa"
    format: "..."             # Optional: format string for varnishncsa
    extraArgs: []             # Optional: additional arguments
    image: "..."              # Optional: custom image with logging tools
```

### Configuration Fields

#### `logging.mode` (required)

The varnish logging tool to use:

- `varnishlog`: Raw varnish shared memory log (VSL) format
- `varnishncsa`: Apache/NCSA combined log format

Future support planned for `varnishlog-json`.

#### `logging.format` (optional)

Custom format string for `varnishncsa`. See `varnishncsa(1)` man page for format specification.

Default format (if not specified): Apache Combined Log Format
```
%h %l %u %t "%r" %s %b "%{Referer}i" "%{User-agent}i"
```

**Note:** This field is ignored when `mode: varnishlog`.

#### `logging.extraArgs` (optional)

Additional arguments passed to the logging tool. Common use cases:

- Filter by request type: `["-g", "request"]`
- Query filtering: `["-q", "ReqMethod eq 'GET'"]`
- Include additional tags: `["-i", "ReqHeader,RespHeader"]`

See `varnishlog(1)` and `varnishncsa(1)` for available options.

#### `logging.image` (optional)

Container image containing varnish logging tools. Defaults to the same image as the gateway (`GATEWAY_IMAGE` env var on operator).

Use this if you need:
- A specific varnish version
- Custom logging tools
- A minimal image with only logging utilities

## Examples

### Example 1: Basic varnishlog

Stream all varnish log entries in raw VSL format:

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-basic-logging
spec:
  logging:
    mode: varnishlog
```

Access logs:
```bash
kubectl logs -f <gateway-pod-name> -c varnish-log
```

### Example 2: NCSA format with custom pattern

Apache combined log format with custom fields:

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-ncsa
spec:
  logging:
    mode: varnishncsa
    format: '%h %l %u %t "%r" %s %b "%{Referer}i" "%{User-agent}i" %{Varnish:time_firstbyte}x'
```

Output example:
```
10.0.1.45 - - [24/Jan/2026:10:23:14 +0000] "GET /api/users HTTP/1.1" 200 1234 "https://example.com" "Mozilla/5.0..." 0.002341
```

### Example 3: Filtered request logs

Only log GET requests to /api paths:

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-filtered
spec:
  logging:
    mode: varnishlog
    extraArgs:
      - "-g"
      - "request"
      - "-q"
      - "ReqMethod eq 'GET' and ReqURL ~ '^/api'"
```

### Example 4: Combined with user VCL

Use logging alongside custom VCL and varnishd tuning:

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-production
spec:
  logging:
    mode: varnishncsa
    format: '%t %h "%r" %s %b %{Varnish:handling}x'
    extraArgs:
      - "-g"
      - "request"

  userVCLConfigMapRef:
    name: production-vcl
    namespace: varnish-gateway-system
    key: user.vcl

  varnishdExtraArgs:
    - "-p"
    - "thread_pools=4"
    - "-p"
    - "workspace_backend=128k"
```

## Usage

### View live logs

```bash
# Follow logs from the logging sidecar
kubectl logs -f <pod-name> -c varnish-log

# View both containers
kubectl logs <pod-name> --all-containers=true
```

### Send to log aggregator

The logging sidecar outputs to stdout, making it compatible with standard Kubernetes log collection:

- **Fluentd/Fluent Bit**: Automatically collected as container logs
- **Elasticsearch/OpenSearch**: Via Fluentd/Filebeat
- **Splunk**: Via Splunk Connect for Kubernetes
- **CloudWatch/Stackdriver**: Via respective agents

No additional configuration needed - the logs appear as regular container output.

### Resource considerations

The logging sidecar has the following default resource limits:

```yaml
resources:
  requests:
    cpu: 10m
    memory: 32Mi
  limits:
    cpu: 100m
    memory: 128Mi
```

These limits are hardcoded but reasonable for most workloads. The sidecar is lightweight but CPU usage can spike during high traffic periods.

## Troubleshooting

### No logs appearing

Check if the sidecar is running:
```bash
kubectl get pod <pod-name> -o jsonpath='{.spec.containers[*].name}'
# Should show: varnish-gateway varnish-log
```

Check sidecar logs for errors:
```bash
kubectl logs <pod-name> -c varnish-log
```

### Sidecar crashes or restarts

Check events:
```bash
kubectl describe pod <pod-name>
```

Common causes:
- Invalid `mode` value (must be "varnishlog" or "varnishncsa")
- Invalid `extraArgs` (syntax error in varnishlog arguments)
- Invalid `format` for varnishncsa

### Varnish working directory not accessible

The sidecar mounts `/var/run/varnish` as read-only. If you see permission errors, check:

```bash
kubectl exec <pod-name> -c varnish-log -- ls -la /var/run/varnish
```

The varnish shared memory log (VSM) files should be readable.

## Performance Impact

### Logging overhead

- **varnishlog**: Low CPU overhead, minimal memory impact
- **varnishncsa**: Slightly higher CPU due to formatting

Both tools read from varnish shared memory (zero-copy) and perform minimal processing before writing to stdout.

### Storage considerations

Varnish can generate significant log volumes under high traffic:

- **Estimate**: ~500 bytes per request (varies with log detail)
- **Example**: 1000 req/s = ~43 GB/day uncompressed

Recommendations:
- Use log filtering (`extraArgs` with `-q`) to reduce volume
- Configure log rotation in your Kubernetes logging pipeline
- Consider sampling for very high-traffic gateways

## Reference

### varnishlog query language

The `-q` flag supports VSL query expressions:

```
# Only successful requests
-q "RespStatus < 400"

# Exclude health checks
-q "ReqURL !~ '^/health'"

# Slow requests only
-q "Timestamp:Resp[3] > 1.0"
```

See `vsl-query(7)` for full syntax.

### varnishncsa format specifiers

Common format tokens:

| Token | Description |
|-------|-------------|
| `%h` | Remote host (client IP) |
| `%l` | Remote logname (always `-`) |
| `%u` | Remote user (from auth) |
| `%t` | Time (Common Log Format) |
| `%r` | Request line |
| `%s` | Status code |
| `%b` | Response size (bytes) |
| `%{Header}i` | Request header |
| `%{Header}o` | Response header |
| `%{Varnish:tag}x` | Varnish-specific data |

See `varnishncsa(1)` for complete list.
