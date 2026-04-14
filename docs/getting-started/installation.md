# Installation Guide

This guide covers installing, upgrading, and uninstalling Varnish Gateway. Helm is the recommended path; kubectl manifests are available as an alternative.

## Prerequisites

- Kubernetes 1.26+
- kubectl configured to access your cluster
- Helm 3.8+ (if using the Helm method)
- [Gateway API CRDs](https://gateway-api.sigs.k8s.io/guides/#installing-gateway-api) installed in your cluster:

  ```bash
  kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml
  ```

## Helm

### Install

Install the chart from the OCI registry:

```bash
helm install varnish-gateway oci://ghcr.io/varnish/charts/varnish-gateway \
  --namespace varnish-gateway-system \
  --create-namespace
```

This installs:

- Varnish-specific CRDs: GatewayClassParameters, VarnishCacheInvalidation, and VarnishCachePolicy
- The operator deployment and RBAC
- A default GatewayClass named `varnish`

### Install a specific version

```bash
helm install varnish-gateway oci://ghcr.io/varnish/charts/varnish-gateway \
  --version v0.19.2 \
  --namespace varnish-gateway-system \
  --create-namespace
```

### Install with custom values

```bash
helm install varnish-gateway oci://ghcr.io/varnish/charts/varnish-gateway \
  --namespace varnish-gateway-system \
  --create-namespace \
  -f my-values.yaml
```

See [charts/varnish-gateway/README.md](../../charts/varnish-gateway/README.md) for all configuration options.

### Upgrade

Helm installs CRDs from a chart's `crds/` directory on first install but [does not touch them on upgrade or uninstall](https://helm.sh/docs/chart_best_practices/custom_resource_definitions/), this is a deliberate Helm design choice to avoid accidentally destroying data held in custom resources. If the Varnish-specific CRDs (`GatewayClassParameters`, `VarnishCacheInvalidation`, `VarnishCachePolicy`) have changed between your installed version and the target version, apply them manually before upgrading:

```bash
# Pull the chart to get the CRDs for the new version
helm pull oci://ghcr.io/varnish/charts/varnish-gateway --version vX.Y.Z --untar
kubectl apply -f varnish-gateway/crds/
```

If CRDs are unchanged between versions, this step is a no-op and can be skipped — applying them is always safe. Release notes call out CRD changes when they happen.

Then upgrade the release:

```bash
helm upgrade varnish-gateway oci://ghcr.io/varnish/charts/varnish-gateway \
  --version vX.Y.Z \
  --namespace varnish-gateway-system
```

This covers the Varnish-specific CRDs only. The Gateway API CRDs (`Gateway`, `HTTPRoute`, etc.) are upstream and upgraded separately — see the [Gateway API release notes](https://github.com/kubernetes-sigs/gateway-api/releases).

### Uninstall

```bash
helm uninstall varnish-gateway --namespace varnish-gateway-system
```

**Warning:** deleting the CRDs also deletes all Gateway, HTTPRoute, and related resources in the cluster.

```bash
kubectl delete crd gatewayclassparameters.gateway.varnish-software.com \
                   varnishcacheinvalidations.gateway.varnish-software.com \
                   varnishcachepolicies.gateway.varnish-software.com
```

## kubectl (Alternative)

Kubernetes manifests live in `deploy/`:

```
deploy/
├── 00-prereqs.yaml       # Namespace + GatewayClassParameters CRD
├── 01-operator.yaml      # ServiceAccount, ClusterRole, ClusterRoleBinding, Deployment
├── 02-chaperone-rbac.yaml # ClusterRole for chaperone to watch EndpointSlices
├── 03-gatewayclass.yaml  # GatewayClass "varnish"
└── sample-gateway.yaml   # Sample Gateway (not applied by default)
```

### Install

```bash
kubectl apply -f deploy/
```

This installs the same resources as the Helm chart, without templating.

### Upgrade

Re-apply the manifests for the new version:

```bash
kubectl apply -f deploy/
```

### Uninstall

```bash
kubectl delete -f deploy/
```

To also remove the Gateway API CRDs:

```bash
kubectl delete -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml
```

## Verify Installation

Check the operator is running:

```bash
kubectl get pods -n varnish-gateway-system
```

You should see:

```
NAME                                        READY   STATUS    RESTARTS   AGE
varnish-gateway-operator-xxxxxxxxxx-xxxxx   1/1     Running   0          30s
```

Check the GatewayClass was created:

```bash
kubectl get gatewayclass
```

You should see:

```
NAME      CONTROLLER                      ACCEPTED   AGE
varnish   varnish-software.com/gateway    True       1m
```

## Troubleshooting

### View operator logs

```bash
kubectl logs -n varnish-gateway-system -l app.kubernetes.io/component=operator -f
```

### GatewayClass not accepted

```bash
kubectl describe gatewayclass varnish
```

For Gateway- and HTTPRoute-level issues, see [First Gateway → Troubleshooting](first-gateway.md#troubleshooting) and [Operations → Troubleshooting](../operations/troubleshooting.md).

## Next Steps

- [First Gateway](first-gateway.md) — create your first Gateway and HTTPRoute
- [GatewayClassParameters](../reference/gatewayclassparameters.md) — advanced configuration
- [Custom VMODs](../operations/custom-vmods.md) — load additional VMODs via a custom image or init container
- [Upgrades](../operations/upgrades.md) — upgrading an existing installation
