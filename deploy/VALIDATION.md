# Deploy Directory Validation

## File Checklist

### 00-prereqs.yaml
- Namespace: `varnish-gateway-system`
- CRD: `gatewayclassparameters.gateway.varnish-software.com`
- Defines the GatewayClassParameters CRD for user VCL injection and varnishd args

### 01-operator.yaml
- ServiceAccount: `varnish-gateway-operator`
- ClusterRole: operator permissions for Gateway API resources
- ClusterRoleBinding: binds operator SA to role
- Deployment: operator controller

### 02-chaperone-rbac.yaml
- ClusterRole: `varnish-gateway-chaperone`
- Permissions for chaperone to watch EndpointSlices and Services
- ClusterRoleBinding: binds to all ServiceAccounts in default namespace

### 03-gatewayclass.yaml
- ConfigMap: `varnish-user-vcl` with example user VCL
- GatewayClassParameters: `varnish-params` with example varnishd args
- GatewayClass: `varnish` with controller name and parameters reference

### 04-sample-gateway.yaml
- Gateway: `varnish-gateway` with a single HTTP listener on port 80
- Users should create their own HTTPRoute resources referencing this Gateway

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

# 5. Sample Gateway
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
kubectl get gateway varnish-gateway -n varnish-gateway-system -o yaml
```
