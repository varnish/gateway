# Varnish Gateway Operator

Kubernetes Gateway API implementation using Varnish. The system consists of two binaries: the **operator** runs cluster-wide, watches Gateway API resources (Gateway, HTTPRoute), generates VCL routing logic, and manages Varnish deployments. The **sidecar** runs alongside each Varnish instance handling runtime concerns: endpoint discovery via Kubernetes EndpointSlices and VCL hot-reloading via varnishadm. This split exists because the operator works at the configuration level (what should exist) while the sidecar works at the runtime level (what's happening now). Backend IPs change frequently as pods scale; this is handled by the sidecar without requiring VCL recompilation.

## Sidecar

The sidecar watches `services.json` (written by the operator) and Kubernetes EndpointSlices, generating `backends.conf` for the Varnish nodes vmod. It also watches `main.vcl` and hot-reloads VCL into Varnish when it changes.

### Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `NAMESPACE` | `default` | Kubernetes namespace to watch |
| `VARNISH_ADMIN_ADDR` | `localhost:6082` | Address for varnishadm listener |
| `VARNISH_SECRET_PATH` | `/etc/varnish/secret` | Path to Varnish admin secret |
| `SERVICES_FILE_PATH` | `/var/run/varnish/services.json` | Input: services to watch |
| `BACKENDS_FILE_PATH` | `/var/run/varnish/backends.conf` | Output: generated backends |
| `VCL_PATH` | `/var/run/varnish/main.vcl` | VCL file to watch for reloads |
| `HEALTH_ADDR` | `:8080` | Health endpoint address |

### Running Locally (Demo Mode)

Requires a local Kubernetes cluster (Rancher Desktop, minikube, etc.) with some services running.

```bash
# Build
go build ./cmd/sidecar

# Create test fixtures
mkdir -p /tmp/varnish-sidecar-test
echo "testsecret" > /tmp/varnish-sidecar-test/secret
touch /tmp/varnish-sidecar-test/main.vcl

# Create services.json with services to watch (must exist in your cluster)
cat > /tmp/varnish-sidecar-test/services.json <<EOF
{
  "services": [
    {"name": "app-alpha", "port": 8080},
    {"name": "app-beta", "port": 8080}
  ]
}
EOF

# Run (uses ~/.kube/config when outside cluster)
NAMESPACE=default \
VARNISH_SECRET_PATH=/tmp/varnish-sidecar-test/secret \
SERVICES_FILE_PATH=/tmp/varnish-sidecar-test/services.json \
BACKENDS_FILE_PATH=/tmp/varnish-sidecar-test/backends.conf \
VCL_PATH=/tmp/varnish-sidecar-test/main.vcl \
./sidecar

# Check the generated backends.conf
cat /tmp/varnish-sidecar-test/backends.conf
```

### Generated Files

**backends.conf** (INI format for nodes vmod):
```ini
[app-alpha]
pod_10_42_0_75 = 10.42.0.75:8080
pod_10_42_0_76 = 10.42.0.76:8080

[app-beta]
pod_10_42_0_77 = 10.42.0.77:8080
```

**services.json** (input from operator):
```json
{
  "services": [
    {"name": "app-alpha", "port": 8080},
    {"name": "app-beta", "port": 8080}
  ]
}
```
