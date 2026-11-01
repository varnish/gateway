# Route VCL: Caveats and Limits

!!! warning "Preview"
    Route VCL ([VarnishVCLPolicy](varnishvclpolicy.md)) is a preview feature. Read this page before you turn it on. Several of these limits follow from how Varnish selects a VCL; they are not bugs that a later release will remove.

Every item states the mechanism and what to do instead.

## How it works, in one paragraph

The gateway's master VCL runs first for every request. Ghost picks the backend and, if the matched route has a policy, sets the internal header `X-Ghost-VCL-Slot`. The master's dispatch block then does `return (vcl(gwvcl-N))`, which hands the request to the route VCL that the label `gwvcl-N` points at. A route VCL is the gateway preamble, then the route's own VCL, then the gateway user VCL, then the postamble, compiled as one document. The dispatch happens in the master's preamble, before the master's copy of the gateway user VCL, so for a request on a slotted route the gateway user VCL runs only inside the route VCL.

## Routing and flow control

### One hop only

Varnish allows a single `return (vcl(...))` per request. A route VCL that itself does `return (vcl(...))` gets a `503`. A route VCL that names another slot's label is not blocked at compile time (a label that points back at itself is refused), but the jump fails at runtime. **Instead:** put shared logic in the gateway VCL (`userVCLConfigMapRef`), which is composed into every route VCL.

### `return (restart)` stays where it was issued

Varnish refuses `return (vcl(...))` after a restart (`Illegal return(vcl): Not after restarts`), so the master dispatches only when `req.restarts == 0`. A restart issued in the master therefore stays in the master: the route VCL does not run for the restarted request. A restart issued inside a route VCL stays in that route VCL. **Instead:** if a route needs restart logic, implement it entirely inside the route VCL, and do not rely on the route VCL running after a master-side restart.

### ESI subrequests always run in the master

The dispatch is guarded by `req.esi_level == 0`, and Varnish resets an ESI subrequest to the active VCL (`cache_esi_deliver.c`). Fragments included with `<esi:include>` run the master VCL, never the route VCL of the parent request. **Instead:** do not depend on route VCL for fragment URLs. Handle them in the gateway VCL.

### Attach window

`vcl_slot` in ghost.json and the route VCL documents reach a pod by two independent paths (the HTTPRoute controller writes routing.json, the Gateway controller writes the route VCL ConfigMap, and the chaperone compiles the slot). A newly slotted route can answer `503` with `X-Ghost-Route-VCL: unprogrammed` from the placeholder for a short time, until the chaperone's slot pass lands. The same applies to a pod that has just started. **Instead:** wait for `Programmed=True` on the policy before sending traffic that must run the route VCL.

## Cache and trust

### The cache is global

Objects are not partitioned by route VCL. Two routes that produce the same hash (same host and URL, unless one of them changes the hash) share objects. A route VCL can read, poison or ban other routes' objects: it can set `req.http.host` or `req.url` to another route's values, choose its own backend and have the response cached under the other route's key, or call `std.ban()` on anything. This is part of the [trust statement](varnishvclpolicy.md#trust-statement): attaching VCL to a route is code execution in the gateway pod. Per-route cache partitioning is a non-goal of this feature; adding `hash_data()` per slot would also break master-side PURGE for route objects. **Instead:** enable route VCL only where everyone who can create an HTTPRoute is trusted with the gateway.

### Internal headers

`X-Ghost-*` headers are stripped at the trust boundary in both the master and every route VCL (in `vcl_recv`, and for `bereq` in `vcl_backend_fetch` and `vcl_pipe`), and never reach backends. A client cannot choose its own slot. `X-Gateway-Route` and `X-Gateway-Listener` are still set inside the route VCL, so route VCL can branch on them as the gateway VCL can.

## Composition and cost

### Gateway VCL is composed into every route VCL

A route VCL document is `preamble + route VCL + gateway user VCL + postamble`. Varnish joins same-named subroutines in that order, so in every subroutine the route's code runs first and the gateway user VCL runs after it and has the last word.

A `return` ends the subroutine for everything after it. A gateway user VCL that ends `vcl_recv` with `return (pass)` (the shipped default in `deploy/03-gatewayclass.yaml`) therefore still lets the route's `vcl_recv` run. The other way round, a `return` in a route's `vcl_recv` skips the gateway user VCL's `vcl_recv` and the postamble, so ghost's deferred `pass` is skipped too: a route that does `return (hash)` is cached even without a cache policy. This is the rule the gateway user VCL already has toward the postamble. It also means gateway-wide normalisation or security headers in the gateway user VCL run for a route only when the route VCL does not `return` before them. That is not a security boundary (route VCL is code execution, see the [trust statement](varnishvclpolicy.md#trust-statement)); it is a convention between the gateway and route owners. **Instead:** keep gateway-wide logic that must always run in subroutines route owners leave alone, or ask route owners not to `return` from the subroutines it lives in.

The cost of composing is duplication:

- Gateway-level `vcl_init` objects, probes and backends in the gateway user VCL are instantiated **once per populated slot**, plus once in the master. Declare few of them, or prefer ghost-managed backends.
- A change to the gateway user VCL changes every route VCL's hash, so **every route VCL is recompiled on every pod**. See the compile cost below.

### Compile cost and startup

A route VCL costs about 0.2 s of CPU to compile (measured unthrottled on a 16-core workstation, Varnish 9.0.0, no CPU limit; expect more under a pod CPU limit). The chaperone compiles serially, one slot at a time, and a restarting pod waits for every slot before it starts varnishd, so the startup probe budget (180 s on operator-created Deployments) has to cover them all:

| Populated slots | Expected startup compile time (default throttle, ~0.2 s/compile) |
| --------------- | ------------------------------------------------------------------ |
| 1               | 0.2 s                                                              |
| 64 (default pool) | about 14 s                                                       |
| 256 (ceiling)   | about 60 s                                                         |

Compiles are rate-limited by a token bucket: `ROUTE_VCL_COMPILE_RATE` (default 4 per second) with `ROUTE_VCL_COMPILE_BURST` (default 8); `ROUTE_VCL_COMPILE_RATE=0` turns it off. The bucket starts full and refills while a compile runs, so a pod's first pass is hardly slowed down at the default; the throttle matters when compiles are faster than 0.25 s, or when you lower the rate to leave CPU for traffic (for example `1` is roughly 20% of a core). Only route VCL `vcl.inline` is throttled, never the master or the placeholder. See [chaperone environment variables](gatewayclassparameters.md#environment-variables).

## Failure behaviour

A route VCL that fails to compile does not affect other slots or the master:

- On a **new attach**, the slot stays on the placeholder: the route answers `503` with `X-Ghost-Route-VCL: unprogrammed`.
- On a **change**, the previous version keeps serving.
- The policy reports `Programmed=False`, reason `CompileFailed`, and the VCC compiler message is in `status.podResults[].message` (and in the condition message, for the first failing pod).
- The same document is not recompiled on every resync; a failure is remembered for five minutes or until the document changes.

**Instead:** check `kubectl get vvp` and the policy's status after every change, and detach the policy if you cannot fix it quickly.

## Capacity limits

### Pool exhaustion and the ConfigMap guard

Each Gateway has a fixed pool (`routeVCL.slotPoolSize`, default 64, ceiling 256). A policy that finds no free slot is reported `NoSlotAvailable` and its route keeps running the master VCL. Route VCLs travel in one ConfigMap per Gateway; the operator leaves a policy out (`ConfigMapFull`) when adding it would push the ConfigMap past 900 KiB, well below the API server's 1 MiB limit. With 256 slots of large documents this guard, not the pool, is the limit. Watch `varnish_gateway_route_vcl_unassigned`.

### Resizing the pool

Changing `slotPoolSize` hot-recompiles the master on every pod (no restart). Growing never moves an existing policy. Shrinking below an assigned slot takes that slot away: the policy is treated like a new one in the same pass and gets a free slot in the smaller pool if there is one, otherwise it waits with reason `SlotOutsidePool` and its route runs the master VCL.

### `max_vcl`

Varnish warns when more than `max_vcl` VCLs are loaded (default 100). The chaperone sets `max_vcl` to `2N + 10` for a pool of N slots, which leaves room for the replaced generations that linger until they are discarded. Labels do not count against `max_vcl`.

## Version

The mechanism (`return (vcl(label))`, `vcl.label`, always-warm labels, `max_vcl`) predates Varnish 9, and the figures on this page were measured on **Varnish 9.0.0**. The target is **9.1.0**, which is what the gateway image is built from; the compile times and startup numbers must be re-measured on it, in a pod with the default CPU request, before they are treated as final.

## See also

- [VarnishVCLPolicy reference](varnishvclpolicy.md)
- [Reload paths](../concepts/reload-paths.md#route-vcl-reload)
- [Observability](../operations/observability.md#route-vcl-metrics)
