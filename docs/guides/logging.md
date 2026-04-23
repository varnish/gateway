# Varnish Gateway Logging

This document describes how to configure varnish logging for the Varnish Gateway operator.

## Overview

The Varnish Gateway supports streaming varnish logs via a sidecar container that runs alongside the main varnish-gateway container. The logging sidecar runs standard varnish logging tools (`varnishlog`, `varnishlog-json`, or `varnishncsa`), streaming output to stdout where it's captured by Kubernetes' logging infrastructure.

**Logging is off by default.** If `spec.logging` is not set on the GatewayClassParameters, no sidecar is created and no varnish log output is produced. Enable it explicitly as shown below.

## Key Design Points

1. Sidecar pattern: Logging runs in a separate container.
2. Shared varnish working directory: Both containers mount the same `/var/run/varnish` volume
3. Stdout streaming: Logs go to stdout for automatic Kubernetes capture
4. Standard kubectl logs: Access logs via `kubectl logs <pod-name> -c varnish-log`
5. Automatic retry: Uses `varnishlog -t off` to wait indefinitely for varnishd to start (no fixed delays)

## Configuration

Logging is configured via the `GatewayClassParameters` CRD:

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-with-logging
spec:
  logging:
    mode: varnishlog # Required: "varnishlog", "varnishlog-json", or "varnishncsa"
    format: "..." # Optional: format string for varnishncsa
    extraArgs: [] # Optional: additional arguments
    image: "..." # Optional: custom image with logging tools
```

### Configuration Fields

#### `logging.mode` (required)

The varnish logging tool to use:

- `varnishlog`: Raw varnish shared memory log (VSL) format — full VSL detail, intended for human reading or ad-hoc parsing.
- `varnishlog-json`: One JSON object per transaction — a structured form of the same VSL data, suitable for log aggregators (Elasticsearch/OpenSearch, Loki, Splunk, etc.) that prefer structured input over line-oriented text.
- `varnishncsa`: Apache/NCSA combined log format — one line per request, ideal for traditional access logging.

#### `logging.format` (optional)

Custom format string for `varnishncsa`. See `varnishncsa(1)` man page for format specification.

Default format (if not specified): Apache Combined Log Format

```
%h %l %u %t "%r" %s %b "%{Referer}i" "%{User-agent}i"
```

**Note:** This field is ignored when `mode: varnishlog` or `mode: varnishlog-json`.

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

### Example 2: Structured JSON logs

Stream one JSON object per transaction. Good when shipping to an aggregator that parses structured fields rather than regex-matching on a line format:

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-json-logging
spec:
  logging:
    mode: varnishlog-json
    extraArgs:
      - "-g"
      - "request"
```

Output (one object per line, abbreviated, pretty-printed for readability):

```json
{
  "vxid": 32771,
  "type": "request",
  "tags": [
    { "tag": "ReqMethod", "value": "GET" },
    { "tag": "ReqURL", "value": "/api/users" },
    { "tag": "RespStatus", "value": "200" }
  ]
}
```

Use `extraArgs: ["-g", "request"]` to group transactions by request — without it you get one object per VSL transaction, which splits client and backend work across multiple records.

### Example 3: NCSA format with custom pattern

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

### Example 4: Filtered request logs

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

## Dynamic Configuration Updates

You can change logging configuration without redeploying Gateways. Edit the GatewayClassParameters:

```bash
# Edit logging configuration
kubectl edit gatewayclassparameters varnish-params

# Change mode, format, or extraArgs
# The operator will automatically update all Gateways using this GatewayClass
```

The operator watches GatewayClassParameters for changes and automatically:

1. Detects which Gateways use the modified parameters
2. Triggers reconciliation for those Gateways
3. Updates the Deployment with new logging configuration
4. Kubernetes performs a rolling update

## Usage

### View live logs

```bash
# Follow logs from the logging sidecar
kubectl logs -f <pod-name> -c varnish-log

# View both containers
kubectl logs <pod-name> --all-containers=true
```

### On-demand varnishlog via kubectl exec

The logging sidecar provides steady-state logs, but for ad-hoc debugging you can run `varnishlog` directly inside the chaperone container. This gives you full access to the VSL query language and all filtering flags without changing any configuration.

The chaperone container has varnish tools installed and the VSM (shared memory) mounted at `/var/run/varnish/vsm`.

```bash
# Basic varnishlog - all traffic
kubectl exec -it <pod-name> -c chaperone -- \
  varnishlog -n /var/run/varnish/vsm

# Filter by URL pattern
kubectl exec -it <pod-name> -c chaperone -- \
  varnishlog -n /var/run/varnish/vsm -q "ReqURL ~ '^/api'"

# Only show specific tags (request and response headers)
kubectl exec -it <pod-name> -c chaperone -- \
  varnishlog -n /var/run/varnish/vsm -i ReqHeader,RespHeader

# Combine query filter with tag selection
kubectl exec -it <pod-name> -c chaperone -- \
  varnishlog -n /var/run/varnish/vsm -q "ReqURL ~ '^/api'" -i ReqHeader,RespHeader,ReqURL,RespStatus

# Rate-limit output on busy systems
kubectl exec -it <pod-name> -c chaperone -- \
  varnishlog -n /var/run/varnish/vsm -R 10/s

# Debug 503 errors
kubectl exec -it <pod-name> -c chaperone -- \
  varnishlog -n /var/run/varnish/vsm -q "RespStatus == 503" -g request

# Exclude health check noise
kubectl exec -it <pod-name> -c chaperone -- \
  varnishlog -n /var/run/varnish/vsm -q "ReqURL !~ '^/health'"

# Show backend-side transactions (useful for debugging upstream issues)
kubectl exec -it <pod-name> -c chaperone -- \
  varnishlog -n /var/run/varnish/vsm -g request -q "BerespStatus >= 500"

# Run varnishncsa for a quick access log view
kubectl exec -it <pod-name> -c chaperone -- \
  varnishncsa -n /var/run/varnish/vsm
```

**Key flags:**

| Flag | Description                                        |
| ---- | -------------------------------------------------- |
| `-q` | VSL query expression (see `vsl-query(7)`)          |
| `-i` | Include only these tags                            |
| `-I` | Include tags matching a regex                      |
| `-x` | Exclude these tags                                 |
| `-X` | Exclude tags matching a regex                      |
| `-g` | Grouping mode: `raw`, `vxid`, `request`, `session` |
| `-R` | Rate limit output (e.g., `10/s`, `100/m`)          |

**Tip:** Use `-g request` to group related log lines by request transaction. This makes it much easier to follow a single request through Varnish.

## Reference

- `vsl-query(7)`
- `varnishlog(1)`
- `varnishncsa(1)`
- `varnishlog-json(1)`
