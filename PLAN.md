# Varnish Gateway Operator

The goal is to build a Kubernetes operator and Varnish sidecar that can act as an implementation
of the [Gateway API](https://gateway-api.sigs.k8s.io/) spec.

## Component Responsibilities

The system consists of two binaries:

**Operator** - Runs as a cluster-wide deployment. Watches Gateway API resources (Gateway, HTTPRoute, etc.) and
translates them into Varnish configuration. When routes change, it generates VCL routing logic, merges it with
user-provided VCL, and writes the result to a ConfigMap. It also manages the lifecycle of Varnish deployments - creating
pods, services, and other resources when a Gateway is created.

**Sidecar** - Runs alongside each Varnish instance. Handles two runtime concerns that the operator cannot: (1) endpoint
discovery - watches Kubernetes EndpointSlices for backend services and writes the backends.conf file that the nodes vmod
reads, and (2) VCL reloading - watches for ConfigMap changes and hot-reloads VCL into Varnish via varnishadm. The
sidecar runs in the same pod as Varnish and communicates with it over localhost.

The split exists because the operator works at the configuration level (what should exist) while the sidecar works at
the runtime level (what's happening now). Backend IPs change frequently as pods scale; this is handled by the sidecar
without requiring VCL recompilation.

Repository Structure
--------------------
Everything is in one repo. Shared CRD types, coordinated versioning, single release. Two binaries from one module.

```
varnish-gateway/
├── cmd/
│   ├── operator/
│   └── sidecar/
├── api/
│   └── v1alpha1/
├── internal/
│   ├── controller/
│   ├── vcl/
│   ├── backends/
│   ├── varnishadm/
│   └── status/
├── config/
│   ├── crd/
│   ├── rbac/
│   └── manager/
├── deploy/
│   └── helm/
└── go.mod
```

* * * * *

Operator
--------

### Packages

**`cmd/operator/`**

Entry point. Wires up the manager, registers controllers, starts health/ready endpoints.

**`api/v1alpha1/`**

Custom resource definitions:

- `GatewayClassParameters` - per-class config (default memory, storage settings, image to use)
- `VarnishConfig` - user VCL attachment to a Gateway

These get registered with the scheme alongside Gateway API types.

**`internal/controller/gateway_controller.go`**

Watches: Gateway, GatewayClass

Responsibilities:

- When Gateway created: create Deployment (varnish + sidecar), Service, ConfigMaps for VCL
- When Gateway updated: update Deployment if parameters change
- When Gateway deleted: clean up owned resources
- Set status conditions (Accepted, Programmed)

**`internal/controller/httproute_controller.go`**

Watches: HTTPRoute, ReferenceGrant

Responsibilities:

- Validate ReferenceGrants for cross-namespace references
- Collect all HTTPRoutes attached to a Gateway
- Trigger VCL regeneration when routes change
- Set status conditions on HTTPRoute (Accepted, ResolvedRefs)

**`internal/controller/varnishconfig_controller.go`**

Watches: VarnishConfig, referenced ConfigMaps (user VCL)

Responsibilities:

- Fetch user VCL from ConfigMap
- Trigger VCL regeneration
- Validate user VCL compiles

**`internal/vcl/generator.go`**

Generates two VCL subroutines from HTTPRoutes:

1. **`vcl_init`** - Creates nodes config groups and udo directors per service
2. **`gateway_backend_fetch`** - Called from vcl_backend_fetch, routes requests to the appropriate backend

Example generated VCL:

```vcl
import nodes;
import udo;

sub vcl_init {
    new svc_foo_conf = nodes.config_group("/var/run/varnish/backends.conf", "svc_foo");
    new svc_foo_dir = udo.director(hash);
    svc_foo_dir.subscribe(svc_foo_conf.get_tag());

    new svc_bar_conf = nodes.config_group("/var/run/varnish/backends.conf", "svc_bar");
    new svc_bar_dir = udo.director(hash);
    svc_bar_dir.subscribe(svc_bar_conf.get_tag());
}

# Note: Request inspection (auth, rate limiting, etc.) can be done in vcl_recv
# before the backend fetch. The gateway does not inject into vcl_recv by default,
# but user VCL can add custom logic there.

sub gateway_backend_fetch {
    if (bereq.http.host == "foo.example.com" && bereq.url ~ "^/api/") {
        set bereq.backend = svc_foo_dir.backend();
        return;
    }
    if (bereq.http.host == "foo.example.com" && bereq.url ~ "^/static/") {
        set bereq.backend = svc_bar_dir.backend();
        return;
    }
    # No match - falls through to default backend or returns 503
}
```

**`internal/vcl/merge.go`**

Combines user VCL + generated routing using the VCL parser (github.com/perbu/vclparser):

1. Parse user VCL into AST using `parser.Parse()`
2. Find `vcl_backend_fetch` SubDecl - iterate `Program.Declarations`, type assert to `*ast.SubDecl`, check
   `Name == "vcl_backend_fetch"`
3. Check if first statement is already `call gateway_backend_fetch;` (check for `*ast.CallStatement` with function name)
4. If not present, prepend a `CallStatement` to `Body.Statements`
5. Prepend the generated subroutines and imports to `Program.Declarations`
6. Render back to VCL using `renderer.Render(program)`

If user VCL doesn't have `vcl_backend_fetch`, generate a default one that calls the gateway subroutine.

**`internal/status/conditions.go`**

Helper functions for setting Gateway API status conditions. Tedious but necessary.

### Configuration

Environment or flags:

| Name                    | Description                              |
|-------------------------|------------------------------------------|
| `GATEWAY_CLASS_NAME`    | Which GatewayClass this operator manages |
| `DEFAULT_VARNISH_IMAGE` | Default image for varnish container      |
| `SIDECAR_IMAGE`         | Image for sidecar container              |
| `METRICS_ADDR`          | Prometheus metrics endpoint              |
| `HEALTH_PROBE_ADDR`     | Health/ready probes                      |
| `LEADER_ELECTION`       | Enable leader election (for HA)          |

### Generated Files

The operator writes these files to a shared volume (ConfigMap mounted as volume):

```
/var/run/varnish/
├── main.vcl          # Merged VCL (user VCL + generated routing)
└── services.json     # Service list for sidecar endpoint discovery
```

**services.json format:**

```json
{
  "services": [
    {
      "name": "svc_foo",
      "port": 8080
    },
    {
      "name": "svc_bar",
      "port": 8080
    }
  ]
}
```

The `name` field matches the INI section in backends.conf. Extensible for future fields (protocol, weight, etc.).

---

## Sidecar

### Packages

**`cmd/sidecar/`**

Entry point. Starts services file watcher, endpoint watcher, VCL reload listener, health server.

**`internal/backends/watcher.go`**

Watches Kubernetes EndpointSlices for services listed in `services.json`.

Responsibilities:

- Watch `services.json` for changes (fsnotify)
- When services.json changes: update the set of EndpointSlice watches
- When EndpointSlices change: regenerate backends.conf, write to disk (nodes vmod auto-reloads)

**`internal/backends/nodes_file.go`**

Generates the INI-like file format that nodes vmod expects:

```ini
# Generated by varnish-gateway sidecar
# Do not edit - this file is auto-generated

[svc_foo]
pod_10_0_0_1 = 10.0.0.1:8080
pod_10_0_0_2 = 10.0.0.2:8080

[svc_bar]
pod_10_0_1_1 = 10.0.1.1:8080
```

Key points:

- Sections are required - entries outside sections are ignored by udo
- Backend names must be valid identifiers (replace dots/colons with underscores)
- Port is required (no default)
- The file is watched by nodes vmod, so updates are picked up without VCL reload

**`internal/varnishadm/client.go`**

Talks to varnishadm over the admin socket:

- `vcl.load <name> <path>`
- `vcl.use <name>`
- `vcl.discard <name>`
- `vcl.list` (for garbage collection of old VCL versions)
- `ping` (for health checks)

**`internal/vcl/reloader.go`**

Watches for VCL changes (main.vcl). On change:

1. Load new VCL via varnishadm with timestamped name
2. If success: switch to it, discard old versions (keep last N for rollback)
3. If failure: log error, keep running old VCL, expose metric for alerting

### Configuration

Environment or flags:

| Name                  | Description                                                              |
|-----------------------|--------------------------------------------------------------------------|
| `VARNISH_ADMIN_ADDR`  | varnishadm socket (e.g., `localhost:6082`)                               |
| `VARNISH_SECRET_PATH` | Path to admin secret file                                                |
| `BACKENDS_FILE_PATH`  | Where to write backends.conf (default: `/var/run/varnish/backends.conf`) |
| `VCL_PATH`            | Path to watch for VCL changes (default: `/var/run/varnish/main.vcl`)     |
| `SERVICES_FILE_PATH`  | Path to services.json (default: `/var/run/varnish/services.json`)        |
| `NAMESPACE`           | Namespace to watch endpoints in (usually from downward API)              |

---

## Dependencies

**Core:**

```
sigs.k8s.io/controller-runtime    # operator framework
k8s.io/client-go                  # kubernetes client (pulled in by controller-runtime)
sigs.k8s.io/gateway-api           # Gateway API types
```

**VCL Processing:**

```
github.com/perbu/vclparser        # VCL parser and renderer
```

**Utility:**

```
github.com/fsnotify/fsnotify      # file watching (for sidecar)
```

Logging uses the standard library `log/slog`.

**Testing:**

```
sigs.k8s.io/controller-runtime/pkg/envtest  # test k8s API server
```

Unit tests use the standard library `testing` package.

---

## Design Decisions

1. **Empty backends** - When a service has zero ready endpoints, udo returns NULL and Varnish issues a 503. This is
   correct behavior - service unavailable = 503. No special handling needed.

2. **Health checks** - Rely on Kubernetes readiness probes. Pods are removed from EndpointSlices when unhealthy. No udo
   health probes.

3. **Hash key** - Use udo's default (vcl_hash, typically req.url + host). Good enough for v1.

4. **VCL validation** - The parser validates syntax. Full compilation check (`varnishd -C`) deferred for now.

---

## Open Questions

1. **Cross-namespace services** - HTTPRoute can reference services in other namespaces (with ReferenceGrant). The
   sidecar needs permission to watch EndpointSlices across namespaces. Defer to v1alpha2? Start with same-namespace
   only?
