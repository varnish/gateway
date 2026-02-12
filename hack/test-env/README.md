# Test Environment for Varnish Gateway

This directory contains test resources for validating the Varnish Gateway implementation.

## Components

### Test Applications

- **deployments.yaml**: Five simple HTTP services (alpha, beta, gamma, delta, epsilon) that echo back JSON responses
- Each service returns: `{"app": "name", "hostname": "pod-name", "path": "request-path"}`
- Each deployment runs 2 replicas for load balancing tests

### Phase 1 Tests (Basic Host-Based Routing)

Phase 1 routes are defined in `/deploy/sample-gateway.yaml`:
- `alpha.example.com` → app-alpha
- `beta.example.com` → app-beta

### Phase 2 Tests (Path-Based Routing)

- **phase2-routes.yaml**: HTTPRoutes demonstrating path-based routing features
- **test-phase2.sh**: Automated test suite for Phase 2 features

## Setup

### 1. Deploy test applications

```bash
kubectl apply -f hack/test-env/deployments.yaml
```

Wait for all pods to be ready:

```bash
kubectl get pods -w
```

### 2. Deploy Phase 1 routes (if not already deployed)

```bash
kubectl apply -f deploy/sample-gateway.yaml
```

### 3. Deploy Phase 2 routes

```bash
kubectl apply -f hack/test-env/phase2-routes.yaml
```

## Testing Phase 1 (Basic Host Routing)

Get the gateway service IP:

```bash
export GATEWAY_IP=$(kubectl get svc varnish-gateway-default -n default -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
```

Test basic host routing with curl:

```bash
# Should route to app-alpha
curl -H "Host: alpha.example.com" http://$GATEWAY_IP/ | jq

# Should route to app-beta
curl -H "Host: beta.example.com" http://$GATEWAY_IP/ | jq
```

Or with HTTPie:

```bash
http http://$GATEWAY_IP Host:alpha.example.com
http http://$GATEWAY_IP Host:beta.example.com
```

## Testing Phase 2 (Path-Based Routing)

### Automated Test Suite

Run all Phase 2 tests with a single command:

```bash
export GATEWAY_IP=24.144.77.118  # or your gateway IP
./hack/test-env/test-phase2.sh
```

### Manual Testing

Test exact path match (should return alpha):

```bash
curl -H "Host: api.example.com" http://$GATEWAY_IP/api/v1/health | jq
# or with HTTPie:
http http://$GATEWAY_IP/api/v1/health Host:api.example.com
```

Test prefix matching:

```bash
# /api/v2/* → beta
curl -H "Host: api.example.com" http://$GATEWAY_IP/api/v2/users | jq
http http://$GATEWAY_IP/api/v2/users Host:api.example.com

# /api/v1/* → gamma
curl -H "Host: api.example.com" http://$GATEWAY_IP/api/v1/products | jq
http http://$GATEWAY_IP/api/v1/products Host:api.example.com
```

Test regex matching:

```bash
# Numeric post IDs → beta
curl -H "Host: blog.example.com" http://$GATEWAY_IP/posts/123 | jq
http http://$GATEWAY_IP/posts/123 Host:blog.example.com

# UUID post IDs → gamma
curl -H "Host: blog.example.com" http://$GATEWAY_IP/posts/550e8400-e29b-41d4-a716-446655440000 | jq
http http://$GATEWAY_IP/posts/550e8400-e29b-41d4-a716-446655440000 Host:blog.example.com
```

Test route ordering:

```bash
# Exact match wins → alpha
http http://$GATEWAY_IP/users/admin Host:mixed.example.com

# Regex match → beta
http http://$GATEWAY_IP/users/123 Host:mixed.example.com

# Prefix fallback → gamma
http http://$GATEWAY_IP/users/john Host:mixed.example.com
```

## Phase 2 Test Coverage

The test suite validates:

### Suite 1: api.example.com (Specificity)
- ✓ Exact match takes precedence over prefix
- ✓ More specific prefix matches first
- ✓ Broader prefix matches later
- ✓ Default fallback for unmatched paths

### Suite 2: static.example.com (Exact Paths)
- ✓ Multiple exact path matches
- ✓ Prefix match for /assets/
- ✓ Default handler for unmatched paths

### Suite 3: blog.example.com (Regex Matching)
- ✓ Exact match for /admin
- ✓ Regex match for numeric IDs: `/posts/[0-9]+`
- ✓ Regex match for UUIDs: `/posts/{uuid}`
- ✓ Prefix fallback for other /posts/ paths
- ✓ Prefix match for /categories/

### Suite 4: mixed.example.com (Overlapping Routes)
- ✓ Exact match takes highest precedence
- ✓ Regex match takes precedence over prefix
- ✓ Prefix matches with proper ordering
- ✓ Root prefix as catch-all

## Chaos Testing

Induce pod failures and watch endpoint updates:

```bash
# Delete random pods every 30s
./hack/test-env/chaos.sh

# Watch endpoints being updated
./hack/test-env/watch-endpoints.sh
```

## Debugging

### Check HTTPRoute status

```bash
kubectl get httproute -o yaml
```

### Check gateway logs

```bash
kubectl logs -n default -l app=varnish-gateway --tail=100 -f
```

### Check ghost.json configuration

```bash
# Get the ConfigMap containing routing config
kubectl get configmap -n default -l gateway.varnish/config=routing -o yaml

# Exec into gateway pod and check ghost.json
POD=$(kubectl get pods -n default -l app=varnish-gateway -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n default $POD -- cat /etc/varnish/ghost.json
```

### Manual routing inspection

```bash
# See all services
kubectl get svc

# See all endpoints
kubectl get endpoints

# See EndpointSlices (used by chaperone)
kubectl get endpointslices
```

## Cleanup

```bash
# Remove Phase 2 routes
kubectl delete -f hack/test-env/phase2-routes.yaml

# Remove all test apps
kubectl delete -f hack/test-env/deployments.yaml

# Remove gateway and Phase 1 routes
kubectl delete -f deploy/sample-gateway.yaml
```
