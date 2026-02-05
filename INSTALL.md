# Installation Guide

This guide covers different installation methods for Varnish Gateway.

## Prerequisites

- Kubernetes 1.26+
- kubectl configured to access your cluster
- Helm 3.8+ (for Helm installation method)

## Installation Methods

### Method 1: Helm (Recommended)

The easiest way to install Varnish Gateway is using the Helm chart:

```bash
# Install the chart
helm install varnish-gateway ./charts/varnish-gateway \
  --namespace varnish-gateway-system \
  --create-namespace
```

This will:
- Install Gateway API CRDs (GatewayClass, Gateway, HTTPRoute)
- Install Varnish-specific CRD (GatewayClassParameters)
- Deploy the operator
- Create RBAC resources
- Create a default GatewayClass named "varnish"

#### Helm Installation Options

**Install without Gateway API CRDs** (if already installed cluster-wide):

```bash
helm install varnish-gateway ./charts/varnish-gateway \
  --set installGatewayAPICRDs=false
```

**Install with custom values**:

```bash
helm install varnish-gateway ./charts/varnish-gateway \
  -f charts/varnish-gateway/examples/custom-values.yaml
```

**Install specific version**:

```bash
helm install varnish-gateway ./charts/varnish-gateway \
  --set operator.image.tag=v0.7.2 \
  --set chaperone.image.tag=v0.7.2
```

See [charts/varnish-gateway/README.md](charts/varnish-gateway/README.md) for all configuration options.

### Method 2: kubectl with manifests

If you prefer not to use Helm, you can install using kubectl:

```bash
# Install Gateway API CRDs
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml

# Install Varnish Gateway
kubectl apply -f deploy/
```

This will install the same resources as the Helm chart, but without templating support.

## Verify Installation

Check that the operator is running:

```bash
kubectl get pods -n varnish-gateway-system
```

You should see:

```
NAME                                        READY   STATUS    RESTARTS   AGE
varnish-gateway-operator-xxxxxxxxxx-xxxxx   1/1     Running   0          30s
```

Check that the GatewayClass was created:

```bash
kubectl get gatewayclass
```

You should see:

```
NAME      CONTROLLER                      ACCEPTED   AGE
varnish   varnish-software.com/gateway    True       1m
```

## Create Your First Gateway

Create a Gateway resource:

```bash
kubectl apply -f - <<EOF
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
EOF
```

Wait for the Gateway to be ready:

```bash
kubectl wait --for=condition=Programmed gateway/my-gateway -n default --timeout=60s
```

Check the Gateway status:

```bash
kubectl get gateway my-gateway -n default
```

You should see:

```
NAME         CLASS     ADDRESS         PROGRAMMED   AGE
my-gateway   varnish   10.96.xxx.xxx   True         1m
```

## Create an HTTPRoute

Route traffic to a backend service:

```bash
kubectl apply -f - <<EOF
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
    - backendRefs:
        - name: my-service
          port: 8080
EOF
```

Check the HTTPRoute status:

```bash
kubectl get httproute my-route -n default
```

## Troubleshooting

### View operator logs

```bash
kubectl logs -n varnish-gateway-system -l app.kubernetes.io/component=operator -f
```

### View Gateway pod logs

```bash
# Find the Gateway pod
kubectl get pods -n default -l gateway.networking.k8s.io/gateway-name=my-gateway

# View logs
kubectl logs -n default <gateway-pod-name> -f
```

### Check Gateway status conditions

```bash
kubectl describe gateway my-gateway -n default
```

### Check HTTPRoute status conditions

```bash
kubectl describe httproute my-route -n default
```

## Upgrading

### Helm

```bash
# Upgrade CRDs first (Helm doesn't auto-upgrade CRDs)
kubectl apply -f charts/varnish-gateway/crds/

# Upgrade the chart
helm upgrade varnish-gateway ./charts/varnish-gateway \
  --namespace varnish-gateway-system
```

### kubectl

```bash
# Re-apply manifests
kubectl apply -f deploy/
```

## Uninstallation

### Helm

```bash
# Uninstall the release
helm uninstall varnish-gateway --namespace varnish-gateway-system

# Optionally delete CRDs (WARNING: This will delete all Gateway/HTTPRoute resources)
kubectl delete -f charts/varnish-gateway/crds/
```

### kubectl

```bash
# Delete manifests
kubectl delete -f deploy/

# Optionally delete Gateway API CRDs
kubectl delete -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml
```

## Next Steps

- See [examples/](charts/varnish-gateway/examples/) for more Gateway and HTTPRoute examples
- Read [docs/configuration-reference.md](docs/configuration-reference.md) for advanced configuration
- Check out [CLAUDE.md](CLAUDE.md) for development information
