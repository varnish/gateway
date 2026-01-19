# Deploy Directory Validation

## Status: ✅ All files valid

All YAML files have been validated with `kubectl apply --dry-run=client`.

## File Checklist

### 00-prereqs.yaml ✅
- Namespace: `varnish-gateway-system`
- CRD: `gatewayclassparameters.gateway.varnish-software.com`
- Defines the GatewayClassParameters CRD for user VCL injection and varnishd args

### 01-operator.yaml ✅
- ServiceAccount: `varnish-gateway-operator`
- ClusterRole: operator permissions for Gateway API resources
- ClusterRoleBinding: binds operator SA to role
- Deployment: operator controller
- All RBAC permissions are appropriate for operator functionality

### 02-chaperone-rbac.yaml ✅
- ClusterRole: `varnish-gateway-chaperone`
- Permissions for chaperone to watch EndpointSlices and Services
- ClusterRoleBinding: binds to all ServiceAccounts in default namespace
- Required for backend discovery and routing updates

### 03-gatewayclass.yaml ✅
- GatewayClassParameters: `varnish-params` with example varnishd args
- GatewayClass: `varnish` with controller name and parameters reference
- Valid parametersRef linking to GatewayClassParameters

### 04-sample-gateway.yaml ✅ (Updated for Phase 2)
**Status: Updated to demonstrate Phase 2 path-based routing features**

Resources:
- Gateway: `varnish-gateway` (unchanged)
- HTTPRoute: `api-route` - demonstrates path prefix matching with specificity
- HTTPRoute: `blog-route` - demonstrates regex path matching
- HTTPRoute: `simple-route` - demonstrates basic host routing (Phase 1 compatibility)

#### Example 1: api.example.com (Path Prefix Specificity)
```yaml
Rules ordered by specificity:
1. Exact: /api/v1/health → app-alpha
2. Prefix: /api/v2/* → app-beta
3. Prefix: /api/v1/* → app-gamma
4. Prefix: /api/* → app-delta
5. Default: * → app-epsilon
```

#### Example 2: blog.example.com (Regex Matching)
```yaml
Rules with regex patterns:
1. Exact: /admin → app-alpha
2. Regex: /posts/[0-9]+ → app-beta (numeric IDs)
3. Prefix: /posts/* → app-gamma (other posts)
4. Default: * → app-delta
```

#### Example 3: simple.example.com (Phase 1 Compatibility)
```yaml
Basic host routing without path matching:
- All paths → app-alpha
```

## Phase 2 Features Demonstrated

The updated sample gateway demonstrates all Phase 2 functionality:

1. ✅ **Exact Path Matching** - `type: Exact`
   - `/api/v1/health`, `/admin`

2. ✅ **Prefix Path Matching** - `type: PathPrefix`
   - `/api/v2/`, `/api/v1/`, `/api/`, `/posts/`

3. ✅ **Regex Path Matching** - `type: RegularExpression`
   - `^/posts/[0-9]+$` (numeric IDs)

4. ✅ **Route Specificity Ordering**
   - More specific matches take precedence
   - Exact > Regex > Prefix > Default

5. ✅ **Backward Compatibility**
   - Phase 1 host-only routing still works (simple-route)

## Testing the Sample Routes

After deploying the sample gateway, test with:

```bash
export GATEWAY_IP=<your-gateway-ip>

# Test exact match
http http://$GATEWAY_IP/api/v1/health Host:api.example.com
# Expected: {"app": "alpha"}

# Test prefix specificity
http http://$GATEWAY_IP/api/v2/users Host:api.example.com
# Expected: {"app": "beta"}

http http://$GATEWAY_IP/api/v1/products Host:api.example.com
# Expected: {"app": "gamma"}

# Test regex matching
http http://$GATEWAY_IP/posts/123 Host:blog.example.com
# Expected: {"app": "beta"}

# Test simple host routing
http http://$GATEWAY_IP/ Host:simple.example.com
# Expected: {"app": "alpha"}
```

## Changes Made

### 04-sample-gateway.yaml
- **Before**: Only Phase 1 host-based routing (alpha.example.com, beta.example.com)
- **After**: Comprehensive Phase 2 examples with path matching
- **Breaking**: Route names changed (alpha-route, beta-route → api-route, blog-route, simple-route)
- **Impact**: If you have existing deployments using the old routes, you'll need to update them

## Deployment Order

```bash
# 1. Prerequisites (namespace + CRDs)
kubectl apply -f deploy/00-prereqs.yaml

# 2. Operator
kubectl apply -f deploy/01-operator.yaml

# 3. Chaperone RBAC
kubectl apply -f deploy/02-chaperone-rbac.yaml

# 4. GatewayClass
kubectl apply -f deploy/03-gatewayclass.yaml

# 5. Deploy test applications (required for sample routes)
kubectl apply -f hack/test-env/deployments.yaml

# 6. Sample Gateway and Routes
kubectl apply -f deploy/04-sample-gateway.yaml
```

## Validation Commands

```bash
# Validate all deploy files
for file in deploy/*.yaml; do
  echo "Validating $file..."
  kubectl apply --dry-run=client -f "$file"
done

# Check Gateway status
kubectl get gateway varnish-gateway -n default -o yaml

# Check HTTPRoute status
kubectl get httproute -n default
kubectl describe httproute api-route
```
