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

# Build and push images (cross-compile for x86 cluster from ARM Mac)
docker build --platform linux/amd64 -t registry.digitalocean.com/varnish-gateway/operator:latest -f Dockerfile.operator .
docker build --platform linux/amd64 -t registry.digitalocean.com/varnish-gateway/sidecar:latest -f Dockerfile.sidecar .
docker push registry.digitalocean.com/varnish-gateway/operator:latest
docker push registry.digitalocean.com/varnish-gateway/sidecar:latest
```

## Install Gateway API CRDs

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml
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

## Clean Up Everything

Remove all resources including Gateway API CRDs:

```bash
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

This mimics real production churn and helps test that the sidecar correctly tracks endpoint changes.
