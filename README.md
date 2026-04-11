<p align="center">
  <img src="assets/varnish-gateway-logo.svg" alt="Varnish Gateway" width="360">
</p>

<p align="center">
  <a href="https://github.com/varnish/gateway/actions/workflows/ci.yml"><img src="https://github.com/varnish/gateway/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://codecov.io/github/varnish/gateway"><img src="https://codecov.io/github/varnish/gateway/graph/badge.svg" alt="codecov"></a>
</p>

Kubernetes [Gateway API](https://gateway-api.sigs.k8s.io/) implementation using Varnish. Passes the Gateway API v1.5.0 conformance suite for the HTTPRoute profile, including core and some extended features.

## Container Images

Pre-built images are available on GitHub Container Registry:

```
ghcr.io/varnish/gateway-operator
ghcr.io/varnish/gateway-chaperone
```

Images are public and require no authentication to pull. Currently amd64 only.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        Kubernetes Cluster                       │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│   ┌─────────────┐         watches          ┌──────────────────┐ │
│   │  Operator   │ ◄────────────────────────│  Gateway API     │ │
│   │             │                          │  Resources       │ │
│   └──────┬──────┘                          │  - Gateway       │ │
│          │                                 │  - HTTPRoute     │ │
│          │ creates/updates                 │  - GatewayClass  │ │
│          ▼                                 └──────────────────┘ │
│   ┌─────────────────────────────────────────────────────────┐   │
│   │                    Varnish Pod                          │   │
│   │  ┌─────────────┐    ┌─────────────┐    ┌─────────────┐  │   │
│   │  │  Varnish    │    │  Chaperone  │    │  ConfigMaps │  │   │
│   │  │  + ghost    │◄───│             │◄───│  - main.vcl │  │   │
│   │  │             │    │             │    │             │  │   │
│   │  └──────┬──────┘    └──────┬──────┘    └─────────────┘  │   │
│   │         ▼                  │                            │   │
│   │     varnishlog-json        │ watches                    │   │
│   │                            ▼                            │   │
│   │                     EndpointSlices                      │   │
│   └─────────────────────────────────────────────────────────┘   │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

## Components

**Operator** - Cluster-wide deployment. Watches Gateway API resources and:
- Creates Varnish pods with ghost VMOD + chaperone
- Generates VCL preamble + user VCL (concatenated)
- Writes routing rules to `routing.json` (via ConfigMap)

**Chaperone** - Runs alongside each Varnish instance:
- Watches EndpointSlices, writes backend IPs to `ghost.json`
- Triggers ghost reload via HTTP (`/.varnish-ghost/reload`)
- Hot-reloads VCL via varnishadm when main.vcl changes

**Ghost VMOD** - Rust-based routing inside Varnish:
- Reads `ghost.json` at init and on reload
- Matches requests by hostname (exact + wildcard)
- Weighted backend selection
- Async HTTP client with connection pooling

## Config Reload

Two separate reload paths, both zero-downtime:
- **VCL changes** (user VCL updates): varnishadm hot-reload
- **Backend/routing changes**: ghost HTTP reload

## Caching

By default, Varnish Gateway operates as a pure reverse proxy with **no caching**. Every request passes through to the backend.

To enable caching, create a **VarnishCachePolicy** (VCP) targeting a Gateway, HTTPRoute, or individual route rule. VCP controls TTL, grace/keep, request coalescing, cache key customization, and bypass conditions.

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: cache-static
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: static-assets
  defaultTTL: 1h
  grace: 5m
```

See [VarnishCachePolicy Reference](docs/varnish-cache-policy.md) for full documentation.

## Real-Time Dashboard

Chaperone includes a real-time web dashboard that shows live gateway state via Server-Sent Events (SSE). It is always enabled on port 9000 inside the chaperone pod. Access it via port-forward:

```bash
kubectl port-forward -n namespace deploy/demo-gateway 9000:9000
```

Then open `http://localhost:9000`. The dashboard shows:
- Gateway status (ready/starting/draining) with a live heartbeat trace
- Virtual hosts and their routes
- Services with individual backend endpoints
- Live event stream (ghost reloads, VCL reloads, endpoint changes, TLS updates)

The dashboard updates every second and highlights changes as they happen. It is not exposed outside the cluster unless you explicitly create a Service for it.

## Configuration Files

**routing.json** (operator → ConfigMap):
```json
{
  "version": 2,
  "vhosts": {
    "api.example.com": {
      "routes": [
        {
          "path_match": {"type": "PathPrefix", "value": "/v2"},
          "service": "api-v2",
          "namespace": "default",
          "port": 8080,
          "weight": 100,
          "priority": 10300,
          "rule_index": 0
        }
      ]
    }
  }
}
```

**ghost.json** (chaperone → ghost VMOD):
```json
{
  "version": 2,
  "vhosts": {
    "api.example.com": {
      "routes": [
        {
          "path_match": {"type": "PathPrefix", "value": "/v2"},
          "backends": [
            {"address": "10.0.0.1", "port": 8080, "weight": 100},
            {"address": "10.0.0.2", "port": 8080, "weight": 100}
          ],
          "priority": 10300,
          "rule_index": 0
        }
      ]
    }
  }
}
```

## Known Limitations

- **BackendTLSPolicy** is currently non-functional. Varnish lacks per-backend CA certificate configuration, so backend TLS verification cannot be implemented correctly. The conformance tests for BackendTLSPolicy are skipped. This will be resolved when [varnish/varnish#26](https://github.com/varnish/varnish/issues/26) is fixed.

## Installation

**Helm (Recommended):**

```bash
helm install varnish-gateway \
  oci://ghcr.io/varnish/charts/varnish-gateway \
  --namespace varnish-gateway-system \
  --create-namespace
```

**kubectl (Alternative):**

```bash
# Install Gateway API CRDs
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.0/standard-install.yaml

# Deploy the operator
kubectl apply -f deploy/
```

See [INSTALL.md](INSTALL.md) for detailed installation instructions and configuration options.

## Loading Custom VMODs

The recommended way to load custom VMODs is to build a custom container image that includes them, then point your GatewayClass at it via `spec.image`:

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: custom-varnish
spec:
  image: my-registry/varnish-gateway-custom:v1.2.3
```

The custom image is typically a one-liner Dockerfile that adds your VMOD to the base image:

```dockerfile
FROM ghcr.io/varnish/varnish-gateway:latest
COPY libvmod_custom.so /usr/lib/varnish/vmods/
```

This is the most reliable approach: the VMOD is always present when varnishd starts, there are no race conditions, and it works identically in every environment. The logging sidecar (if configured) also inherits the custom image automatically, unless `logging.image` is set explicitly.

Changing `spec.image` triggers a rolling restart of all gateway pods using that GatewayClass.

### Alternative: Init containers

For cases where building a custom image is not practical, you can use `extraInitContainers` to copy VMOD files into a shared volume at pod startup:

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-params
spec:
  varnishdExtraArgs:
    - "-p"
    - "vmod_path=/usr/lib/varnish/vmods:/extra-vmods"
  extraVolumes:
    - name: extra-vmods
      emptyDir: {}
  extraVolumeMounts:
    - name: extra-vmods
      mountPath: /extra-vmods
  extraInitContainers:
    - name: install-vmods
      image: my-registry/my-vmods:latest
      command: ["cp", "/vmods/libvmod_custom.so", "/dst/"]
      volumeMounts:
        - name: extra-vmods
          mountPath: /dst
```

The init container copies the `.so` files into a shared `emptyDir` volume, the main container mounts that volume, and `varnishdExtraArgs` extends the VMOD search path so Varnish finds them. This approach is more fragile than a custom image since it depends on init container ordering during pod startup.

See [CLAUDE.md](CLAUDE.md) for development setup and detailed documentation.
