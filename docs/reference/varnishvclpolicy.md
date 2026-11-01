# VarnishVCLPolicy Reference

!!! warning "API preview, not yet wired to the data plane"
    You can create VarnishVCLPolicy resources, and the operator validates them
    and sets the `Accepted` condition. No route VCL is compiled or delivered
    to the gateway pods yet, so attaching a policy does not change how
    requests are handled. Fields and behaviour may change before the feature
    leaves preview.

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

## Status and Conditions

The operator writes one entry under `status.ancestors`, with the target HTTPRoute as `ancestorRef`.

| Condition      | Reason            | Meaning                                                                                                                                  |
| -------------- | ----------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| Accepted=True  | Accepted          | The policy is valid, its references resolve, and route VCL is enabled for at least one parent Gateway of the route. The message lists any parent Gateways where it is disabled |
| Accepted=False | Invalid           | Bad spec: wrong `targetRef` group or kind, `sectionName` set, empty ConfigMap name, or a VCL document larger than 256 KiB                     |
| Accepted=False | TargetNotFound    | The HTTPRoute does not exist in the policy's namespace. Rechecked every 10s                                                                  |
| Accepted=False | ConfigMapNotFound | The ConfigMap, or the key in it, does not exist. Rechecked every 10s                                                                         |
| Accepted=False | Disabled          | None of the route's parent Gateways belongs to a GatewayClass with `routeVCL.enabled: true`. The message names each Gateway and GatewayClass |
| Accepted=False | Conflicted        | Another VarnishVCLPolicy already targets the same HTTPRoute                                                                              |

Conflict resolution: if two policies target the same HTTPRoute, the older one (by creation timestamp, then name) wins. When the winner is deleted or retargeted, the other policy is promoted.

A `Programmed` condition is reserved for when the VCL has compiled and loaded on every gateway pod. It is not set yet. Slot assignment and per-pod compile results will be added to the status later.
