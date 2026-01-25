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

## Deploy Test Applications

Two stable mock HTTP services for testing:

- **app-alpha** and **app-beta**: JSON API servers on port 8080
- Return hostname and request info (useful for verifying routing)
- Stable by default - pods run indefinitely without crashing
- Start with 2 replicas each

```bash
kubectl apply -f hack/test-env/deployments.yaml
```

Test that they're working:

```bash
kubectl port-forward svc/app-alpha 8080:8080 &
curl localhost:8080
# {"app": "alpha", "hostname": "app-alpha-xxx", "path": "/"}
```

## Watch Endpoint Changes

See EndpointSlices update in real-time:

```bash
./hack/test-env/watch-endpoints.sh
```

Or manually:

```bash
kubectl get endpointslices -l 'kubernetes.io/service-name in (app-alpha, app-beta)' --watch
```

## Inject Chaos

Use the interactive chaos tool to induce failures:

```bash
./hack/test-env/chaos.sh
```

This opens an interactive menu:

```
=== Chaos Menu ===
  1) Kill a random pod
  2) Kill a specific pod
  3) Scale a deployment
  4) Start gentle continuous chaos
  5) Start rolling chaos (scale up/down)
  6) Reset to stable state (2 replicas each)
  s) Show current status
  q) Quit
```

### Non-interactive mode

You can also run chaos commands directly:

```bash
# Kill a random pod
./hack/test-env/chaos.sh --kill

# Kill a random pod from a specific app
./hack/test-env/chaos.sh --kill app-alpha

# Scale a deployment
./hack/test-env/chaos.sh --scale app-beta 3

# Reset to stable state
./hack/test-env/chaos.sh --reset

# Show current status
./hack/test-env/chaos.sh --status

# Start continuous chaos (kills one pod every 30-60s)
./hack/test-env/chaos.sh --continuous
```

## Manual Operations

### Scale deployments

```bash
kubectl scale deployment app-alpha --replicas=3
kubectl scale deployment app-beta --replicas=1
```

### Kill pods

```bash
kubectl delete pod -l app=app-alpha
```

## Test the Gateway

Once deployed, test routing through the gateway:

```bash
# Get the gateway service IP
kubectl get svc varnish-gateway

# Port-forward to test locally
kubectl port-forward svc/varnish-gateway 8080:80 &

# Test routing (uses Host header to route)
curl -H "Host: alpha.example.com" localhost:8080
# {"app": "alpha", ...}

curl -H "Host: beta.example.com" localhost:8080
# {"app": "beta", ...}
```

## Debug Gateway Backend Stats

The chaperone exposes a `/debug/backends` endpoint that shows ghost VMOD backend statistics. Access it using kubectl port-forward:

```bash
# Find the gateway pod
kubectl get pods -l app.kubernetes.io/name=gateway

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

## Watch Kubernetes Events

```bash
kubectl get events --watch
```

## Useful Watches

```bash
# All pods
kubectl get pods --watch

# EndpointSlices with IPs
kubectl get endpointslices -o wide --watch

# Specific service
kubectl get endpointslices -l kubernetes.io/service-name=app-alpha --watch -o yaml
```

## Clean Up

Remove test deployments:

```bash
kubectl delete -f hack/test-env/deployments.yaml
```

Remove gateway operator:

```bash
kubectl delete -f deploy/
```

## Clean Up Everything

Remove all resources including Gateway API CRDs:

```bash
# Delete gateway operator and resources
kubectl delete -f deploy/

# Delete test deployments
kubectl delete -f hack/test-env/deployments.yaml

# Delete Gateway API CRDs
kubectl delete -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml
```

## What You'll See

With the test environment running and chaos.sh active, you'll observe:

1. **Pod termination** - chaos.sh kills pods, k8s restarts them
2. **Endpoint removal** - Killed pods are removed from EndpointSlices
3. **Endpoint addition** - New pods added once readiness probe passes
4. **Scaling changes** - If using rolling chaos, replica counts fluctuate

This mimics real production churn and helps test that the chaperone correctly tracks endpoint changes.
