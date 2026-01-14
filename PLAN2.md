# Varnish Gateway Operator - Architecture Plan v2

This plan supersedes PLAN.md. The key architectural change is replacing the nodes/udo/activedns vmod trio with a purpose-built **ghost** vmod.

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                        Kubernetes Cluster                        │
├─────────────────────────────────────────────────────────────────┤
│                                                                   │
│   ┌─────────────┐         watches          ┌──────────────────┐ │
│   │  Operator   │ ◄────────────────────────│  Gateway API     │ │
│   │             │                          │  Resources       │ │
│   └──────┬──────┘                          │  - Gateway       │ │
│          │                                  │  - HTTPRoute     │ │
│          │ creates/updates                  │  - GatewayClass  │ │
│          ▼                                  └──────────────────┘ │
│   ┌─────────────────────────────────────────────────────────┐   │
│   │                    Varnish Pod                           │   │
│   │  ┌─────────────┐    ┌─────────────┐    ┌─────────────┐  │   │
│   │  │  Varnish    │    │  Chaperone  │    │  ConfigMaps │  │   │
│   │  │  + ghost    │◄───│             │◄───│  - main.vcl │  │   │
│   │  │             │    │             │    │             │  │   │
│   │  └─────────────┘    └──────┬──────┘    └─────────────┘  │   │
│   │                            │                             │   │
│   │                            │ watches                     │   │
│   │                            ▼                             │   │
│   │                     EndpointSlices                       │   │
│   └─────────────────────────────────────────────────────────┘   │
│                                                                   │
└─────────────────────────────────────────────────────────────────┘
```

## Component Responsibilities

### Operator

Runs as a cluster-wide deployment. Watches Gateway API resources and translates them into:

1. **Varnish Deployments**: Creates pods with Varnish + ghost vmod + chaperone
2. **VCL ConfigMaps**: Minimal VCL preamble + user VCL concatenated
3. **Ghost Config**: Writes routing rules to `ghost.json` (via ConfigMap)

The operator no longer generates complex VCL routing logic. Instead, it generates:
- A simple VCL preamble that imports ghost and calls its functions
- A JSON configuration file that ghost reads at runtime

### Chaperone

Runs alongside each Varnish instance. Handles:

1. **Endpoint Discovery**: Watches EndpointSlices, writes backend IPs to ghost.json
2. **Configuration Reload**: Triggers ghost reload via HTTP request to localhost
3. **Health Reporting**: Reports reload success/failure to operator

### Ghost VMOD

Handles all routing logic internally:

1. **Configuration Loading**: Reads ghost.json at init and on reload
2. **Request Routing**: Matches requests to backend groups
3. **Backend Selection**: Weighted selection within groups
4. **Hot Reload**: Updates config without VCL reload

See `ghost-vmod.md` for the vmod development plan.

---

## Generated Files

The operator and chaperone write these files (via ConfigMaps/shared volumes):

```
/var/run/varnish/
├── main.vcl          # VCL preamble + user VCL
└── ghost.json        # Ghost configuration (routing + backends)
```

### main.vcl

Minimal VCL that integrates ghost:

```vcl
vcl 4.1;

import ghost;

sub vcl_init {
    ghost.init("/var/run/varnish/ghost.json");
}

sub vcl_recv {
    ghost.recv();
}

sub vcl_backend_fetch {
    ghost.backend_fetch();
}

# --- User VCL concatenated below ---
```

User VCL is appended after this preamble. VCL's subroutine concatenation means user code in `vcl_recv` and `vcl_backend_fetch` runs after ghost's routing.

### ghost.json

Configuration file containing routing rules and backends. Schema evolves with ghost phases:

**Phase 1 (vhost routing):**
```json
{
  "version": 1,
  "vhosts": {
    "api.example.com": {
      "backends": [
        {"address": "10.0.0.1", "port": 8080, "weight": 100},
        {"address": "10.0.0.2", "port": 8080, "weight": 100}
      ]
    }
  }
}
```

**Phase 2+ adds paths, headers, filters** (see ghost-vmod.md).

---

## Phased Development

Development is synchronized with ghost vmod phases.

### Phase 1: Virtual Host Routing ✓ GHOST COMPLETE

**Ghost VMOD** (COMPLETE):
- JSON config loading with version validation
- Exact and wildcard hostname matching
- Weighted backend selection
- Connection pooling and streaming responses
- Hot-reload via magic endpoint

**Operator changes** (TODO):
- Simplify VCL generator to produce only the ghost preamble
- Generate ghost.json with vhost-to-backend mappings from HTTPRoutes
- Remove nodes/udo vmod references
- Update Deployment to use Varnish 7.6 + ghost image

**Chaperone changes** (TODO):
- Replace backends.conf (INI) writer with ghost.json (JSON) writer
- Implement reload trigger via HTTP to `/.varnish-ghost/reload`
- Handle reload response (success/failure)
- Remove varnishadm VCL reload (ghost handles config changes)

**Gateway API coverage:**
- HTTPRoute hostnames ✓
- HTTPRoute backendRefs (single backend per rule) ✓
- Backend weights ✓
- Gateway status conditions ✓

**What's deferred:**
- Path matching
- Header/method/query matching
- Traffic splitting across multiple backends
- Request/response filters

### Phase 2: Path Matching

**Operator changes:**
- Extend ghost.json generator to include path rules
- Parse HTTPRoute path matches (exact, prefix, regex)
- Order routes by specificity

**Chaperone changes:**
- No changes (ghost.json schema is extensible)

**Gateway API coverage:**
- HTTPPathMatch Exact ✓
- HTTPPathMatch PathPrefix ✓
- HTTPPathMatch RegularExpression ✓

### Phase 3: Advanced Request Matching

**Operator changes:**
- Add header matching to ghost.json
- Add method matching to ghost.json
- Add query parameter matching to ghost.json

**Gateway API coverage:**
- HTTPHeaderMatch ✓
- HTTPMethodMatch ✓
- HTTPQueryParamMatch ✓

### Phase 4: Traffic Management

**Operator changes:**
- Support multiple backendRefs per rule with weights
- Generate backend groups in ghost.json
- Support RequestMirror filter

**Gateway API coverage:**
- Traffic splitting (weighted backendRefs) ✓
- RequestMirror filter ✓

### Phase 5: Request/Response Modification

**Operator changes:**
- Parse HTTPRoute filters
- Generate filter config in ghost.json
- Add ghost.deliver() call to VCL preamble

**Gateway API coverage:**
- RequestHeaderModifier ✓
- ResponseHeaderModifier ✓
- URLRewrite ✓
- RequestRedirect ✓

### Phase 6: TLS and Production Hardening

**Operator changes:**
- Parse Gateway TLS configuration
- Generate TLS settings in ghost.json
- Watch and rotate TLS secrets

**Chaperone changes:**
- Support certificate file updates
- Trigger reload on cert changes

**Gateway API coverage:**
- Gateway TLS termination ✓
- BackendTLSPolicy ✓

---

## Repository Structure

```
varnish-gateway/
├── cmd/
│   ├── operator/           # Operator entry point
│   └── chaperone/          # Chaperone entry point
├── api/
│   └── v1alpha1/           # CRD types (GatewayClassParameters)
├── internal/
│   ├── controller/         # Kubernetes controllers
│   │   ├── gateway_controller.go
│   │   └── httproute_controller.go
│   ├── ghost/              # NEW: ghost.json generation
│   │   ├── config.go       # Config structs matching ghost schema
│   │   ├── generator.go    # Generate ghost.json from HTTPRoutes
│   │   └── generator_test.go
│   ├── vcl/                # Simplified VCL generation
│   │   ├── preamble.go     # Generate ghost preamble VCL
│   │   └── merge.go        # Concatenate with user VCL
│   ├── backends/           # SIMPLIFIED: Just endpoint discovery
│   │   └── watcher.go      # Watch EndpointSlices
│   ├── reload/             # NEW: Ghost reload client
│   │   └── client.go       # HTTP client for reload endpoint
│   ├── varnishadm/         # RETAINED: For health checks, status
│   │   └── ...
│   └── status/
│       └── conditions.go
├── config/
│   ├── crd/
│   ├── rbac/
│   └── manager/
├── ghost/                  # NEW: Ghost vmod source (Rust)
│   ├── Cargo.toml
│   ├── src/
│   │   └── lib.rs
│   └── tests/
├── docker/
│   ├── Dockerfile.varnish  # Varnish 7.6 + ghost.so
│   └── Dockerfile.chaperone
└── go.mod
```

---

## Package Changes from PLAN.md

### Removed/Simplified

| Package | Change |
|---------|--------|
| `internal/vcl/generator.go` | Simplified: only generates ghost preamble, no routing logic |
| `internal/backends/nodes_file.go` | Removed: no longer generating INI files |
| `internal/backends/services.go` | Removed: services info now in ghost.json |

### New

| Package | Purpose |
|---------|---------|
| `internal/ghost/config.go` | Go structs matching ghost.json schema |
| `internal/ghost/generator.go` | Generate ghost.json from Gateway API resources |
| `internal/reload/client.go` | HTTP client to trigger ghost reload |
| `ghost/` | Rust source for ghost vmod |

### Modified

| Package | Change |
|---------|--------|
| `internal/backends/watcher.go` | Outputs to ghost.json instead of backends.conf |
| `internal/controller/gateway_controller.go` | Uses new ghost config generator |
| `internal/controller/httproute_controller.go` | Triggers ghost.json regeneration |

---

## Operator Configuration

| Name | Description |
|------|-------------|
| `GATEWAY_CLASS_NAME` | Which GatewayClass this operator manages |
| `VARNISH_IMAGE` | Varnish 7.6 + ghost image |
| `CHAPERONE_IMAGE` | Chaperone image |
| `METRICS_ADDR` | Prometheus metrics endpoint |
| `HEALTH_PROBE_ADDR` | Health/ready probes |
| `LEADER_ELECTION` | Enable leader election (for HA) |

## Chaperone Configuration

| Name | Description |
|------|-------------|
| `GHOST_CONFIG_PATH` | Path to ghost.json (default: `/var/run/varnish/ghost.json`) |
| `VARNISH_ADDR` | Varnish HTTP address for reload (default: `localhost:80`) |
| `NAMESPACE` | Namespace to watch endpoints in |

---

## Reload Flow

```
HTTPRoute changes
       │
       ▼
┌─────────────┐
│  Operator   │ ──► regenerates ghost.json in ConfigMap
└─────────────┘
       │
       │ ConfigMap update propagates to pod
       ▼
┌─────────────┐
│  Chaperone  │ ──► detects ghost.json change (fsnotify)
└─────────────┘
       │
       │ HTTP request to localhost
       ▼
┌─────────────┐
│   Varnish   │
│  + ghost    │ ──► ghost.recv() handles reload request
└─────────────┘
       │
       │ ghost reloads config atomically
       ▼
   New routing active
```

**Failure handling:**
- If ghost reload fails (invalid config), chaperone logs error
- Old config remains active (no traffic disruption)
- Chaperone reports failure via metrics/status
- Operator can observe failure and update Gateway status

---

## Migration from PLAN.md

For existing code:

1. **VCL Generator** (`internal/vcl/generator.go`)
   - Keep `CollectServices()` logic, adapt to generate ghost.json
   - Replace routing VCL generation with simple preamble
   - Move complex logic to `internal/ghost/generator.go`

2. **Backends Watcher** (`internal/backends/watcher.go`)
   - Change output format from INI to JSON
   - Merge with routing config into single ghost.json

3. **Controllers**
   - Update to use new ghost generator
   - Remove varnishadm VCL reload calls
   - Add ghost reload trigger

4. **Tests**
   - Update expected outputs from VCL to ghost.json
   - Add tests for ghost config generation

---

## Design Decisions

1. **Single config file**: ghost.json contains both routing and backends. Simpler than separate files, atomic updates.

2. **HTTP reload trigger**: Simpler than varnishadm, provides synchronous feedback, works through Varnish's normal request path.

3. **Varnish Cache 7.6**: Open source, no Enterprise dependencies, full control over the image.

4. **Config versioning**: ghost.json includes version field. Ghost validates compatibility, fails reload if version unsupported.

5. **VCL concatenation preserved**: User VCL is still concatenated after preamble. Users can still customize behavior in vcl_recv, vcl_backend_fetch, etc.

---

## Current Code Status

### Ghost VMOD - Phase 1 COMPLETE

The ghost VMOD (`ghost/`) is production-ready for Phase 1 functionality:

**Implemented:**
- JSON configuration loading with version validation (`src/config.rs`)
- Host matching: exact, wildcard (`*.example.com`), default fallback (`src/routing.rs`)
- Weighted random backend selection
- Async HTTP client with connection pooling (32 idle/host, 90s timeout) (`src/runtime.rs`)
- Streaming response bodies via tokio channels (`src/response.rs`)
- Hot-reload via `/.varnish-ghost/reload` magic endpoint
- Hop-by-hop header filtering (RFC 7230 compliant)
- Proper error responses: 404 (no vhost), 503 (no backends)
- 29 unit tests, VTC integration test framework

**VMOD Interface:**
```vcl
import ghost;
sub vcl_init { ghost.init("/etc/varnish/ghost.json"); new router = ghost.ghost_backend(); }
sub vcl_recv { set req.http.x-ghost-reload = ghost.recv(); if (req.http.x-ghost-reload) { return (synth(200, "Reload")); } }
sub vcl_backend_fetch { set bereq.backend = router.backend(); }
```

### Go Components (from previous development):

**Working:**
- Gateway controller creates Deployments, Services, ConfigMaps
- HTTPRoute controller watches routes, collects by Gateway
- Status conditions set on Gateway and HTTPRoute
- VCL generator with hostname and path matching
- Backends watcher with EndpointSlice watching
- nodes_file.go generates INI format (to be replaced)
- varnishadm client implementation (retained for health checks)
- VCL reloader via varnishadm (to be simplified)

**To migrate:**
- `internal/vcl/generator.go` → `internal/ghost/generator.go`
- `internal/backends/nodes_file.go` → incorporated into ghost generator
- VCL output → ghost.json output

**To remove:**
- nodes/udo vmod references in generated VCL
- Complex routing logic in VCL generator
- INI file generation

### Migration order:

1. ~~Create ghost VMOD with Phase 1 routing~~ ✓ DONE
2. Create `internal/ghost/` package with config types and generator
3. Update backends watcher to output JSON
4. Simplify VCL generator to preamble-only
5. Update controllers to use ghost generator
6. Add reload client for chaperone
7. Update container images to Varnish 7.6 + ghost
8. Remove deprecated code

---

## Open Questions

1. **Cross-namespace services**: HTTPRoute can reference services in other namespaces. Chaperone needs RBAC to watch EndpointSlices across namespaces. Same approach as before, just different output format.

2. **Config size limits**: ghost.json in ConfigMap has 1MB limit. Should be fine for reasonable route counts. Monitor and add compression if needed.

3. **Reload rate limiting**: Rapid HTTPRoute changes could cause rapid reloads. Add debouncing in chaperone? Or rely on ConfigMap propagation delay?

4. **Ghost vmod repository**: Same repo or separate? Starting in same repo (`ghost/` directory) for tight coupling during development. Can split later if needed.
