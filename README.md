# Varnish Gateway Operator

Kubernetes Gateway API implementation using Varnish.

## Container Images

Pre-built images are available on GitHub Container Registry:

```
ghcr.io/varnish/gateway-operator
ghcr.io/varnish/gateway-chaperone
```

Images are public and require no authentication to pull.

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

## Config reload

All changes in HTTPRoutes will be 

Two separate reload paths:
- **VCL changes** (user VCL updates): varnishadm hot-reload
- **Backend/routing changes**: ghost HTTP reload

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
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml

# Deploy the operator
kubectl apply -f deploy/
```

See [INSTALL.md](INSTALL.md) for detailed installation instructions and configuration options.

See [CLAUDE.md](CLAUDE.md) for development setup and detailed documentation.
