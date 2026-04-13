# Varnish Gateway Documentation

Kubernetes Gateway API implementation using Varnish.

## Getting Started

- [Installation](getting-started/installation.md)
- [First Gateway](getting-started/first-gateway.md)

## Concepts

- [Architecture Overview](concepts/architecture.md) — TODO
- [Multi-Listener Model](concepts/multi-listener.md) — TODO
- [VCL Merging](concepts/vcl-merging.md) — TODO
- [Reload Paths](concepts/reload-paths.md) — TODO
- [Caching Model](concepts/caching-model.md) — TODO
- [Gateway Topology](concepts/gateway-topology.md)

## Guides

- [Canary Deployments](guides/canary-deployments.md)
- [Logging](guides/logging.md)
- [Custom VCL](guides/custom-vcl.md) — TODO
- [Cache Invalidation](guides/cache-invalidation.md) — TODO
- [TLS](guides/tls.md) — TODO

## Operations

- [Resources and Scaling](operations/resources-and-scaling.md)
- [Pod Disruption Budgets](operations/pod-disruption-budgets.md) — TODO
- [Horizontal Pod Autoscaling](operations/horizontal-pod-autoscaling.md)
- [Upgrades](operations/upgrades.md)
- [Custom VMODs](operations/custom-vmods.md)
- [Observability](operations/observability.md) — TODO
- [Troubleshooting](operations/troubleshooting.md) — TODO

## Reference

- [GatewayClassParameters](reference/gatewayclassparameters.md)
- [VarnishCachePolicy](reference/varnishcachepolicy.md)
- [VarnishCacheInvalidation](reference/varnishcacheinvalidation.md)
- [varnishd Arguments](reference/varnishd-args.md) — TODO (currently covered in GatewayClassParameters)
