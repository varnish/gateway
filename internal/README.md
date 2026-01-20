# Internal Packages

This directory contains the core Go packages for the Varnish Gateway Operator.

## Package Overview

```
internal/
├── controller/   # Kubernetes reconciliation loops (heart of the operator)
├── ghost/        # Ghost config generation and endpoint watching
├── reload/       # HTTP client for ghost VMOD reload
├── status/       # Gateway API status condition helpers
├── varnishadm/   # Varnish admin protocol implementation
├── vcl/          # VCL generation, merging, and hot-reload
└── vrun/         # Varnishd process lifecycle management
```

## Reconciliation Loops

The following packages contain reconciliation loops - the core control logic that makes this operator work:

### controller (Operator Reconciliation)

Location: `controller/gateway_controller.go`, `controller/httproute_controller.go`

This is the heart of the operator. Contains two Kubernetes controllers:

- GatewayReconciler - Watches `Gateway` resources and reconciles child resources:
  - Creates/updates Deployment, Service, ConfigMap, Secret, ServiceAccount
  - Sets Gateway status conditions (Accepted/Programmed)
  - Handles Gateway deletion via finalizers

- HTTPRouteReconciler - Watches `HTTPRoute` resources and updates routing:
  - Generates `routing.json` from HTTPRoute specs
  - Merges generated VCL with user VCL
  - Updates ConfigMaps when routes change
  - Sets HTTPRoute status conditions

Both controllers use controller-runtime's reconciliation pattern and are registered with the manager in `cmd/operator/main.go`.

### ghost (Chaperone Reconciliation)

Location: `ghost/watcher.go`

The Watcher implements a reconciliation loop that runs inside the chaperone:

- Watches `routing.json` (from operator) via fsnotify
- Watches Kubernetes EndpointSlices via informers
- Merges routing rules with discovered endpoints to produce `ghost.json`
- Triggers ghost VMOD reload when backends change

This is the runtime reconciliation that keeps backends in sync with pod IPs.

### vcl (VCL Hot-Reload Loop)

Location: `vcl/reloader.go`

The Reloader watches `main.vcl` and performs hot-reloads:

- Watches VCL file via fsnotify
- Loads new VCL into Varnish via varnishadm
- Activates the new VCL
- Garbage collects old VCL versions

---

## Supporting Packages

### ghost

Files: `config.go`, `generator.go`, `watcher.go`

Configuration types and generation logic for the ghost VMOD:

- `Config` / `RoutingConfig` - JSON schema types
- `Generate()` - Merges routing rules with endpoints to create ghost.json
- `GenerateRoutingConfig()` - Creates routing config from HTTPRoute backends

### reload

Files: `client.go`

HTTP client for triggering ghost config reloads via the `/.varnish-ghost/reload` endpoint.

### status

Files: `conditions.go`

Helper functions for setting Gateway API status conditions:

- `SetGatewayAccepted()` / `SetGatewayProgrammed()`
- `SetHTTPRouteAccepted()` / `SetHTTPRouteResolvedRefs()`

### varnishadm

Files: `varnishadm.go`, `commands.go`, `parsers.go`

Full implementation of the Varnish admin protocol:

- Runs in reverse mode (listens for varnishd connections via `-M` flag)
- Handles authentication challenge/response
- VCL commands: load, use, discard, list
- Parameter commands: get, set
- TLS certificate management

### vcl

Files: `generator.go`, `merge.go`, `reloader.go`

VCL handling:

- `Generate()` - Creates ghost preamble VCL (imports, init, recv, backend_fetch)
- `Merge()` - Concatenates generated VCL with user VCL
- `CollectHTTPRouteBackends()` - Extracts backend info from HTTPRoutes

### vrun

Files: `process.go`, `config.go`, `logwriter.go`

Varnishd process management:

- `Manager` - Handles process lifecycle (start, wait, workspace prep)
- `Config` - Builds varnishd command-line arguments
- `PrepareWorkspace()` - Creates working directory and secret file
- `Start()` - Non-blocking start, returns ready channel
- `Wait()` - Blocks until process exits
