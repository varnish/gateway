# Configuration Reference

## GatewayClassParameters

Cluster-scoped CRD for configuring Varnish Gateway behavior. Referenced by `GatewayClass.spec.parametersRef`.

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: my-params
spec:
  image: my-registry/varnish-gateway-custom:v1 # optional, overrides GATEWAY_IMAGE
  userVCLConfigMapRef: # optional
    name: my-vcl
    namespace: default
    key: user.vcl # optional, defaults to "user.vcl"
  varnishdExtraArgs: # optional
    - "-p"
    - "thread_pools=2"
```

### spec.image

Custom container image for varnish-gateway pods. Overrides the operator's `GATEWAY_IMAGE` environment variable for all Gateways using this GatewayClass. Useful for images with additional VMODs baked in. If not set, the operator default is used.

The logging sidecar also inherits this image unless `logging.image` is set explicitly.

Changing this field triggers a rolling restart of all affected gateway pods.

### spec.userVCLConfigMapRef

Reference to a ConfigMap containing custom VCL. The VCL is appended to the generated VCL using Varnish's subroutine concatenation.

| Field       | Required | Description                              |
| ----------- | -------- | ---------------------------------------- |
| `name`      | yes      | ConfigMap name                           |
| `namespace` | yes      | ConfigMap namespace                      |
| `key`       | no       | Key containing VCL (default: `user.vcl`) |

### spec.varnishdExtraArgs

Additional command-line arguments passed to varnishd. Each array element is a separate argument.

**Protected flags** (cannot be overridden):

- `-M` - Admin socket (controlled by operator)
- `-S` - Secret file (controlled by operator)
- `-F` - Foreground mode (required for containers)
- `-f` - VCL file (loaded via admin socket)
- `-n` - Working directory (controlled by operator)

**Common extra args:**

| Args                        | Description                                                         |
| --------------------------- | ------------------------------------------------------------------- |
| `-p thread_pool_stack=160k` | Worker thread stack size (recommended for ghost VMOD, default: 80k) |
| `-p thread_pools=N`         | Number of thread pools (default: 2)                                 |
| `-p thread_pool_min=N`      | Min threads per pool                                                |
| `-p thread_pool_max=N`      | Max threads per pool                                                |
| `-p workspace_client=N`     | Client workspace size (e.g., `256k`)                                |
| `-s malloc,SIZE`            | Additional malloc storage (e.g., `512m`, `2g`)                      |
| `-s file,PATH,SIZE`         | File-based storage                                                  |

**Note:** The ghost VMOD uses Rust regex which benefits from increased stack size (160k vs default 80k), especially in debug builds. This is safe and adds minimal memory overhead (~16MB for typical thread pool configurations).

### spec.service

Configures the data-plane Service. When omitted, the operator preserves the historical default: `Type: LoadBalancer` with no extra annotations or labels. Service-shape changes do **not** trigger pod restarts.

| Field                      | Type                  | Description                                                                                                                                                                                          |
| -------------------------- | --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `type`                     | `string`              | One of `ClusterIP`, `NodePort`, `LoadBalancer`. Defaults to `LoadBalancer`. Headless services (`clusterIP: None`) and `ExternalName` are intentionally unsupported.                                  |
| `annotations`              | `map[string]string`   | Annotations applied to the Service. Tracked via the `varnish.io/managed-annotations` sentinel; cloud-controller-added annotations are preserved on update.                                           |
| `labels`                   | `map[string]string`   | Labels applied to the Service. Controller-managed labels (`app.kubernetes.io/managed-by`, `gateway.networking.k8s.io/gateway-name`, `gateway.networking.k8s.io/gateway-namespace`) cannot be overridden. |
| `loadBalancerClass`        | `string`              | Selects a specific LB implementation. Only valid when `type: LoadBalancer`.                                                                                                                          |
| `loadBalancerSourceRanges` | `[]string`            | CIDR allowlist for the LoadBalancer. Only valid when `type: LoadBalancer`. CIDR syntax is validated by Kubernetes when the Service is reconciled.                                                    |
| `externalTrafficPolicy`    | `Cluster` \| `Local`  | Source-IP preservation. Only valid when `type` is `LoadBalancer` or `NodePort`.                                                                                                                      |

**Layering with `Gateway.spec.infrastructure`:** the Gateway API v1.1+ `Gateway.spec.infrastructure.{labels,annotations}` field overlays on top of the GatewayClass-level config — per-Gateway keys win on collisions. The other four fields (Type, LoadBalancerClass, LoadBalancerSourceRanges, ExternalTrafficPolicy) are GatewayClass-level only.

**Admission-time validation:**

- `loadBalancerClass` requires `type: LoadBalancer`
- `loadBalancerSourceRanges` requires `type: LoadBalancer`
- `externalTrafficPolicy` requires `type` to be `LoadBalancer` or `NodePort`

**First-time opt-in behavior:** Services that existed before this feature was introduced get the operator's managed-keys sentinel established on the first reconcile that materializes a non-empty `annotations` or `labels` in `spec.service` (or in `Gateway.spec.infrastructure`). Pre-existing annotations are treated as not-managed-by-the-operator — they will not be pruned even if a key with the same name later appears in spec and is then removed. To bring an existing annotation under operator management, delete it from the Service manually and re-add it via `spec.service.annotations`.

#### Example: ClusterIP behind another L7 (gateway-behind-gateway)

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: internal-cache
spec:
  service:
    type: ClusterIP
```

#### Example: MetalLB annotation-driven LoadBalancer on bare metal

```yaml
spec:
  service:
    type: LoadBalancer
    annotations:
      metallb.universe.tf/loadBalancerIPs: "10.0.0.42"
      metallb.universe.tf/address-pool: production
```

#### Example: Preserve source IP via Local traffic policy

```yaml
spec:
  service:
    type: LoadBalancer
    externalTrafficPolicy: Local
    loadBalancerSourceRanges:
      - 198.51.100.0/24
```

## Environment Variables

Chaperone reads these environment variables. The operator sets most of them automatically when it builds the gateway pod, so the "Chaperone default" column documents what chaperone falls back to when run standalone — in operator-managed pods the operator-set value is what you'll see in practice.

### Varnishd Process

| Variable              | Chaperone default | Description                                                                                                                                                                                       |
| --------------------- | ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `VARNISH_LISTEN`      | `http=:80`        | Listen address(es) for varnishd (`-a` flag). Semicolon-separated for multiple. The operator builds this from Gateway listeners in `{proto}-{port}=:{port},{proto}` form, so the default never applies to operator-managed pods. |
| `VARNISH_STORAGE`     | `malloc,256m`     | Storage backend(s) (`-s` flag). Semicolon-separated for multiple.                                                                                                                                 |
| `VARNISH_ADMIN_PORT`  | `6082`            | Admin socket port (`-M` flag)                                                                                                                                                                     |
| `VARNISH_DIR`         | _(empty)_         | Varnish working directory (`-n` flag). Empty uses varnish default. The operator sets `/var/run/varnish/vsm`.                                                                                      |
| `VARNISHD_EXTRA_ARGS` | _(none)_          | Additional varnishd args. Semicolon-separated. Set via GatewayClassParameters.                                                                                                                    |

### Paths

| Variable            | Chaperone default             | Description                                                                                                                                          |
| ------------------- | ----------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `WORK_DIR`          | `/var/run/varnish`            | Chaperone working directory (secrets, runtime files)                                                                                                 |
| `VCL_PATH`          | `/var/run/varnish/main.vcl`   | Path to VCL file (watched for changes). The operator sets `/etc/varnish/main.vcl`, mounted from the gateway's ConfigMap.                             |
| `CONFIGMAP_NAME`    | `gateway-vcl`                 | Name of the ConfigMap that holds `routing.json` and `main.vcl`. Chaperone watches this directly via the Kubernetes informer rather than a file path. |
| `GHOST_CONFIG_PATH` | `/var/run/varnish/ghost.json` | Generated ghost config output                                                                                                                        |

### Runtime

| Variable            | Chaperone default | Description                                                                                                                                |
| ------------------- | ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `NAMESPACE`         | `default`         | Kubernetes namespace for EndpointSlice watching                                                                                            |
| `HEALTH_ADDR`       | `:8080`           | Health/readiness endpoint address. The operator sets `:8081` and points the readiness probe at it, so operator-managed pods listen on 8081. |
| `VARNISH_HTTP_ADDR` | `localhost:1969`  | Varnish HTTP address for ghost reload requests (dedicated loopback listener)                                                               |
| `LOG_LEVEL`         | `info`            | Chaperone log level. Accepted values: `debug`, `info`, `warn`, `error` (case-insensitive). Unknown values fall back to `info` with a warning. When `LOG_LEVEL` is set on the operator, it is forwarded into every chaperone pod the operator reconciles, so the data plane inherits the same verbosity. |

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
    - "-p"
    - "thread_pool_stack=160k"
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
    - "thread_pool_stack=160k"
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
