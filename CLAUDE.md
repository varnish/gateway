# Varnish Gateway Operator - Development Guide

## Project Overview

Kubernetes Gateway API implementation using Varnish. Three components:
- **operator**: watches Gateway API resources, generates ghost.json config, manages deployments
- **chaperone**: handles endpoint discovery and triggers ghost reload
- **ghost**: Rust VMOD that handles all routing logic internally (replaces nodes/udo/activedns vmods)

See PLAN2.md for full architecture (supersedes PLAN.md).

## Progress

### Phase 1 Complete

All three components (operator, chaperone, ghost) are now at Phase 1 completion.

**Operator** (`cmd/operator/main.go`) - Phase 1 Complete:
- Gateway controller - creates Deployment, Service, ConfigMap, Secret, ServiceAccount
- HTTPRoute controller - watches routes, regenerates routing.json on changes
- Status conditions (Accepted/Programmed) on Gateway and HTTPRoute
- GatewayClassParameters CRD for user VCL injection
- Single-container deployment model (combined varnish+ghost+chaperone image)
- VCL generator produces ghost preamble (no complex routing VCL)
- ConfigMap contains `main.vcl` and `routing.json`

**Chaperone** (`cmd/chaperone/main.go`) - Phase 1 Complete:
- Environment variable configuration with defaults
- Kubernetes client (in-cluster + kubeconfig fallback for local dev)
- Graceful shutdown on SIGTERM/SIGINT
- Health endpoint for k8s probes
- Uses vrun package to start and manage varnishd process
- Integrates ghost watcher for dynamic backend/routing updates
- VCL reloader for user VCL hot-reload via varnishadm
- Combined Docker image (`docker/chaperone.Dockerfile`) includes chaperone + varnish + ghost

**Ghost package** (`internal/ghost/`) - Phase 1 Complete:
- `config.go` - Types for ghost.json and routing.json configuration
- `generator.go` - Merges routing rules (from operator) with EndpointSlices to produce ghost.json
- `watcher.go` - Watches routing config + EndpointSlices, regenerates ghost.json, triggers HTTP reload

**Reload package** (`internal/reload/`):
- `client.go` - HTTP client to trigger ghost VMOD reload via `/.varnish-ghost/reload` endpoint

**VCL package** (`internal/vcl/`):
- `generator.go` - Generates ghost preamble VCL (imports ghost, initializes router)
- `merge.go` - Merges generated VCL with user VCL
- `reloader.go` - Watches main.vcl, hot-reloads via varnishadm with garbage collection
- `types.go` - Backend info types for routing config generation

**Varnishadm package** (`internal/varnishadm/`):
- Full varnishadm protocol implementation (reverse mode, -M flag)
- Authentication, VCL commands, parameter commands, TLS cert commands

**Vrun package** (`internal/vrun/`):
- `process.go` - Manager for varnishd process lifecycle (start, workspace prep, secret generation)
- `config.go` - Config struct and BuildArgs for constructing varnishd command-line arguments
- `logwriter.go` - Routes varnishd stdout/stderr through slog
- Start() is non-blocking; returns ready channel, call Wait() to block until exit
- VCL not loaded at startup (`-f ""`); load via admin socket after start

**Ghost VMOD** (`ghost/`) - Phase 1 Complete:
- Rust-based VMOD replacing nodes/udo/activedns vmod trio
- JSON configuration loading with validation (`src/config.rs`)
- Host matching with exact and wildcard support (`src/routing.rs`)
- Weighted backend selection with random distribution
- Async HTTP client with connection pooling (`src/runtime.rs`)
- Streaming response bodies via tokio channels (`src/response.rs`)
- Hot-reload via `/.varnish-ghost/reload` endpoint
- 29 unit tests, VTC integration test framework ready
- Builds against Varnish 8.0 with `varnish` crate 0.5.5

### Not Yet Implemented

See TODO.md for details.

## Development Setup

### Prerequisites

- Go 1.21+
- Rancher Desktop (or any local k8s)
- kubectl configured to talk to your cluster

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

2. **Chaperone** runs as wrapper for varnishd
   - Starts varnishd via vrun package
   - Watches `routing.json` + EndpointSlices
   - Merges them into `ghost.json` (host → actual pod IPs)
   - Triggers ghost reload via HTTP endpoint

3. **Ghost VMOD** handles all routing inside Varnish
   - Reads `ghost.json` on reload
   - Routes requests by hostname to weighted backends
   - Streams responses via async HTTP client

### Two Reload Paths

- **VCL changes** (user VCL updates): varnishadm hot-reload
- **Backend/routing changes**: ghost HTTP reload (`/.varnish-ghost/reload`)

## Key Dependencies

```
sigs.k8s.io/controller-runtime    # operator framework
sigs.k8s.io/gateway-api           # Gateway API types
github.com/fsnotify/fsnotify      # file watching (chaperone)
github.com/perbu/vclparser        # optional: VCL syntax validation
```

## VCL Merging Strategy

VCL allows multiple definitions of the same subroutine - they get concatenated at compile time. We use this
to avoid parsing user VCL:

1. Generated VCL includes: ghost import, `vcl_init` (ghost.init, router creation), `vcl_recv` (ghost.recv), `vcl_backend_fetch` (router.backend)
2. User VCL is appended after the generated VCL
3. If user defines the same subroutines, their code runs *after* the gateway code

Users who need pre-routing logic (URL normalization, etc.) should define their own `vcl_recv`.

## File Formats

### routing.json (operator output, stored in ConfigMap)

Generated by operator from HTTPRoutes. Maps hostnames to service names/ports:

```json
{
  "version": 1,
  "vhosts": {
    "api.example.com": {
      "service": "api-service",
      "namespace": "default",
      "port": 8080,
      "weight": 100
    }
  }
}
```

### ghost.json (chaperone output, consumed by ghost VMOD)

Generated by chaperone by merging routing.json with EndpointSlice discoveries:

```json
{
  "version": 1,
  "vhosts": {
    "api.example.com": {
      "backends": [
        {"address": "10.0.0.1", "port": 8080, "weight": 100},
        {"address": "10.0.0.2", "port": 8080, "weight": 100}
      ]
    },
    "*.staging.example.com": {
      "backends": [
        {"address": "10.0.2.1", "port": 8080, "weight": 100}
      ]
    }
  },
  "default": {
    "backends": [
      {"address": "10.0.99.1", "port": 80, "weight": 100}
    ]
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

Kubernetes manifests are in `deploy/`:

```
deploy/
├── 00-prereqs.yaml       # Namespace + GatewayClassParameters CRD
├── 01-operator.yaml      # ServiceAccount, ClusterRole, ClusterRoleBinding, Deployment
├── 02-chaperone-rbac.yaml # ClusterRole for chaperone to watch EndpointSlices
├── 03-gatewayclass.yaml  # GatewayClass "varnish"
└── 04-sample-gateway.yaml # Sample Gateway + HTTPRoutes for testing
```

Deploy to cluster:

```bash
# Install Gateway API CRDs first
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml

# Deploy the operator
kubectl apply -f deploy/
```

## Running Locally

```bash
# Run tests
go test ./...

# Build binaries
make build-go

# Build Docker images
make docker

# Run operator locally (uses ~/.kube/config)
GATEWAY_CLASS_NAME=varnish \
GATEWAY_IMAGE=varnish-gateway:local \
./dist/operator

# Run chaperone standalone (uses ~/.kube/config)
NAMESPACE=default \
VARNISH_ADMIN_PORT=6082 \
VARNISH_HTTP_ADDR=localhost:8080 \
VCL_PATH=/tmp/main.vcl \
ROUTING_CONFIG_PATH=/tmp/routing.json \
GHOST_CONFIG_PATH=/tmp/ghost.json \
WORK_DIR=/tmp/varnish \
HEALTH_ADDR=:8081 \
./dist/chaperone
```

## Docker Registry

Images are pushed to DigitalOcean Container Registry:

```
registry.digitalocean.com/varnish-gateway/gateway-operator
registry.digitalocean.com/varnish-gateway/gateway-chaperone
```

Build and push:

```bash
# Authenticate
doctl registry login

# Build and push (single arch)
make docker-push

# Build and push (multi-arch: amd64 + arm64)
make docker-buildx
```

## Conventions

### Go Code
- Use `log/slog` for logging
- Use stdlib `testing` package (no ginkgo/gomega)
- Vendor dependencies (`go mod vendor`)
- Format with `gofmt`
- Error returns should follow this pattern: `fmt.Errorf("io.Open(%s): %w", filename, err)`

### Ghost VMOD (Rust)
- Uses `varnish` crate 0.5.5 for VMOD bindings
- All public functions exposed via `#[vmod]` macro
- Configuration validated on load; invalid configs rejected
- Connection pooling via reqwest persists across config reloads
- Response bodies streamed via tokio channels (not buffered)
- Build: `cargo build --release` produces `target/release/libvmod_ghost.so`
- Tests: `cargo test` runs 29 unit tests
- Docker build available via `ghost/Dockerfile`
