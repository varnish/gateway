# Gateway Topology

A Gateway resource's namespace determines where its Varnish data plane runs. The operator creates the Deployment, Service, and ConfigMap in the same namespace as the Gateway resource. The operator itself lives in `varnish-gateway-system` regardless. This is purely about where Gateway resources, and therefore Varnish pods, live.

Two patterns are common. Both are supported by the Gateway API.

## Shared Gateway

One Gateway resource in a dedicated namespace, often `varnish-gateway-system`, `ingress`, or a platform-team namespace. Application teams create HTTPRoutes in their own namespaces and attach them to the shared Gateway.

```
varnish-gateway-system/      # platform team
  Gateway: public-gateway
  (Varnish pods run here)

team-a/                      # app team
  HTTPRoute → public-gateway
  Service, Deployment

team-b/                      # app team
  HTTPRoute → public-gateway
  Service, Deployment
```

Cross-namespace attachment requires two Gateway API mechanisms:

- The Gateway's `listeners[].allowedRoutes.namespaces` must permit routes from app namespaces (e.g. `from: All` or a label selector).
- A `ReferenceGrant` in each app namespace permits the Gateway to read the route's target Services.

### When to use

- **Caching is a primary goal.** One large cache has a materially better hit ratio than several small ones for the same total memory — hot objects from any tenant stay resident, and the LRU works across a bigger working set. If you're caching heavily, this is usually the decisive factor.
- A platform team owns ingress; app teams own routes.
- Policy (TLS certs, rate limits, logging) is centrally managed.

### Tradeoffs

- Cache is shared — a noisy tenant can evict another tenant's hot objects. Tune eviction carefully, or partition memory per vhost if isolation matters more than efficiency.
- Blast radius: a Varnish restart affects every app attached to the Gateway.
- More ceremony (allowedRoutes, ReferenceGrant) for each app onboarding.

## Gateway Per Application

Each app namespace owns its own Gateway, HTTPRoute, Service, and Deployment.

```
team-a/
  Gateway: team-a-gateway
  HTTPRoute, Service, Deployment
  (Varnish pods run here)

team-b/
  Gateway: team-b-gateway
  HTTPRoute, Service, Deployment
  (Varnish pods run here)
```

### When to use

- Teams are autonomous and want to own their full stack.
- Isolation matters — separate caches, independent restart domains, per-team VCL.
- Small number of apps, or apps with very different traffic characteristics.

### Tradeoffs

- Varnish pods per namespace — higher resource footprint, and cache memory is fragmented across pods. For pure proxy / low-cache-ratio workloads this barely matters; for cache-heavy workloads it's a real cost in hit ratio.
- Each team needs to manage TLS certs, VCL, and scaling themselves.

## Which should I pick?

The main axis is **how much you rely on the cache**:

- **Cache-heavy** (high hit ratios, large working set, content you want Varnish to actually retain) → lean toward **shared Gateway**. Cache efficiency scales with pool size; many small caches waste memory on duplicated hot objects and evict usefully cached ones sooner.
- **Pure proxy / light caching** (mostly pass-through, TLS termination, routing) → **Gateway per application** is fine and often simpler. The cache-sharing argument doesn't apply when there's little to cache.

Secondary factors: platform team vs. autonomous teams, blast-radius tolerance, and how much operational ceremony you want around onboarding.

Start with **Gateway per application** if you're evaluating or have a handful of apps — it's simpler to reason about and the quickstart uses this pattern. Switching later is mostly a matter of moving the Gateway resource to a central namespace and adding `allowedRoutes` + `ReferenceGrant`; HTTPRoutes don't need to change structurally.

Nothing prevents mixing: a shared public Gateway for most apps plus a dedicated Gateway for a team that needs isolation is a reasonable configuration.
