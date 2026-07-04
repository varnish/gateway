# TODO

Items deferred from the `internal/controller` cruft review (2026-07-04). These are judged
riskier than straightforward cleanup — they need a deliberate decision, not a mechanical fix.

## 1. Silent no-op on unhandled resource types in `reconcileResource`

`internal/controller/gateway_controller.go` — `reconcileResource`'s type-switch falls
through to `return nil` for any `client.Object` that isn't a ConfigMap, Deployment, Service,
PodDisruptionBudget, or `-tls`-suffixed Secret. If a future resource type is added to the
`resources` slice in `reconcileResources` without adding a matching case here, it will be
created once on first reconcile and then silently never updated again — no error, no log.

Options: add a debug/warn log in the fallthrough branch so silent no-ops are at least
visible, or restructure so new resource types must opt into an update strategy (e.g. an
interface each desired resource implements) rather than relying on someone remembering to
extend the switch.

## 2. Two independent route/listener matching algorithms

`internal/controller/httproute_controller.go` — `AttachedRoutes` counting
(`countRoutesForListener` / `routeAttachesToListener`) and route acceptance
(`isRouteAllowedByGateway` / `listenerAllowsRouteNamespace` / `hostnamesIntersect`) each
reimplement route-to-listener matching independently, with different rules (acceptance
checks namespace policy, attachment counting checks sectionName/port/hostname). This split
looks intentional per Gateway API semantics, but it isn't documented as such — a future
editor could plausibly try to unify them and subtly break status semantics.

Needs: either a comment cross-referencing the two functions explaining why they can't be
merged, or confirmation that they truly diverge and should stay separate, written down
somewhere durable (code comment, not just this TODO).
