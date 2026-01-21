# TODO

## Phase 1: Complete

- Basic vhost-based routing

## Phase 2: Complete

- Path-based routing with exact, prefix, and regex matching
- Route ordering by specificity
- Extended ghost.json format with path rules

## Phase 3: Advanced Request Matching

- Header matching (`rule.Matches[].Headers`)
- Method matching (`rule.Matches[].Method`)
- Query parameter matching (`rule.Matches[].QueryParams`)

## Phase 4: Traffic Management

- Traffic splitting (weighted backendRefs)
- RequestMirror filter

## Phase 5: Request/Response Modification

- RequestHeaderModifier filter
- ResponseHeaderModifier filter
- URLRewrite filter
- RequestRedirect filter
- Add `ghost.deliver()` call to VCL preamble

## Phase 6: client-side TLS

- Listener TLS termination (watch `certificateRefs` Secrets)
- Certificate hot-reload on Secret changes
- BackendTLSPolicy support (upstream TLS)

Note: In k8s, cert-manager handles ACME. We just consume `kubernetes.io/tls` Secrets.

## Phase 7: Connection Draining and Stats

### Overview

When a chaperone pod is being replaced (k8s sends SIGTERM), we need to gracefully drain existing connections before shutting down varnishd.

### Requirements

- Monitor `MAIN.sess_conn` counter to track active client connections
- Health endpoint must return 503 once draining starts (triggers k8s to stop routing new traffic)
- Log draining progress every 10 seconds
- When sess_conn reaches 0, perform clean varnishd shutdown via `stop` command
- No explicit timeout - rely on k8s `terminationGracePeriodSeconds` to send SIGKILL if needed

### Implementation Plan

#### 1. On SIGTERM Receipt

```
- Set health state to draining (health endpoint returns 503)
- Log: "Starting connection draining, health check now failing"
- Start polling loop
```

#### 2. Polling Loop (every 1 second)

```
- Get MAIN.sess_conn value
- If sess_conn == 0:
  - Log: "Drain complete, stopping varnishd"
  - Issue `stop` command via varnishadm
  - Wait for varnishd process to exit cleanly
  - os.Exit(0)
- Every 10 seconds: log current sess_conn value
```

#### 3. If SIGKILL Arrives

- K8s will forcefully terminate the pod
- This is expected behavior if draining takes longer than terminationGracePeriodSeconds

### Benefits

- Regular, proper shutdowns exercise the full shutdown path in production
- Catches bugs in varnishd shutdown logic early
- Clean exits reduce risk of corrupted state

### Configuration

- Default k8s `terminationGracePeriodSeconds`: should be set to 300-600s (5-10 minutes)
- Polling interval: 1 second
- Logging interval: 10 seconds

### Stats Package Design

#### Requirements

1. **Connection Draining**: Need to poll MAIN.sess_conn every 1 second
2. **Prometheus Metrics**: Need to expose varnishd stats + chaperone metrics via /metrics endpoint
3. **JSON Output**: Use `varnishstat -j` for clean parsing (avoid spawning `varnishstat -1 -f FIELD` repeatedly)

#### Proposed API

```go
// Package: internal/stats

// Collector collects varnish statistics via varnishstat -j
// and chaperone internal metrics
type Collector struct {
    varnishDir string
}

// Snapshot represents a point-in-time stats snapshot
// Contains both varnishd counters and chaperone metrics
type Snapshot struct {
    Timestamp time.Time
    Counters  map[string]int64
}

// Get returns a one-shot snapshot of all stats
func (c *Collector) Get() (*Snapshot, error)

// Watch starts a goroutine that polls stats at the given interval
// and sends snapshots to the returned channel
func (c *Collector) Watch(ctx context.Context, interval time.Duration) <-chan *Snapshot

// GetCounter retrieves a specific counter from a snapshot
func (s *Snapshot) GetCounter(name string) (int64, bool)
```

#### Usage Examples

**Connection Draining:**
```go
collector := stats.New(cfg.VarnishDir)
snapshots := collector.Watch(ctx, 1*time.Second)

ticker := time.NewTicker(10*time.Second)
for {
    select {
    case snap := <-snapshots:
        sessConn, _ := snap.GetCounter("MAIN.sess_conn")
        if sessConn == 0 {
            // Drain complete
            return
        }
        select {
        case <-ticker.C:
            slog.Info("draining connections", "remaining", sessConn)
        default:
        }
    case <-ctx.Done():
        return
    }
}
```

**Prometheus Metrics:**
```go
collector := stats.New(cfg.VarnishDir)

http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
    snap, err := collector.Get()
    if err != nil {
        // handle error
    }

    // Export varnish counters
    for name, value := range snap.Counters {
        fmt.Fprintf(w, "varnish_%s %d\n", sanitize(name), value)
    }

    // Add chaperone-specific metrics
    fmt.Fprintf(w, "chaperone_vcl_reloads_total %d\n", vclReloadCount)
})
```

#### Implementation Notes

- Spawn `varnishstat -n <dir> -j` once per poll
- Parse JSON output into map[string]int64
- Handle both gauge and counter types (JSON includes type info)
- Consider caching last snapshot for Prometheus scrapes (avoid spawning on every scrape)

#### Stats to Monitor

- **MAIN.sess_conn** - Active client connections (draining)
- **MAIN.client_req** - Total client requests (prometheus)
- **MAIN.cache_hit** - Cache hits (prometheus)
- **MAIN.cache_miss** - Cache misses (prometheus)
- **MAIN.backend_conn** - Backend connections (prometheus)
- **MAIN.backend_fail** - Backend failures (prometheus)

## Gateway Features

- Listener hostname filtering
- sectionName matching (`parentRef.SectionName`)
- Cross-namespace routes (ReferenceGrant validation)

## Observability

- Add varnishlog-json subprocess to chaperone for access logging to stdout
- Ensure chaperone uses JSON logging (slog.NewJSONHandler) for consistency
- Both log streams intermingled on stdout with distinguishing fields
- Logging policy configuration:
  - Add logging defaults to GatewayClassParameters (format, query, verbosity)
  - Create VarnishLoggingPolicy CRD using Gateway API policy attachment pattern
  - Policy targets Gateway via `targetRef`, overrides class defaults when present
  - Enables per-gateway logging config (e.g., verbose for staging, minimal for prod)

## Open Questions

- Cross-namespace services: Chaperone needs RBAC to watch EndpointSlices across namespaces
- Config size limits: ghost.json in ConfigMap has 1MB limit
- Reload rate limiting: Add debouncing for rapid HTTPRoute changes?
