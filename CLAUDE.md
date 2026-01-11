# Varnish Gateway Operator - Development Guide

## Project Overview

Kubernetes Gateway API implementation using Varnish. Two binaries:
- **operator**: watches Gateway API resources, generates VCL, manages deployments
- **sidecar**: handles endpoint discovery and VCL hot-reloading

See PLAN.md for full architecture.

## Progress

### Completed

**Sidecar** (`cmd/sidecar/main.go`):
- Environment variable configuration with defaults
- Kubernetes client (in-cluster + kubeconfig fallback for local dev)
- Graceful shutdown on SIGTERM/SIGINT
- Health endpoint for k8s probes
- Component integration: varnishadm server, backends watcher, VCL reloader

**Backends package** (`internal/backends/`):
- `nodes_file.go` - Generates INI-format backends.conf for nodes vmod
- `services.go` - Parses services.json
- `watcher.go` - Watches services.json + EndpointSlices, regenerates backends.conf

**VCL package** (`internal/vcl/`):
- `reloader.go` - Watches main.vcl, hot-reloads via varnishadm with garbage collection

**Varnishadm package** (`internal/varnishadm/`):
- Full varnishadm protocol implementation (reverse mode, -M flag)
- Authentication, VCL commands, parameter commands, TLS cert commands

### Not Started

- Operator binary
- VCL generator (from HTTPRoutes)
- Gateway/HTTPRoute controllers
- Custom resource definitions (GatewayClassParameters, VarnishConfig)

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

## Development Approach

### Phase 1: File Generation (No Varnish Required)

Start by building and testing the file generators in isolation:

1. **VCL Generator** (`internal/vcl/generator.go`)
   - Input: list of HTTPRoutes
   - Output: generated VCL string
   - Test: unit tests with expected VCL output

2. **VCL Merger** (`internal/vcl/merge.go`)
   - Input: generated VCL + user VCL
   - Output: concatenated VCL string (generated first, user appended)
   - VCL's subroutine concatenation feature means no parsing needed
   - Test: unit tests verifying correct ordering

3. **Backends File Generator** (`internal/backends/nodes_file.go`)
   - Input: map of service name → list of endpoints
   - Output: INI-format backends.conf string
   - Test: unit tests

4. **Services File Generator**
   - Input: list of services from HTTPRoutes
   - Output: services.json
   - Test: unit tests

Write files to a local directory and manually inspect them. No k8s needed for this phase.

### Phase 2: Kubernetes Integration

Once file generation is solid:
- Add controllers that watch Gateway API resources
- Write generated files to ConfigMaps
- Test with real k8s resources

### Phase 3: Varnish Integration

Finally:
- Deploy Varnish Enterprise pods with sidecar
- Test end-to-end routing

## Key Dependencies

```
sigs.k8s.io/controller-runtime    # operator framework
sigs.k8s.io/gateway-api           # Gateway API types
github.com/fsnotify/fsnotify      # file watching (sidecar)
github.com/perbu/vclparser        # optional: VCL syntax validation
```

## VCL Merging Strategy

VCL allows multiple definitions of the same subroutine - they get concatenated at compile time. We use this
to avoid parsing user VCL:

1. Generated VCL includes: imports, `vcl_init`, `gateway_backend_fetch`, and a `vcl_backend_fetch` that calls it
2. User VCL is appended after the generated VCL
3. If user defines `vcl_backend_fetch`, their code runs *after* the gateway routing call

Users who need pre-routing logic (URL normalization, etc.) should use `vcl_recv` instead.

## File Formats

### backends.conf (nodes vmod INI format)

```ini
[svc_foo]
pod_10_0_0_1 = 10.0.0.1:8080
pod_10_0_0_2 = 10.0.0.2:8080

[svc_bar]
pod_10_0_1_1 = 10.0.1.1:8080
```

### services.json

```json
{
  "services": [
    {"name": "svc_foo", "port": 8080},
    {"name": "svc_bar", "port": 8080}
  ]
}
```

## Running Locally

```bash
# Run tests
go test ./...

# Build both binaries
go build ./cmd/operator
go build ./cmd/sidecar

# Run sidecar locally (uses ~/.kube/config)
NAMESPACE=default \
VARNISH_SECRET_PATH=/tmp/secret \
SERVICES_FILE_PATH=/tmp/services.json \
BACKENDS_FILE_PATH=/tmp/backends.conf \
VCL_PATH=/tmp/main.vcl \
./sidecar
```

## Conventions

- Use `log/slog` for logging
- Use stdlib `testing` package (no ginkgo/gomega)
- Vendor dependencies (`go mod vendor`)
- Format with `gofmt`
- Error returns should follow this pattern return `fmt.Errorf("io.Open(%s): %w", filename, err)`
