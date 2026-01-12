# Kubernetes Development Environment

How to set up a noisy Kubernetes environment for testing the gateway operator.

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

Two unstable applications that simulate real-world pod churn:

- **app-alpha** and **app-beta**: Simple HTTP servers on port 8080
- Pods crash randomly after 30-120 seconds
- Readiness probes mark pods ready/unready
- Start with 2 replicas each

```bash
kubectl apply -f hack/test-env/deployments.yaml
```

## Watch Endpoint Changes

See EndpointSlices update in real-time as pods come and go:

```bash
./hack/test-env/watch-endpoints.sh
```

Or manually:

```bash
kubectl get endpointslices -l 'kubernetes.io/service-name in (app-alpha, app-beta)' --watch
```

## Add Extra Chaos (Optional)

Randomly scale deployments between 1-3 replicas every 15-45 seconds:

```bash
./hack/test-env/chaos.sh
```

## Manual Scaling

```bash
kubectl scale deployment app-alpha --replicas=3
kubectl scale deployment app-beta --replicas=1
```

## Kill Pods Manually

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

With the test environment running, you'll observe:

1. **Pod crashes** - Every 30-120s pods exit and k8s restarts them
2. **Endpoint removal** - Crashed pods are removed from EndpointSlices
3. **Endpoint addition** - New pods added once readiness probe passes
4. **Scaling changes** - If chaos.sh is running, replica counts fluctuate

This mimics real production churn and helps test that the sidecar correctly tracks endpoint changes.
