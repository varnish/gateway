# Route VCL

!!! note "Preview"
    Route VCL is a preview feature. The API and behaviour may change before it
    leaves preview. See [Status of this feature](#status-of-this-feature).

Attach a VCL document to one HTTPRoute. Its VCL runs only for requests that
route matched. Other routes, and the gateway VCL, are not affected. This
includes the case where your document fails to compile: the other routes keep
serving, and so does the gateway.

For the gateway-wide version, see [Custom VCL](custom-vcl.md). For the resource
reference, see [VarnishVCLPolicy](../reference/varnishvclpolicy.md).

!!! danger "Trust statement"
    Attaching VCL to an HTTPRoute is code execution inside the gateway pod.
    Submitted VCL is not inspected before it reaches varnishd. A route VCL can:

    - Read the gateway's TLS private key. It is mounted from a Secret, and
      `std.fileread` can reach it.
    - Read the chaperone's ServiceAccount token. The pod can watch
      EndpointSlices and read and write VarnishCacheInvalidation resources.
    - Read the varnishadm shared secret. The manager, the child and the
      chaperone all run as the same user in the container.
    - Flush every other tenant's cached objects with `std.ban()`.
    - Declare arbitrary backends, with all the network access the pod has.

    Enable route VCL only where everyone who can create an HTTPRoute in a
    namespace routed by the GatewayClass is trusted with the gateway itself.
    There is no per-namespace allowlist.

## Enable it

Route VCL is off by default. Turn it on in the `GatewayClassParameters` that
your Gateways use:

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-params
spec:
  routeVCL:
    enabled: true
    slotPoolSize: 64   # optional, default 64, range 1-256
```

Each Gateway gets a fixed pool of `slotPoolSize` slots. Each accepted policy
uses one slot. Unused slots cost nothing. Each populated slot adds about 0.2 s
of compile time per pod. See
[`spec.routeVCL`](../reference/gatewayclassparameters.md#specroutevcl) for the
numbers.

Changing `routeVCL` does not restart pods. Turning it on or off, or changing
`slotPoolSize`, recompiles the master VCL in place.

## Worked example

This example sets a response header on every response from `api-route`, and
rejects requests for `/admin/` on that route.

1. Put the VCL in a ConfigMap. The key is `route.vcl` unless you say otherwise:

   ```yaml
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: api-vcl
     namespace: default
   data:
     route.vcl: |
       sub vcl_recv {
           if (req.url ~ "^/admin/") {
               return (synth(403, "Forbidden"));
           }
       }

       sub vcl_deliver {
           set resp.http.X-Api-Route = "api-route";
       }
   ```

2. Create the policy. It targets the HTTPRoute in the same namespace:

   ```yaml
   apiVersion: gateway.varnish-software.com/v1alpha1
   kind: VarnishVCLPolicy
   metadata:
     name: api-vcl
     namespace: default
   spec:
     targetRef:
       group: gateway.networking.k8s.io
       kind: HTTPRoute
       name: api-route
     vclConfigMapRef:
       name: api-vcl
       # key: route.vcl   # optional, defaults to "route.vcl"
   ```

3. Apply both and check the status:

   ```bash
   kubectl apply -f api-vcl.yaml
   kubectl get vvp api-vcl
   kubectl describe vvp api-vcl
   ```

   `kubectl get vvp` prints the target name and the `Accepted`, `Programmed`
   and `Age` columns. `Programmed` becomes `True` once every live gateway pod
   has compiled the document. Wait for it before you send traffic that must run
   the route VCL.

## What goes in the document

The document is composed as one unit:

```
preamble + your route VCL + gateway user VCL + postamble
```

So you write plain subroutines and `import` lines. Some rules follow from that:

- Write `sub vcl_recv`, `sub vcl_backend_response`, `sub vcl_deliver` and so
  on. Varnish joins same-named subroutines in declaration order, so in every
  subroutine your code runs after the gateway preamble (ghost has already
  picked the backend) and **before** the gateway user VCL.
- A `return` ends the subroutine for everything after it. A `return` in your
  `vcl_recv` skips the gateway user VCL's `vcl_recv` and the postamble, so
  ghost's deferred `pass` does not happen either: `return (hash)` makes the
  request cacheable even on a route without a cache policy. That is the same
  rule the gateway user VCL already has toward the postamble. Without a
  `return`, the gateway user VCL runs after your code and has the last word.
  A gateway VCL that ends `vcl_recv` with `return (pass)` (the shipped
  default) therefore still runs your `vcl_recv` first.
- You do not need `vcl 4.1;`. If you include it, it is dropped.
- `X-Gateway-Route` (`namespace/name`) and `X-Gateway-Listener`
  (`{proto}-{port}`) are set, as they are for the gateway VCL.
- Do not write `return (vcl(...))`. The gateway's master VCL does that jump.
- `return (restart)` stays inside your route VCL. A restart issued in the
  master does not run your VCL on the restarted request.

## Read the status

The operator writes one entry under `status.ancestors`, with two conditions.

**`Accepted`** says whether the policy is valid and applies:

| Reason | Meaning |
| ------ | ------- |
| `Accepted` | Valid, and route VCL is enabled on a parent Gateway the route is attached to. |
| `Invalid` | Bad spec, for example a wrong `targetRef`, or `sectionName` set. |
| `TargetNotFound` | The HTTPRoute does not exist in the policy's namespace. |
| `ConfigMapNotFound` | The ConfigMap, or the key in it, does not exist. |
| `Conflicted` | Another policy already targets the same HTTPRoute. The older one wins. |
| `Disabled` | `routeVCL.enabled` is not set on the GatewayClass of any parent Gateway. |
| `RouteNotAttached` | The route is not attached to the Gateway, for example because of `allowedRoutes`. |

**`Programmed`** says whether the document runs on the gateway pods:

| Reason | Meaning |
| ------ | ------- |
| `Programmed` | Every live pod runs the current document. |
| `Pending` | Some live pods have not reported the current document yet. |
| `CompileFailed` | A live pod could not compile the document. The message has the compiler output. |
| `NoSlotAvailable` | The Gateway could not give the policy a slot. The message says why. |
| `NotAccepted` | The policy is not `Accepted`. |

`NotProgrammed` (status `Unknown`) means no Gateway has picked the policy up
yet.

The policy's status also has:

- `status.slots`: the slot the policy holds on each parent Gateway.
- `status.podResults`: one entry per pod, with `programmed`, `hash` and
  `message`. The `message` is the compiler output for that pod. An entry whose
  `hash` does not match the current document is stale.

```yaml
status:
  slots:
    - gateway: default/my-gateway
      slot: 3
  podResults:
    - podName: my-gateway-7d9f5c-abcde
      slot: 3
      programmed: false
      message: "<compiler output for this pod>"
```

## Update and remove

**Update.** Edit the ConfigMap. The operator picks up the change and the
chaperone recompiles the document on each pod. No restart happens. If the new
document fails to compile, the previous version keeps serving, and the policy
reports `Programmed=False` with reason `CompileFailed`. Fix the document and
apply it again.

A policy that is attached for the first time behaves differently. If its first
document fails to compile, the route answers `503` with
`X-Ghost-Route-VCL: unprogrammed`. Detach the policy, or fix the document.

**Remove.** Delete the policy:

```bash
kubectl delete vvp api-vcl
```

The slot is released, but it is not reused straight away. It is held for at
least 60 seconds, and until no running pod still reports the old document. Until
then, requests that still carry the slot get a `503`. Another policy gets the
slot after that.

## Operations

**Metrics.** The chaperone exports `chaperone_route_vcl_compiles_total`,
`chaperone_route_vcl_compile_duration_seconds`, `chaperone_route_vcl_slots` and
`chaperone_route_vcl_master_reloads_total`. The operator exports
`varnish_gateway_route_vcl_slots`, `varnish_gateway_route_vcl_unassigned` and
`varnish_gateway_route_vcl_policies`. Route VCL loads are not counted in
`chaperone_vcl_reloads_total`. Jumps per second are not exported. To see dispatch,
run `varnishlog -q 'VCL_use ~ via'`. See
[Observability](../operations/observability.md#route-vcl-metrics).

**Dashboard.** When route VCL is on, the chaperone's built-in dashboard
(port 9000) has a **Route VCL** section. It shows the pool size, the active
master VCL, and one row per populated slot with its policy, route, state and
compiler error.

**Compile throttle.** Compiles are rate-limited by a token bucket.
`ROUTE_VCL_COMPILE_RATE` sets the rate (default `4` per second), and
`ROUTE_VCL_COMPILE_BURST` sets the bucket size (default `8`). Set the rate to
`0` to turn the throttle off. Only route VCL is throttled.

**Pool exhaustion.** When every slot is taken, a new policy gets no slot. It
reports `Programmed=False` with reason `NoSlotAvailable`. Its route keeps
running the gateway VCL. The operator logs a warning once, and
`varnish_gateway_route_vcl_unassigned` counts the policy. It gets a slot when a
slot is freed or the pool grows. Shrinking the pool below an in-use slot moves
that policy to a free slot, if there is one. Otherwise it waits with reason
`SlotOutsidePool`.

**ConfigMap size guard.** All route VCLs of a Gateway are stored in one
ConfigMap, `{gateway}-route-vcl`. If adding a document would push it past
900 KiB, that policy gets no slot. It reports `Programmed=False` with reason
`NoSlotAvailable`, and the condition message names `ConfigMapFull` as the cause. Each document can be at most 256 KiB.

## Limitations

The full list is in [Route VCL: caveats and limits](../reference/route-vcl-caveats.md).
The five that matter most:

- **One jump per request.** A route VCL cannot jump to another route VCL, and
  shared logic belongs in the gateway VCL.
- **Restarts stay where they were issued.** A restart in the master does not run
  the route VCL.
- **ESI fragments run the master VCL.** They never run the route VCL of the parent.
- **The cache is shared.** Route VCL can read, poison or ban other routes' objects.
- **The gateway VCL is composed into every route VCL.** A change to it recompiles
  every route VCL on every pod.

## Status of this feature

Route VCL is in preview. The mechanism was measured on Varnish 9.0.0 on a
local machine. The gateway image ships Varnish 9.1.0, and the figures must be
measured again on 9.1.0 before they are final. Load testing and the Gateway API
conformance run with the feature on are still being completed.
