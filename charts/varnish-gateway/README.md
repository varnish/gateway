# Varnish Gateway Helm Chart

This Helm chart installs the Varnish Gateway operator, which implements the Kubernetes Gateway API using Varnish.

## Prerequisites

- Kubernetes 1.26+
- Helm 3.8+

## Installation

```bash
helm install varnish-gateway oci://ghcr.io/varnish/charts/varnish-gateway \
  --version 0.x.y \
  --namespace varnish-gateway-system \
  --create-namespace
```

## Configuration

The following table lists the configurable parameters of the Varnish Gateway chart and their default values.

### Operator Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `operator.replicas` | Number of operator replicas | `1` |
| `operator.image.repository` | Operator image repository | `ghcr.io/varnish/gateway-operator` |
| `operator.image.tag` | Operator image tag (defaults to chart appVersion) | `""` |
| `operator.image.pullPolicy` | Operator image pull policy | `IfNotPresent` |
| `operator.resources.limits.cpu` | CPU limit | `500m` |
| `operator.resources.limits.memory` | Memory limit | `128Mi` |
| `operator.resources.requests.cpu` | CPU request | `10m` |
| `operator.resources.requests.memory` | Memory request | `64Mi` |
| `operator.leaderElection.enabled` | Enable leader election | `true` |

### Gateway Class Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `gatewayClass.name` | Name of the GatewayClass | `varnish` |
| `gatewayClass.controllerName` | Controller identifier | `varnish-software.com/gateway` |
| `gatewayClass.createDefaultParams` | Create default GatewayClassParameters | `true` |
| `gatewayClass.defaultParams.userVCL.enabled` | Enable user VCL configuration | `true` |
| `gatewayClass.defaultParams.userVCL.content` | Default VCL for initial install (not overwritten on upgrade) | See values.yaml |
| `gatewayClass.defaultParams.logging.enabled` | Enable logging | `true` |
| `gatewayClass.defaultParams.logging.mode` | Logging mode (varnishlog or varnishncsa) | `varnishlog` |
| `gatewayClass.defaultParams.varnishdExtraArgs` | Extra varnishd arguments | See values.yaml |

### Chaperone Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `chaperone.image.repository` | Chaperone image repository | `ghcr.io/varnish/gateway-chaperone` |
| `chaperone.image.tag` | Chaperone image tag (defaults to chart appVersion) | `""` |
| `chaperone.image.pullPolicy` | Chaperone image pull policy | `IfNotPresent` |
| `chaperone.imagePullSecrets` | Image pull secrets for chaperone pods | `""` |

### Other Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `namespace` | Namespace for operator deployment | `varnish-gateway-system` |
| `rbac.create` | Create RBAC resources | `true` |
| `serviceAccount.create` | Create service account | `true` |
| `installGatewayAPICRDs` | Install Gateway API CRDs | `true` |

## Examples

### Install with custom operator image

```bash
helm install varnish-gateway oci://ghcr.io/varnish/charts/varnish-gateway \
  --set operator.image.tag=v0.x.y \
  --set chaperone.image.tag=v0.x.y
```

### Install without Gateway API CRDs

If you've already installed Gateway API CRDs cluster-wide:

```bash
helm install varnish-gateway oci://ghcr.io/varnish/charts/varnish-gateway \
  --set installGatewayAPICRDs=false
```

### Customize VCL configuration

```bash
helm install varnish-gateway oci://ghcr.io/varnish/charts/varnish-gateway \
  --set-file gatewayClass.defaultParams.userVCL.content=./my-custom.vcl
```

### Disable default GatewayClassParameters

If you want to create your own GatewayClassParameters:

```bash
helm install varnish-gateway oci://ghcr.io/varnish/charts/varnish-gateway \
  --set gatewayClass.createDefaultParams=false
```

## Upgrading

### Upgrade the chart

```bash
helm upgrade varnish-gateway oci://ghcr.io/varnish/charts/varnish-gateway \
  --namespace varnish-gateway-system
```

### CRD Upgrades

Helm does not automatically upgrade CRDs. To upgrade CRDs:

```bash
kubectl apply -f charts/varnish-gateway/crds/
```

## Uninstallation

```bash
helm uninstall varnish-gateway --namespace varnish-gateway-system
```

**Note**: This will not remove CRDs. To remove CRDs:

```bash
kubectl delete -f charts/varnish-gateway/crds/
```

## Usage

After installing the chart, you can create Gateway and HTTPRoute resources:

```yaml
# Create a Gateway
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: my-gateway
  namespace: default
spec:
  gatewayClassName: varnish
  listeners:
    - name: http
      protocol: HTTP
      port: 80
---
# Create an HTTPRoute
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: my-route
  namespace: default
spec:
  parentRefs:
    - name: my-gateway
  hostnames:
    - "example.com"
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /api
      backendRefs:
        - name: api-service
          port: 8080
```

## Development

To test the chart locally:

```bash
# Lint the chart
helm lint charts/varnish-gateway

# Template the chart to see rendered output
helm template varnish-gateway charts/varnish-gateway

# Install with dry-run
helm install varnish-gateway charts/varnish-gateway --dry-run --debug
```

## License

See the main repository LICENSE file.
