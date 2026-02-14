# Kubernetes Development Environment

How to set up a local Kubernetes environment for testing the gateway operator.

## Prerequisites

- Rancher Desktop (or any local k8s), OR DigitalOcean Kubernetes
- kubectl configured
- doctl configured (for DO)

## DigitalOcean Setup

Cluster: `varnish-gateway-dev`
Registry: `varnish-gateway`

```bash
# Connect registry to cluster (only needed once)
doctl kubernetes cluster registry add varnish-gateway-dev

# Authenticate docker to the registry
doctl registry login

# Build and push using Makefile
make docker-push

# Or build multi-arch (amd64 + arm64)
make docker-buildx
```

Images:
- `registry.digitalocean.com/varnish-gateway/gateway-operator`
- `registry.digitalocean.com/varnish-gateway/gateway-chaperone`

## Install Gateway API CRDs

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml
```

## Deploy the Operator

### Set up Image Pull Secrets

The DigitalOcean registry requires authentication. Create secrets in the namespaces where pods will run:

```bash
# Create secret in operator namespace
doctl registry kubernetes-manifest --name regcred | \
  sed 's/namespace: kube-system/namespace: varnish-gateway-system/' | \
  kubectl apply -f -

# Create secret in default namespace (for chaperone pods)
doctl registry kubernetes-manifest --name regcred | \
  sed 's/namespace: kube-system/namespace: default/' | \
  kubectl apply -f -
```

### Deploy

```bash
kubectl apply -f deploy/
```

This creates:
- `varnish-gateway-system` namespace with the operator
- `varnish` GatewayClass
- RBAC for operator and chaperone pods
- Sample Gateway and HTTPRoutes (in default namespace)

### Verify

```bash
# Check operator
kubectl get pods -n varnish-gateway-system

# Check gateway status
kubectl get gateway,httproute

# Check chaperone pod (created by operator)
kubectl get pods -l app.kubernetes.io/managed-by=varnish-gateway-operator
```

## Running Conformance Tests

The Gateway API conformance suite is the primary test harness:

```bash
# Run full suite (~3.5 minutes)
make test-conformance

# Run a single test (~1 minute)
make test-conformance-single TEST=HTTPRouteMethodMatching

# Run with report output
make test-conformance-report
```

The conformance tests create their own namespaces, Gateways, HTTPRoutes, and backend pods. No manual test app setup needed.

## Manual Testing

For ad-hoc testing, deploy your own services and route to them:

```bash
# Get the gateway service IP
kubectl get svc -l gateway.networking.k8s.io/gateway-name

# Port-forward to test locally
kubectl port-forward svc/<gateway-svc> 8080:80 &

# Test routing (uses Host header to route)
curl -H "Host: example.com" localhost:8080
```

## Debug Gateway Backend Stats

The chaperone exposes a `/debug/backends` endpoint that shows ghost VMOD backend statistics. Access it using kubectl port-forward:

```bash
# Find the gateway pod
kubectl get pods -l app.kubernetes.io/managed-by=varnish-gateway-operator

# Port-forward to the health server (default port 8080)
kubectl port-forward <gateway-pod-name> 8080:8080

# In another terminal, access the debug endpoint

# Normal output (table format)
curl http://localhost:8080/debug/backends

# Detailed output (multi-line per vhost with backend breakdown)
curl http://localhost:8080/debug/backends?detailed=true

# JSON output (for scripting/monitoring)
curl http://localhost:8080/debug/backends?format=json | jq .
```

Example output (detailed mode):
```
Backend: ghost.alpha.example.com
  Admin: auto
  Health: healthy
  Routes: 1
  Total requests: 1234
  Last request: 2026-01-25T15:30:45Z
  Backends:
    10.244.0.5:8080 - 890 selections (72.0%)
    10.244.0.6:8080 - 344 selections (28.0%)
```

This is useful for:
- Verifying routing decisions (which backend was selected)
- Checking traffic distribution across pod replicas
- Debugging why certain endpoints aren't receiving traffic
- Monitoring request counts per vhost

**Note**: Stats are reset when the configuration is reloaded (new EndpointSlices or routing changes).

## Useful Watches

```bash
# All pods
kubectl get pods --watch

# Kubernetes events
kubectl get events --watch

# EndpointSlices with IPs
kubectl get endpointslices -o wide --watch

# Specific service
kubectl get endpointslices -l kubernetes.io/service-name=<svc> --watch -o yaml
```

## Clean Up

Remove gateway operator:

```bash
kubectl delete -f deploy/
```

Remove everything including Gateway API CRDs:

```bash
kubectl delete -f deploy/
kubectl delete -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml
```
