# TODO

## Phase 1: Complete

- Basic vhost-based routing

## Phase 2: Complete

- Path-based routing with exact, prefix, and regex matching
- Route ordering by specificity
- Extended ghost.json format with path rules

## Phase 3: Complete

- Header matching (`rule.Matches[].Headers`)
- Method matching (`rule.Matches[].Method`)
- Query parameter matching (`rule.Matches[].QueryParams`)
- Priority-based route selection with additive specificity bonuses

## Phase 4: Complete

- Traffic splitting (weighted backendRefs)
  - HTTPRoute controller extracts weights from Gateway API BackendRefs
  - Routes with identical match criteria are merged with weighted backends
  - Ghost VMOD implements weighted random backend selection
  - Full test coverage including VTC canary deployment test

## Phase 5: Request/Response Modification (Partial)

### Implemented

- RequestHeaderModifier filter (Set, Add, Remove)
- ResponseHeaderModifier filter (Set, Add, Remove)
- URLRewrite filter - hostname rewriting
- URLRewrite filter - ReplaceFullPath
- URLRewrite filter - ReplacePrefixMatch (accurate implementation)
  - Tracks exact matched prefix from route selection
  - Replaces only the matched portion, preserving remainder
  - Handles query string preservation
  - VTC test coverage included
- Add `ghost.deliver()` call to VCL preamble

### Not Implemented

#### RequestRedirect Filter

**Status**: Not implemented

**Gateway API Requirement**: Support for synthetic HTTP redirect responses.

**Specification**:
```yaml
filters:
- type: RequestRedirect
  requestRedirect:
    scheme: https
    hostname: new-host.example.com
    path:
      type: ReplaceFullPath
      replaceFullPath: /new/path
    port: 8443
    statusCode: 301
```

**Implementation Requirements**:
1. Create synthetic redirect backend (similar to `NotFoundBackend`)
2. Generate `Location` header from redirect config components:
   - scheme (default: use current scheme)
   - hostname (default: use current host)
   - path (apply path rewrite if specified)
   - port (default: use current port)
3. Return appropriate 3xx status code (301, 302, 307, 308)
4. Ensure redirect backend works with Varnish director pattern

**Files to Create/Modify**:
- `ghost/src/redirect_backend.rs` - New synthetic backend for redirects
- `ghost/src/vhost_director.rs:resolve()` - Return redirect backend when filter present
- `ghost/src/director.rs` - Register redirect backend in pool

**Test Coverage Needed**:
- VTC tests for all redirect status codes (301, 302, 307, 308)
- Test scheme rewriting
- Test hostname rewriting
- Test path rewriting in redirects
- Test port modification
- Test Location header construction
- Test with HTTPS redirects

**Gateway API Conformance Impact**:
- Core feature required for conformance
- Many real-world use cases (HTTP→HTTPS redirect, domain migration, etc.)
- Blocking for production readiness

### Implementation Priority

**Critical for Release**:
1. RequestRedirect implementation (missing core feature)

**Nice to Have**:
- Enhanced error reporting for filter application failures
- Metrics for filter application (success/failure counts)
- VTC integration tests for all filter combinations

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

## Infrastructure & RBAC

### Per-Gateway ClusterRoleBinding (OPEN ISSUE)

**Problem**: Chaperone pods need permissions to watch EndpointSlices, but the current RBAC setup only grants permissions to the `default` namespace. When deploying a Gateway to other namespaces, chaperone cannot discover backends.

**Root Cause**:
- Operator creates namespace-specific ServiceAccounts for each Gateway
- Existing ClusterRoleBinding only grants to `system:serviceaccounts:default` group
- ServiceAccounts in other namespaces don't have permissions

**Recommended Solution** (Option 1 from RBAC.md):
- Operator creates a ClusterRoleBinding for each Gateway's ServiceAccount
- Binding references the existing `varnish-gateway-chaperone` ClusterRole
- Use label-based tracking for cleanup (`gateway.networking.k8s.io/gateway-name`)
- On Gateway deletion, query and delete ClusterRoleBindings with matching labels

**Implementation**:
```go
// In Gateway reconciler
func (r *GatewayReconciler) createChaperoneCRB(ctx context.Context, gw *gatewayv1.Gateway) error {
    crb := &rbacv1.ClusterRoleBinding{
        ObjectMeta: metav1.ObjectMeta{
            Name: fmt.Sprintf("varnish-gateway-chaperone-%s-%s", gw.Namespace, gw.Name),
            Labels: map[string]string{
                "gateway.networking.k8s.io/gateway-name": gw.Name,
            },
        },
        RoleRef: rbacv1.RoleRef{
            APIGroup: "rbac.authorization.k8s.io",
            Kind:     "ClusterRole",
            Name:     "varnish-gateway-chaperone",
        },
        Subjects: []rbacv1.Subject{
            {
                Kind:      "ServiceAccount",
                Name:      fmt.Sprintf("%s-chaperone", gw.Name),
                Namespace: gw.Namespace,
            },
        },
    }
    return r.Create(ctx, crb)
}
```

**Workaround**: Manually create ClusterRoleBinding for each namespace until this is implemented.

## Gateway API Conformance

### Conflict Resolution

Implement spec-defined conflict resolution for overlapping routes (e.g., two HTTPRoutes claiming the same hostname):

**Precedence Rules** (from [GEP-713](https://gateway-api.sigs.k8s.io/geps/gep-713/)):
1. **Oldest** resource (by `CreationTimestamp`) wins
2. If still tied, **Alphabetical** order (by `Namespace/Name`) wins

**Critical**: Follow the spec exactly to ensure conformance.

### ReferenceGrant Validation

Implement the ReferenceGrant pattern for cross-namespace Secret access (TLS certificates):

- Gateway in Namespace A referencing Secret in Namespace B requires ReferenceGrant in Namespace B
- Prevents "Secret data exfiltration" where users steal certificates from namespaces they don't own
- Required for Gateway API conformance

### Conformance Testing

Use the Gateway API Conformance Suite to validate implementation:

- Package: `sigs.k8s.io/gateway-api/conformance`
- Runs battery of tests for hostname handling, path matching, status updates
- Gold standard for idiomatic implementation
- Should be integrated into CI pipeline

### Policy Attachment Pattern

Use Policy Attachment instead of GatewayClass-specific fields for Varnish configuration:

- **Direct Policy**: Affects only the object it's attached to (e.g., `VarnishRetryPolicy` on HTTPRoute)
- **Inherited Policy**: Defined at Gateway level, flows down to all HTTPRoutes
- Use `metav1.Hierarchy` logic to traverse from Service to Gateway to find applicable policies

## Gateway Features

- Listener hostname filtering
- sectionName matching (`parentRef.SectionName`)
- Cross-namespace routes (requires ReferenceGrant validation - see above)

## Observability

- Varnish logging via sidecar container (varnishlog/varnishncsa) - Complete
- Add logging configuration to GatewayClassParameters (format, mode, extraArgs) - Complete
- Ensure chaperone uses JSON logging (slog.NewJSONHandler) for consistency - Complete
- Future: Add varnishlog-json support when available
- Future: Create VarnishLoggingPolicy CRD using Gateway API policy attachment pattern
  - Policy targets Gateway via `targetRef`, overrides class defaults when present
  - Enables per-gateway logging config (e.g., verbose for staging, minimal for prod)

## Development Workflow

### CRD Generation

**Problem**: Currently maintaining CRD schema manually in `deploy/00-prereqs.yaml`, which can drift from Go types in `api/v1alpha1/`.

**Solution**: Set up controller-gen workflow:
1. Add `make manifests` target to regenerate CRDs from Go types
2. Auto-copy generated CRD into `deploy/00-prereqs.yaml` (preserving namespace header)
3. Make Go types the source of truth for schema
4. Run `make manifests` before commits that change CRD types

**Benefits**:
- Prevents schema drift
- Kubebuilder markers (validation, defaults) automatically applied
- Less error-prone than manual YAML editing

## Future Enhancements

- **HTTPRoute data-plane readiness signal**: Currently `Accepted=True` is set by the operator immediately, but the route isn't active until chaperone reloads ghost. Consider having chaperone set a custom `Programmed` condition on HTTPRoute after successful ghost reload. Note: Gateway API spec only defines `Programmed` on Gateway/Listener, not HTTPRoute — this would be a custom condition. Requires adding route identity (name/namespace) to routing.json so chaperone can trace back to HTTPRoute objects.

## Open Questions

- Config size limits: ghost.json in ConfigMap has 1MB limit (consider using multiple ConfigMaps or custom storage)
- Reload rate limiting: Add debouncing for rapid HTTPRoute changes?
- Cross-namespace backend discovery: When ReferenceGrant support is added, chaperone will need cluster-wide EndpointSlice watch permissions
