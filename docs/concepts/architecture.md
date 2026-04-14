# Architecture Overview

Varnish Gateway implements the Kubernetes Gateway API using Varnish as the data
plane. It is split into three components that run in separate processes:

| Component | Runs as                            | Role                                                                                                                          |
| --------- | ---------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| operator  | Cluster-wide Deployment            | Watches Gateway API resources; generates VCL and routing config; manages gateway Deployments, Services, ConfigMaps, and RBAC. |
| chaperone | Varnish wrapper in the gateway pod | Supervises varnishd; watches routing config and EndpointSlices; triggers reloads.                                             |
| ghost     | Rust VMOD loaded by varnishd       | Performs per-request routing inside Varnish.                                                                                  |

The gateway pod itself runs a single container that includes varnishd, the
ghost VMOD, and chaperone (as PID 1).

## Motivation

### Why chaperone, not a plain varnishd container

`varnishd` on its own cannot watch Kubernetes resources. Something in the pod has to resolve Service names to pod IPs using EndpointSlices, watch the ConfigMap for VCL and routing changes, and drive the reload protocols — varnishadm for VCL, HTTP for ghost. Chaperone is that something. Running it as PID 1 gives it a clean place to own varnishd's lifecycle: signal handling, graceful shutdown, restart on crash.

### Why ghost is a VMOD, why not generate VCL

Generating VCL risks producing something invalid, which would be fatal at load time. VCL compilation is also slow and involves multiple steps that can fail. To avoid this, ghost implements the routing rules as a VMOD that reads its configuration at runtime.

## Data flow

```
Gateway / HTTPRoute                      EndpointSlices
        │                                      │
        ▼                                      │
   ┌─────────┐                                 │
   │ Operator│─── routing.json ──┐             │
   │         │─── main.vcl ──────┤             │
   └─────────┘                   │             │
                                 ▼             ▼
                          ┌───────────────────────┐
                          │      Chaperone        │
                          │  (in the gateway pod) │
                          │                       │
                          │  merges routing.json  │
                          │  + EndpointSlices     │
                          │  → ghost.json         │
                          └───────────────────────┘
                                 │             │
                       varnishadm│             │HTTP /.varnish-ghost/reload
                                 ▼             ▼
                          ┌──────────────────────┐
                          │       varnishd       │
                          │     (ghost VMOD)     │
                          └──────────────────────┘
```

1. The operator watches Gateway, HTTPRoute, and `GatewayClassParameters`
   resources. It writes two artifacts into the gateway's ConfigMap:
   `main.vcl`, generated preamble plus user VCL, and `routing.json`,
   host → service mappings with match criteria.

2. Chaperone, running in the gateway pod, watches the ConfigMap plus the
   EndpointSlices for every Service referenced by `routing.json`. It merges
   them into `ghost.json` — the same routing tree, but with Service names
   resolved to concrete pod addresses.

3. varnishd, with the ghost VMOD loaded, consults `ghost.json` on every
   request. Ghost matches the request against the routes for its vhost and
   listener, picks a weighted backend group, and dispatches to a pod.

## Controller separation

The operator runs two reconcilers with non-overlapping responsibilities:

HTTPRoute reconciler:

- Primary watch: HTTPRoute
- Also watches: Gateway (to resolve parent refs and update status.listeners[].attachedRoutes)
- Produces: the routing.json key of the gateway's ConfigMap
- Also updates: AttachedRoutes counts on the parent Gateway's listener status, and status on the HTTPRoute itself (Accepted / ResolvedRefs)

Gateway reconciler

- Primary watch: Gateway
- Also watches: GatewayClassParameters, and the user-VCL ConfigMap referenced by it
- Produces:
  - The main.vcl key of the gateway's ConfigMap (generated preamble + user VCL)
  - The gateway Deployment, Service, ServiceAccount, Role/RoleBinding, and any TLS Secret
  - The varnish.io/infra-hash annotation on the pod template (the signal that decides whether pods need rolling)
- Also updates: status on the Gateway (Accepted / Programmed)

## Reloads and restarts

Three kinds of change can happen to a gateway, and each has a different
propagation path:

| Change                               | Propagation                                                                             | Downtime        |
| ------------------------------------ | --------------------------------------------------------------------------------------- | --------------- |
| HTTPRoute / Service endpoints        | `ghost.json` regenerated; ghost hot-reloads via HTTP                                    | None            |
| User VCL                             | `main.vcl` regenerated; varnishadm reloads VCL in place                                 | None            |
| Listener ports, image, varnishd args | Infrastructure hash changes on the Deployment's pod template; Kubernetes rolls the pods | Rolling restart |

The `varnish.io/infra-hash` annotation is the single signal that
distinguishes "config change" from "pod-level change". If it doesn't change,
pods aren't touched.

See [reload-paths.md](reload-paths.md) for the detailed reload mechanics.

## Multi-listener model

Each Gateway listener maps to a Varnish `-a` socket named `{proto}-{port}`
(e.g., `http-80`, `https-443`). Multiple Gateway listeners that share a port
collapse into a single Varnish socket — hostname isolation is handled by
ghost's vhost routing rather than by separate sockets. Container ports equal
listener ports; there is no port translation.

See [multi-listener.md](multi-listener.md) for details and the
`X-Gateway-Listener` / `X-Gateway-Route` request headers ghost sets.

## VCL composition

Generated VCL and user VCL are combined by concatenation, relying on
Varnish's ability to define a subroutine (e.g., `vcl_recv`) multiple times
and run each body in order. This avoids parsing user VCL in the operator and
keeps the generator's surface small. Users who need pre-routing logic can
define their own `vcl_recv` — it runs after the generator's.

See [vcl-merging.md](vcl-merging.md) for which subroutines are safe to
override and the caveats around `vcl_synth` / `vcl_backend_error`.

## Caching

By default — without a `VarnishCachePolicy` attached to a route — ghost sets
pass on every request, so the gateway behaves as a plain reverse proxy. This
matches Gateway API expectations: routing is the base contract, caching is
opt-in per route.

See [caching-model.md](caching-model.md) for cache policies and
invalidation.

## See also

- [Multi-listener model](multi-listener.md)
- [VCL merging](vcl-merging.md)
- [Reload paths](reload-paths.md)
- [Caching model](caching-model.md)
