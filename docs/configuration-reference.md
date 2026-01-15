# Configuration Reference

## GatewayClassParameters

Cluster-scoped CRD for configuring Varnish Gateway behavior. Referenced by `GatewayClass.spec.parametersRef`.

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: my-params
spec:
  userVCLConfigMapRef:    # optional
    name: my-vcl
    namespace: default
    key: user.vcl         # optional, defaults to "user.vcl"
  varnishdExtraArgs:      # optional
    - "-p"
    - "thread_pools=2"
```

### spec.userVCLConfigMapRef

Reference to a ConfigMap containing custom VCL. The VCL is appended to the generated VCL using Varnish's subroutine concatenation.

| Field | Required | Description |
|-------|----------|-------------|
| `name` | yes | ConfigMap name |
| `namespace` | yes | ConfigMap namespace |
| `key` | no | Key containing VCL (default: `user.vcl`) |

### spec.varnishdExtraArgs

Additional command-line arguments passed to varnishd. Each array element is a separate argument.

**Protected flags** (cannot be overridden):
- `-M` - Admin socket (controlled by operator)
- `-S` - Secret file (controlled by operator)
- `-F` - Foreground mode (required for containers)
- `-f` - VCL file (loaded via admin socket)
- `-n` - Working directory (controlled by operator)

**Common extra args:**

| Args | Description |
|------|-------------|
| `-p thread_pools=N` | Number of thread pools (default: 2) |
| `-p thread_pool_min=N` | Min threads per pool |
| `-p thread_pool_max=N` | Max threads per pool |
| `-p workspace_client=N` | Client workspace size (e.g., `256k`) |
| `-s malloc,SIZE` | Additional malloc storage (e.g., `512m`, `2g`) |
| `-s file,PATH,SIZE` | File-based storage |

**Varnish Enterprise:**

| Args | Description |
|------|-------------|
| `-s mse,PATH` | Massive Storage Engine config file |
| `-p feature=+http2` | Enable HTTP/2 |

## Environment Variables

Chaperone reads these environment variables. The operator sets most of them automatically.

### Varnishd Process

| Variable | Default | Description |
|----------|---------|-------------|
| `VARNISH_LISTEN` | `:80,http` | Listen address(es) for varnishd (`-a` flag). Semicolon-separated for multiple. |
| `VARNISH_STORAGE` | `malloc,256m` | Storage backend(s) (`-s` flag). Semicolon-separated for multiple. |
| `VARNISH_ADMIN_PORT` | `6082` | Admin socket port (`-M` flag) |
| `VARNISH_DIR` | *(empty)* | Varnish working directory (`-n` flag). Empty uses varnish default. |
| `VARNISHD_EXTRA_ARGS` | *(none)* | Additional varnishd args. Semicolon-separated. Set via GatewayClassParameters. |

### Paths

| Variable | Default | Description |
|----------|---------|-------------|
| `WORK_DIR` | `/var/run/varnish` | Chaperone working directory (secrets, runtime files) |
| `VCL_PATH` | `/var/run/varnish/main.vcl` | Path to VCL file (watched for changes) |
| `ROUTING_CONFIG_PATH` | `/etc/varnish/routing.json` | Routing config from operator |
| `GHOST_CONFIG_PATH` | `/var/run/varnish/ghost.json` | Generated ghost config output |

### Runtime

| Variable | Default | Description |
|----------|---------|-------------|
| `NAMESPACE` | `default` | Kubernetes namespace for EndpointSlice watching |
| `HEALTH_ADDR` | `:8080` | Health/readiness endpoint address |
| `VARNISH_HTTP_ADDR` | `localhost:80` | Varnish HTTP address for ghost reload requests |

### Semicolon-separated values

Variables that accept multiple values use semicolons as separators (not commas, since commas appear in varnish arg values like `:80,http`).

```
VARNISH_LISTEN=":80,http;:443,https"
VARNISH_STORAGE="malloc,256m;file,/var/cache/varnish,10g"
VARNISHD_EXTRA_ARGS="-p;thread_pools=4;-p;workspace_client=256k"
```

## Examples

### Basic with extra memory

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-params
spec:
  varnishdExtraArgs:
    - "-s"
    - "malloc,1g"
    - "-p"
    - "thread_pools=4"
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: varnish
spec:
  controllerName: varnish-software.com/gateway
  parametersRef:
    group: gateway.varnish-software.com
    kind: GatewayClassParameters
    name: varnish-params
```

### With custom VCL

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: my-vcl
  namespace: default
data:
  user.vcl: |
    sub vcl_recv {
      if (req.url ~ "^/api/") {
        set req.http.X-API-Request = "true";
      }
    }
---
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-params
spec:
  userVCLConfigMapRef:
    name: my-vcl
    namespace: default
  varnishdExtraArgs:
    - "-p"
    - "thread_pools=2"
```

### Varnish Enterprise with MSE

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-enterprise
spec:
  varnishdExtraArgs:
    - "-s"
    - "mse,/etc/varnish/mse.conf"
    - "-p"
    - "feature=+http2"
```
