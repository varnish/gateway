# VarnishVCLPolicy Reference

!!! warning "API preview: end-to-end wiring complete, not yet load-tested or conformance-verified with the feature on"
    An accepted policy gets a slot on each parent Gateway, its VCL is compiled
    and loaded on every gateway pod, and requests matched by the route run it.
    The feature has not yet been load-tested, and the Gateway API conformance
    suite has not yet been run with it turned on. Fields and behaviour may
    change before the feature leaves preview.

## Overview

**VarnishVCLPolicy (VVP)** attaches a VCL document to an HTTPRoute. Requests matched by that route will run the attached VCL, composed with the gateway's own VCL. Gateway-wide VCL (`GatewayClassParameters.spec.userVCLConfigMapRef`) stays in force. A route owner cannot opt out of it.

The feature is off by default. It has to be turned on per GatewayClass with [`spec.routeVCL.enabled`](gatewayclassparameters.md#specroutevcl).

## Trust statement

**Attaching VCL to an HTTPRoute is equivalent to granting code execution inside the gateway pod.** Submitted VCL is not inspected before it reaches varnishd. A route VCL can:

- **Read the gateway's TLS private key.** It is mounted from a Secret into the container, and `std.fileread` can reach it.
- **Read the chaperone's ServiceAccount token.** The pod's ServiceAccount can watch EndpointSlices and read and write VarnishCacheInvalidation resources across the cluster.
- **Possibly read the varnishadm shared secret**, depending on which user the container runs as.
- **Flush every other tenant's cached objects** with `std.ban()`, which applies to the whole cache.
- **Declare arbitrary backends**, which gives it all the network access the gateway pod has.

This is a much larger grant than HTTPRoute carries today. With `routeVCL.enabled: true` on a GatewayClass, anyone who can create an HTTPRoute in a namespace routed by that class gets this grant. There is no per-namespace allowlist. Enable the feature only where the people who create HTTPRoutes are trusted with the gateway itself.

## Spec

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: api-vcl
  namespace: default
data:
  route.vcl: |
    sub vcl_recv {
      set req.http.X-Api-Version = "2";
    }
---
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

### Fields

| Field                   | Type   | Default     | Description                                                                 |
| ----------------------- | ------ | ----------- | --------------------------------------------------------------------------- |
| `targetRef.group`       | string | required    | Must be `gateway.networking.k8s.io`                                          |
| `targetRef.kind`        | string | required    | Must be `HTTPRoute`                                                          |
| `targetRef.name`        | string | required    | Name of an HTTPRoute in the policy's namespace                               |
| `targetRef.sectionName` | string | —           | Not supported. A policy that sets it is rejected (`Invalid`)                 |
| `vclConfigMapRef.name`  | string | required    | ConfigMap in the policy's own namespace. Cross-namespace references are not possible |
| `vclConfigMapRef.key`   | string | `route.vcl` | Key in the ConfigMap's `data` that holds the VCL document                    |

The VCL document can be at most 256 KiB. All route VCLs of a gateway are delivered in a single ConfigMap, and the API server caps a ConfigMap at 1 MiB.

Short name: `kubectl get vvp`.

## Slots

Each Gateway with route VCL enabled has a fixed pool of VCL slots ([`routeVCL.slotPoolSize`](gatewayclassparameters.md#specroutevcl), default 64). An accepted policy occupies one slot on every parent Gateway of its route that has the feature on. The slot number is what ghost tags a matched request with, and what the master VCL dispatches on.

- **Slots are stable.** A policy keeps its slot for as long as it stays accepted and attached, including when its VCL document changes. Only the document in the slot is replaced (and recompiled on each pod).
- **Assignment order.** A policy without a slot gets the lowest free slot. When several policies need one in the same pass, the oldest (creation timestamp, then namespace/name) goes first. Slots that are already held are honoured before any new ones are handed out.
- **Release.** A policy that is deleted, retargeted, no longer accepted, or whose route leaves the Gateway frees its slot. The slot can go to another policy in the same pass.
- **Pool exhaustion.** When every slot is taken, the policy gets no slot on that Gateway. It reports `Programmed=False`, reason `NoSlotAvailable`, and requests on its route keep running the gateway's master VCL. The operator logs a warning once and the `varnish_gateway_route_vcl_unassigned` metric counts it. It gets a slot as soon as one is freed or the pool grows.
- **Pool shrink.** If `slotPoolSize` is reduced below a slot that is in use, that policy loses its slot (other policies keep their numbers). It is given a free slot inside the smaller pool if there is one, otherwise it waits with reason `SlotOutsidePool`.
- **Size limit.** All route VCLs of a Gateway travel in one ConfigMap (`{gateway}-route-vcl`). If adding a document would push it past 900 KiB, that policy gets no slot (reason `ConfigMapFull`).

The operator-side metrics `varnish_gateway_route_vcl_slots{state="used|free"}` and `varnish_gateway_route_vcl_unassigned` show pool usage per Gateway.

## Status and Conditions

The operator writes one entry under `status.ancestors`, with the target HTTPRoute as `ancestorRef`. It holds two conditions, `Accepted` and `Programmed`.

### Accepted

| Condition      | Reason            | Meaning                                                                                                                                  |
| -------------- | ----------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| Accepted=True  | Accepted          | The policy is valid, its references resolve, and route VCL is enabled for at least one parent Gateway of the route. The message lists any parent Gateways where it is disabled |
| Accepted=False | Invalid           | Bad spec: wrong `targetRef` group or kind, `sectionName` set, empty ConfigMap name, or a VCL document larger than 256 KiB                     |
| Accepted=False | TargetNotFound    | The HTTPRoute does not exist in the policy's namespace. Rechecked every 10s                                                                  |
| Accepted=False | ConfigMapNotFound | The ConfigMap, or the key in it, does not exist. Rechecked every 10s                                                                         |
| Accepted=False | Disabled          | None of the route's parent Gateways belongs to a GatewayClass with `routeVCL.enabled: true`. The message names each Gateway and GatewayClass |
| Accepted=False | Conflicted        | Another VarnishVCLPolicy already targets the same HTTPRoute                                                                              |

Conflict resolution: if two policies target the same HTTPRoute, the older one (by creation timestamp, then name) wins. When the winner is deleted or retargeted, the other policy is promoted.

### Programmed

`Programmed` is derived for every parent Gateway that has route VCL enabled, from that Gateway's slot assignment and from the results its pods report in `status.podResults`. A pod counts as "live" when it belongs to the Gateway's Deployment and is neither terminating nor finished.

| Condition          | Reason          | Meaning                                                                                                                       |
| ------------------ | --------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| Programmed=True    | Programmed      | On every Gateway, every live pod reports the current document compiled and its slot label pointing at it                       |
| Programmed=False   | CompileFailed   | A live pod reports that the current document failed to compile or label. The message carries that pod's compiler output        |
| Programmed=False   | NoSlotAvailable | A Gateway could not give the policy a slot. The message gives the reason: `NoSlotAvailable`, `SlotOutsidePool` or `ConfigMapFull` |
| Programmed=False   | Pending         | The policy has a slot, but some live pods have not reported the current document yet (still compiling, just started, or a Gateway has not picked the policy up yet while another has) |
| Programmed=Unknown | NotProgrammed   | No Gateway has picked the policy up yet                                                                                         |
| Programmed=False   | NotAccepted     | The policy is not `Accepted`                                                                                                   |

With several parent Gateways, the worst state wins, in the order CompileFailed, NoSlotAvailable, Pending. The message names the Gateway (and pod) each part refers to.

A compile failure leaves the slot's previous VCL in place on that pod. For a new policy that is a placeholder that answers 503 for the route's requests, so fix the VCL or detach the policy.

### status.slots

One entry per parent Gateway on which the policy holds a slot:

```yaml
status:
  slots:
    - gateway: default/my-gateway   # <namespace>/<name>
      slot: 3
```

### status.podResults

Written by each gateway pod's chaperone after every attempt to compile and label the policy's VCL. One entry per pod:

| Field        | Description                                                                                       |
| ------------ | ------------------------------------------------------------------------------------------------- |
| `podName`    | The gateway pod                                                                                   |
| `slot`       | The slot the pod loaded the VCL into (`-1` if the slot in the manifest was not a number)          |
| `hash`       | SHA-256 of the composed route VCL document the result refers to                                   |
| `programmed` | `true` when the document compiled and the slot label points at it                                 |
| `message`    | Compiler output, or why the entry was skipped, when `programmed` is `false`                       |
| `lastUpdate` | When the pod last changed the entry                                                               |

An entry whose `hash` differs from the current document's is stale: the pod has not caught up yet. The operator removes entries for pods that no longer exist. It keeps entries for live pods even when they are stale, because a pod only rewrites its entry when its result changes.
