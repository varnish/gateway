# Test Data

This directory contains CRDs and other test fixtures for envtest-based integration tests.

## Gateway API CRDs

The `gateway-api-crds.yaml` file contains the standard Gateway API CRDs (Gateway, GatewayClass, HTTPRoute, etc.) from the official Kubernetes Gateway API project.

These CRDs are required for envtest to function properly, as envtest runs a real Kubernetes API server that needs the actual CRD definitions (not just Go types).

### Updating Gateway API CRDs

If you need to update to a newer version of the Gateway API:

```bash
# Replace v1.2.0 with the desired version
curl -sL https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml \
  -o internal/controller/testdata/gateway-api-crds.yaml
```

### Why not use the Go types?

The `gatewayv1.Install(scheme)` function only registers the Go types with the scheme - it doesn't install the actual CRDs in the API server. Envtest needs the YAML definitions to create the CustomResourceDefinitions in its embedded etcd.
