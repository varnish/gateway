# Varnish Gateway Operator - Development Guide

## Project Overview

Kubernetes Gateway API implementation using Varnish. Three components:
- **operator**: watches Gateway API resources, generates ghost.json config, manages deployments
- **chaperone**: handles endpoint discovery and triggers ghost reload
- **ghost**: Rust VMOD that handles all routing logic internally

## Documentation

- [Configuration Reference](docs/configuration-reference.md) - GatewayClassParameters, varnishd args, defaults

## Current Status

The project passes the Gateway API conformance test suite (`make test-conformance`). See TODO.md for remaining work.

### Component Overview

**Operator** (`cmd/operator/main.go`):
- Gateway controller - creates Deployment, Service, ConfigMap, Secret, ServiceAccount
- HTTPRoute controller - watches routes, regenerates routing.json on changes
- Status conditions (Accepted/Programmed) on Gateway and HTTPRoute
- GatewayClassParameters CRD for user VCL injection and varnishd extra args
- Client-side TLS termination with cert-manager support and hot-reload

**Chaperone** (`cmd/chaperone/main.go`):
- Starts and manages varnishd via vrun package
- Watches routing.json + EndpointSlices, merges into ghost.json
- Triggers ghost reload via HTTP, VCL reload via varnishadm
- TLS cert loading and hot-reload via fsnotify

**Ghost VMOD** (`ghost/`):
- Rust-based VMOD handling all routing inside Varnish
- Path matching (exact, prefix, regex), method, header, query parameter matching
- Priority-based route selection with additive specificity bonuses
- Hot-reload via `/.varnish-ghost/reload`
- See `ghost/CLAUDE.md` and `ghost/README.md` for details

### Key Packages

- `internal/ghost/` - Config types, ghost.json generator, EndpointSlice watcher
- `internal/vcl/` - VCL generator, merge, hot-reload via varnishadm
- `internal/varnishadm/` - Full varnishadm protocol (reverse mode, -M flag)
- `internal/vrun/` - varnishd process lifecycle management
- `internal/reload/` - HTTP client for ghost reload endpoint

## Development Setup

### Prerequisites

- Go 1.21+
- Rancher Desktop (or any local k8s)
- kubectl configured to talk to your cluster
- Rust 1.75+ (for ghost VMOD development)

### Install Gateway API CRDs

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml
```

### Local k8s development setup

See k8s-dev-howto.md for instruction on how to setup a local k8s cluster and how to induce noise and chaos into
the cluster.

### Version Management

The `.version` file and git tags are managed by [bump](https://github.com/perbu/bump). It's installed as a Go tool dependency.

```bash
# Bump patch version (v0.1.2 -> v0.1.3)
go tool bump -patch

# Bump minor version (v0.1.2 -> v0.2.0)
go tool bump -minor

# Bump major version (v0.1.2 -> v1.0.0)
go tool bump -major
```

## Architecture

### Data Flow

1. **Operator** watches Gateway/HTTPRoute resources
   - Generates `routing.json` (host → service mappings) and stores in ConfigMap
   - Generates ghost preamble VCL and merges with user VCL
   - Creates Varnish listeners from Gateway listeners (one per unique port)

2. **Chaperone** runs as wrapper for varnishd
   - Starts varnishd via vrun package with named listeners from `VARNISH_LISTEN`
   - Watches `routing.json` + EndpointSlices
   - Merges them into `ghost.json` (host → actual pod IPs)
   - Triggers ghost reload via HTTP endpoint

3. **Ghost VMOD** handles all routing inside Varnish
   - Reads `ghost.json` on reload
   - Routes requests by hostname to weighted backend groups
   - Filters routes by `local.socket` for listener-aware routing
   - Sets `X-Gateway-Listener` header on requests for user VCL

### Multi-Listener Architecture

Each Gateway listener maps to a Varnish `-a` socket named `{proto}-{port}` (e.g., `http-80`, `https-443`).
Multiple Gateway listeners on the same port collapse into a single Varnish socket — hostname
isolation is handled by ghost's existing vhost routing.

**Container ports = listener ports**: No port translation. Listener port 3000 means Varnish
binds to `:3000` inside the container. Listener changes trigger a pod restart (via infra hash).

**Listener naming convention**: `{proto}-{port}` encodes both protocol and port:
- `http-80` — HTTP on port 80
- `https-443` — HTTPS on port 443
- `http-3000` — HTTP on port 3000

Ghost uses `local.socket()` to get the Varnish socket name and filters routes accordingly.
The `listeners` field in routing.json/ghost.json contains socket names that a route applies to.
An empty `listeners` list means the route matches on all listeners.

**Request headers**: Ghost sets two headers on every request for use in user VCL:
- `X-Gateway-Listener` — Varnish socket name (e.g., `http-80`)
- `X-Gateway-Route` — HTTPRoute namespace/name (e.g., `default/my-route`)

These propagate to the backend and are available in user VCL:

```vcl
sub vcl_recv {
    if (req.http.X-Gateway-Listener == "http-80") {
        // redirect to HTTPS, restrict access, etc.
    }
    if (req.http.X-Gateway-Route == "production/api-route") {
        // route-specific logic
    }
}
```

**VCL caveat**: User VCL applies globally across all listeners. There is no per-listener or
per-route VCL. Users who need listener-specific behavior should branch on these headers.

### Controller Architecture Principles

**Separation of Concerns**

The Gateway and HTTPRoute controllers have clearly defined responsibilities:

**Gateway Controller** (infrastructure owner):
- Responsibilities:
  - Generate ghost preamble VCL
  - Fetch and merge user VCL from GatewayClassParameters
  - Create and update ConfigMap with `main.vcl`
  - Manage Deployment, Service, RBAC resources
  - Compute infrastructure hash for pod restart detection
- Watches:
  - Gateway resources (primary)
  - GatewayClassParameters (for varnishdExtraArgs, logging, user VCL ref)
  - User VCL ConfigMaps (referenced by GatewayClassParameters)
- Owns: Deployment, Service, ConfigMap, Secret, ServiceAccount, Role, RoleBinding

**HTTPRoute Controller** (routing owner):
- Responsibilities:
  - Generate routing.json from HTTPRoutes
  - Update ConfigMap with `routing.json` ONLY (preserves main.vcl)
  - Update Gateway listener AttachedRoutes counts
- Watches:
  - HTTPRoute resources (primary)
  - Gateway resources (for parent references)
- Owns: routing.json field in ConfigMap (shared ownership)

**Watch Chain Diagram**

```
User VCL ConfigMap → Gateway Controller → ConfigMap main.vcl → Chaperone hot-reload
GatewayClassParameters → Gateway Controller → Infrastructure hash → Rolling restart
HTTPRoute → HTTPRoute Controller → ConfigMap routing.json → Ghost hot-reload
```

**Change Detection Strategy**

**Pod Restart Required** (infrastructure changes):
- Image version change
- VarnishdExtraArgs change
- Logging configuration change
- ImagePullSecrets change
- Listener changes (port additions/removals, protocol changes)
- **Detection**: Infrastructure hash annotation (`varnish.io/infra-hash`) on Deployment pod template
- **Trigger**: Annotation change causes Kubernetes to perform rolling restart

**Hot Reload** (config changes):
- User VCL changes → varnishadm reload (no pod restart)
- Routing changes → ghost HTTP reload (no pod restart)
- **Detection**: Chaperone watches ConfigMap via Kubernetes informers with content-based deduplication
- **Trigger**: In-process reload, zero downtime

**ConfigMap Shared Ownership Pattern**

The Gateway's ConfigMap (`{gateway-name}-vcl`) uses shared ownership:

```
ConfigMap:
├── main.vcl (owned by Gateway controller)
│   ├── Generated by: vcl.Generate() + vcl.Merge()
│   ├── Updated when: Gateway reconciles (VCL changes, user VCL changes)
│   └── Watched by: Chaperone → hot-reloads via varnishadm
└── routing.json (owned by HTTPRoute controller)
    ├── Generated by: ghost.GenerateRoutingConfigV2()
    ├── Updated when: HTTPRoute reconciles (route changes)
    └── Watched by: Chaperone → triggers ghost reload via HTTP
```

**Rule**: Each controller updates only its owned fields, preserves others.

**Event Cascade**

```
User VCL ConfigMap change
  ↓ (watched by Gateway controller via enqueueGatewaysForConfigMap)
Gateway reconcile
  ↓ (regenerate VCL, update ConfigMap main.vcl)
  ↓ (infra-hash unchanged? YES)
  → ConfigMap updated
     ├─> Ghost watcher: sees update, checks routing.json (unchanged), SKIPS reload
     └─> VCL reloader: sees update, checks main.vcl (changed), TRIGGERS VCL reload

GatewayClassParameters.varnishdExtraArgs change
  ↓ (watched by Gateway controller via enqueueGatewaysForParams)
Gateway reconcile
  ↓ (compute new infrastructure hash)
  ↓ (infra-hash changed? YES)
  → Deployment annotation updated
     └─> Kubernetes triggers rolling restart

HTTPRoute change
  ↓ (primary resource of HTTPRoute controller)
HTTPRoute reconcile
  ↓ (generate routing.json, update ConfigMap routing.json)
  → ConfigMap updated
     ├─> Ghost watcher: sees update, checks routing.json (changed), TRIGGERS ghost reload
     └─> VCL reloader: sees update, checks main.vcl (unchanged), SKIPS reload
```

### Two Reload Paths

- **VCL changes** (user VCL updates): varnishadm hot-reload
- **Backend/routing changes**: ghost HTTP reload (`/.varnish-ghost/reload`)

### Cache Invalidation

VarnishCacheInvalidation is a one-shot CRD for purging/banning cached objects across all gateway pods.
Supports multiple paths per resource for batch invalidation.

**Data Flow:**

1. User creates a VarnishCacheInvalidation CR with type (Purge/Ban), hostname, paths, and gatewayRef
2. Each chaperone pod watches VarnishCacheInvalidation resources via dynamic client
3. Chaperone sends HTTP requests to local Varnish for each path: PURGE or BAN method
4. VCL preamble handles the method: `return(purge)` for PURGE, `std.ban()` for BAN
5. Chaperone writes per-pod result (with per-path detail) to `status.podResults`
6. Phase transitions: Pending → InProgress → Complete (all pods succeed) or Failed
7. Operator GC deletes completed CRs after TTL (default 1h)

**PURGE vs BAN:**

- **PURGE**: Exact URL removal. Chaperone sends `PURGE /path` with `Host: hostname`. Varnish looks up and removes the single cached object.
- **BAN**: Regex pattern invalidation. Chaperone sends `BAN /pattern` with `Host: hostname`. Uses ban lurker-friendly expressions (`obj.http.x-cache-host`, `obj.http.x-cache-url`) for efficient background cleanup.

**Ban Lurker Headers:**

The VCL preamble stores `x-cache-host` and `x-cache-url` on cached objects in `vcl_backend_response` and strips them in `vcl_deliver`. These enable efficient ban lurker matching.

**Example:**

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: VarnishCacheInvalidation
metadata:
  name: purge-user-pages
spec:
  gatewayRef:
    name: my-gateway
  type: Purge
  hostname: api.example.com
  paths:
    - /api/users/123
    - /api/users/456
    - /api/users/789
```

### Ghost ↔ VCL Communication

Ghost should use the Varnish C API directly whenever possible instead of setting internal
request headers for VCL to interpret. Headers are a last resort for values that must cross
VCL subroutine boundaries (e.g., `vcl_recv` → `vcl_backend_response`).

**Use header + postamble for VCL flow control:**
- `X-Ghost-Pass` header — signals pass mode; the postamble `vcl_recv` (appended after
  user VCL) checks this header and calls `return(pass)`. This ensures user VCL runs
  before the pass decision takes effect. Do NOT use `ctx.set_pass()` as it terminates
  VCL execution immediately, preventing user VCL from running.

**Use headers only for cross-subroutine bridging** (values set in `vcl_recv` that must
reach `vcl_backend_response` via `bereq`):
- `X-Ghost-Default-TTL` / `X-Ghost-Forced-TTL` — TTL control
- `X-Ghost-Grace` / `X-Ghost-Keep` — stale serving parameters
- `X-Ghost-Cache-Key-Extra` — additional hash data

**Default cache behavior**: Without a VarnishCachePolicy, ghost sets `pass` on all
routes — behaving as a plain reverse proxy with no caching, consistent with Gateway API
expectations.

## Key Dependencies

```
sigs.k8s.io/controller-runtime    # operator framework
sigs.k8s.io/gateway-api           # Gateway API types
k8s.io/client-go                  # Kubernetes client and informers
github.com/perbu/vclparser        # optional: VCL syntax validation
```

## VCL Merging Strategy

VCL allows multiple definitions of the same subroutine - they get concatenated at compile time. We use this
to avoid parsing user VCL:

1. Generated VCL includes: ghost import, `vcl_init` (ghost.init, router creation), `vcl_recv` (ghost.recv), `vcl_backend_fetch` (router.backend)
2. User VCL is appended after the generated VCL
3. If user defines the same subroutines, their code runs *after* the gateway code

Users who need pre-routing logic (URL normalization, etc.) should define their own `vcl_recv`.

**Important:** Avoid generating `vcl_synth` and `vcl_backend_error` in the gateway VCL:
- These subroutines are commonly customized by users for error handling and synthetic responses
- If our generated VCL includes `return` statements in these subroutines, user VCL won't run (VCL execution stops at first return)
- This creates unexpected conflicts where user customizations are silently ignored
- Instead, handle errors and special cases in `vcl_recv` before backend selection, or provide VMOD methods users can call explicitly

### Synthetic Response Pattern for VMODs

When the ghost VMOD needs to generate synthetic responses (e.g., 404 for unknown vhosts), we use the **synthetic backend pattern** instead of VCL error handlers:

1. **Create a synthetic backend** - A struct implementing `VclBackend` trait that generates responses programmatically
2. **Populate beresp in get_response()** - Set status, headers, and body content directly
3. **Return from director** - The director returns the synthetic backend instead of `None`

Example from ghost's NotFoundBackend:
```rust
impl VclBackend<NotFoundBody> for NotFoundBackend {
    fn get_response(&self, ctx: &mut Ctx) -> Result<Option<NotFoundBody>, VclError> {
        let beresp = ctx.http_beresp.as_mut().unwrap();
        beresp.set_status(404);
        beresp.set_header("Content-Type", "application/json")?;
        Ok(Some(NotFoundBody { ... }))
    }
}
```

**Benefits:**
- No VCL generation needed for error handling
- Zero conflict with user VCL
- Responses generated entirely within the VMOD
- User can still customize via `vcl_backend_response` or `vcl_deliver` if needed

**See:** `ghost/src/not_found_backend.rs` for the implementation

## File Formats

### routing.json (operator output, stored in ConfigMap)

Generated by operator from HTTPRoutes. Maps hostnames to routes with match criteria:

```json
{
  "version": 2,
  "vhosts": {
    "api.example.com": {
      "routes": [
        {
          "hostname": "api.example.com",
          "path_match": {"type": "PathPrefix", "value": "/v2"},
          "service": "api-v2",
          "namespace": "default",
          "port": 8080,
          "weight": 100,
          "listeners": ["http-80"],
          "route_name": "default/api-v2-route",
          "priority": 10300,
          "rule_index": 0
        }
      ]
    }
  }
}
```

The `listeners` field contains Varnish socket names (`{proto}-{port}`) that this route
applies to. Empty or absent means the route matches on all listeners.

### ghost.json (chaperone output, consumed by ghost VMOD)

Generated by chaperone by merging routing.json with EndpointSlice discoveries.

Uses **backend groups** for correct weighted traffic distribution. Weights belong to the
service group, not individual pods. Selection is two-level: (1) pick a group by weight,
(2) pick a random pod within the group. This ensures traffic split matches configured
weights regardless of replica counts.

If a group has no backends (service has no ready endpoints), requests selecting that group
return 500. Traffic is **not** redistributed to other groups — this matches the Gateway API
spec behavior (consistent with Envoy Gateway).

```json
{
  "version": 2,
  "vhosts": {
    "api.example.com": {
      "routes": [
        {
          "path_match": {"type": "PathPrefix", "value": "/v2"},
          "backend_groups": [
            {
              "weight": 100,
              "backends": [
                {"address": "10.0.0.1", "port": 8080},
                {"address": "10.0.0.2", "port": 8080}
              ]
            }
          ],
          "listeners": ["http-80"],
          "route_name": "default/api-v2-route",
          "priority": 10300,
          "rule_index": 0
        }
      ]
    }
  }
}
```

## Operator Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `GATEWAY_CLASS_NAME` | `varnish` | Which GatewayClass this operator manages |
| `GATEWAY_IMAGE` | `ghcr.io/varnish/varnish-gateway:latest` | Combined varnish+ghost+chaperone image |
| `IMAGE_PULL_SECRETS` | `` | Comma-separated list of image pull secret names for chaperone pods |

## Deployment

### Helm (Recommended)

The easiest way to install Varnish Gateway is using the Helm chart:

```bash
helm install varnish-gateway ./charts/varnish-gateway \
  --namespace varnish-gateway-system \
  --create-namespace
```

The Helm chart includes:
- Gateway API CRDs (GatewayClass, Gateway, HTTPRoute, etc.)
- Varnish-specific CRD (GatewayClassParameters)
- Operator deployment with RBAC
- Default GatewayClass configuration

See [INSTALL.md](INSTALL.md) for detailed installation instructions and [charts/varnish-gateway/README.md](charts/varnish-gateway/README.md) for configuration options.

### kubectl (Alternative)

If you prefer not to use Helm, Kubernetes manifests are available in `deploy/`:

```
deploy/
├── 00-prereqs.yaml       # Namespace + GatewayClassParameters CRD
├── 01-operator.yaml      # ServiceAccount, ClusterRole, ClusterRoleBinding, Deployment
├── 02-chaperone-rbac.yaml # ClusterRole for chaperone to watch EndpointSlices
├── 03-gatewayclass.yaml  # GatewayClass "varnish"
└── sample-gateway.yaml   # Sample Gateway (not applied by make deploy)
```

Deploy to cluster:

```bash
# Install Gateway API CRDs first
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml

# Deploy the operator
kubectl apply -f deploy/
```

## Testing

### Test Infrastructure

The project uses **envtest** for controller integration tests. Envtest provides a real Kubernetes API server and etcd, allowing us to test Server-Side Apply (SSA) and other features that don't work with fake clients.

**Test files:**
- `internal/controller/*_test.go` - Unit tests using fake clients
- `internal/controller/*_envtest_test.go` - Integration tests using envtest
- `internal/controller/testdata/` - Gateway API CRDs for envtest

### Running Tests

```bash
# Run all unit/integration tests (Go + Rust)
make test

# Run only Go tests (includes envtest setup)
make test-go

# Run only envtest integration tests
make test-envtest
```

### Conformance Tests

The Gateway API conformance suite is the primary end-to-end test harness. It requires a live cluster with the operator deployed.

```bash
# Run full conformance suite (~3.5 minutes)
make test-conformance

# Run a single conformance test (~1 minute)
make test-conformance-single TEST=HTTPRouteMethodMatching

# Run with report output
make test-conformance-report
```

**Note:** Envtest downloads ~50MB of binaries (kube-apiserver, etcd) to `testbin/` on first run. These are cached for subsequent test runs.

### Why Envtest?

The Gateway controller uses **Server-Side Apply (SSA)** to manage resources. SSA requires a real API server to compute field ownership and handle conflicts. The fake client doesn't support SSA, so we use envtest for integration tests that verify:
- Resource creation with proper field managers
- Conflict resolution between multiple controllers
- Status updates on Gateway and HTTPRoute resources

See `internal/controller/gateway_controller_envtest_test.go` for examples.

## Running Locally

```bash
# Build binaries
make build-go

# Build Docker images
make docker

# Run operator locally (uses ~/.kube/config)
GATEWAY_CLASS_NAME=varnish \
GATEWAY_IMAGE=varnish-gateway:local \
./dist/operator

# Run chaperone standalone (uses ~/.kube/config)
# Note: Set thread_pool_stack for regex support in debug builds
# VARNISH_LISTEN uses {proto}-{port}=:{port},{proto} format, semicolon-separated
NAMESPACE=default \
VARNISH_ADMIN_PORT=6082 \
VARNISH_HTTP_ADDR=localhost:8080 \
VARNISH_LISTEN="http-8080=:8080,http" \
VCL_PATH=/tmp/main.vcl \
ROUTING_CONFIG_PATH=/tmp/routing.json \
GHOST_CONFIG_PATH=/tmp/ghost.json \
WORK_DIR=/tmp/varnish \
HEALTH_ADDR=:8081 \
VARNISHD_EXTRA_ARGS="-p;thread_pool_stack=160k" \
./dist/chaperone
```

## Docker Registry

Images are pushed to GitHub Container Registry (public):

```
ghcr.io/varnish/gateway-operator
ghcr.io/varnish/gateway-chaperone
ghcr.io/varnish/varnish-ghost
```

**Automated CI**: Tests run automatically via GitHub Actions on push to `main` and pull requests.

**Automated builds**: Images are built and pushed automatically via GitHub Actions on version tags (`v*`) only, tagged with both the version and `latest`.

**Manual builds**:

```bash
# Build locally
make docker

# Build and push (requires ghcr.io authentication)
echo $GITHUB_TOKEN | docker login ghcr.io -u USERNAME --password-stdin
make docker-push
```

## Conventions

### Go Code
- Use `log/slog` for logging
- Use stdlib `testing` package (no ginkgo/gomega)
- Vendor dependencies (`go mod vendor`)
- Format with `gofmt`
- Error returns should follow this pattern: `fmt.Errorf("io.Open(%s): %w", filename, err)`

### Testing Conventions
- **Unit tests**: Use fake clients for simple logic tests (`*_test.go`)
- **Integration tests**: Use envtest for tests requiring real API server (`*_envtest_test.go`)
- Use envtest when testing:
  - Server-Side Apply (SSA)
  - Field manager conflicts
  - Status subresource updates
  - Complex API server behavior
- Add `//go:build !race` to envtest files (envtest doesn't work with race detector)

### Ghost VMOD (Rust)
See `ghost/CLAUDE.md` for Rust-specific conventions, build instructions, and testing details.
