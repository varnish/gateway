# VarnishCacheInvalidation Reference

## Overview

`VarnishCacheInvalidation` is a one-shot Kubernetes resource that purges or bans cached
objects across every chaperone pod serving a given Gateway. Create the resource; each
chaperone observes it via a watch, executes the invalidation against its local Varnish,
and writes its outcome back to `status.podResults`. The operator garbage-collects the
resource once it reaches a terminal phase and its TTL expires.

**API group:** `gateway.varnish-software.com/v1alpha1`
**Kind:** `VarnishCacheInvalidation`
**Short name:** `vcinv`
**Scope:** Namespaced

## Purge vs. Ban

|              | Purge                           | Ban                                         |
| ------------ | ------------------------------- | ------------------------------------------- |
| Matching     | Exact URL (Host + path)         | Regex pattern against URL                   |
| Operation    | Immediate removal of one object | Ban lurker sweeps objects in the background |
| Best for     | Known, specific URLs            | Bulk invalidation by pattern                |
| VCL handling | `return(purge)` in `vcl_recv`   | `std.ban()` with lurker-friendly expression |

Ghost installs ban-lurker friendly headers (`obj.http.x-cache-host`, `obj.http.x-cache-url`) on
every cached object and strips them in `vcl_deliver`, so ban expressions match efficiently.

## Spec

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: VarnishCacheInvalidation
metadata:
  name: purge-user-pages
  namespace: default
spec:
  gatewayRef:
    name: my-gateway
    # namespace: default    # defaults to the VarnishCacheInvalidation's namespace
  type: Purge # Purge | Ban
  hostname: api.example.com
  paths:
    - /api/users/123
    - /api/users/456
  ttl: 1h # optional, default 1h
```

### Fields

| Field                  | Type                  | Required         | Description                                          |
| ---------------------- | --------------------- | ---------------- | ---------------------------------------------------- |
| `gatewayRef.name`      | string                | yes              | Name of the target Gateway                           |
| `gatewayRef.namespace` | string                | no               | Defaults to the VarnishCacheInvalidation's namespace |
| `type`                 | enum (`Purge`, `Ban`) | yes              | Invalidation method                                  |
| `hostname`             | string                | yes              | `Host` header used for the invalidation request      |
| `paths`                | []string              | yes (MinItems=1) | For `Purge`: exact paths. For `Ban`: regex patterns  |
| `ttl`                  | Duration              | no               | Retention after completion. Default `1h`             |

### Paths semantics

- **Purge** — each path is the exact URL that was cached (same query string, same
  casing). The chaperone issues `PURGE <path>` with `Host: <hostname>` to local Varnish.
- **Ban** — each path is a regex matched against cached URLs. Use anchored patterns
  where possible (`^/api/v1/users/.*`). The chaperone issues `BAN <pattern>` with
  `Host: <hostname>` which gets processed by `internal/vcl/preamble.vcl`.

## Status

```yaml
status:
  phase: Complete
  completedAt: "2026-04-13T10:15:32Z"
  podResults:
    - podName: my-gateway-7c8f9-abc12
      success: true
      completedAt: "2026-04-13T10:15:31Z"
      pathResults:
        - path: /api/users/123
          success: true
        - path: /api/users/456
          success: false
          message: "404 Not in cache"
```

### Phase lifecycle

| Phase        | Meaning                                             |
| ------------ | --------------------------------------------------- |
| `Pending`    | Resource created, no pod has reported yet           |
| `InProgress` | At least one pod has reported, but not all          |
| `Complete`   | Every chaperone pod reported success for every path |
| `Failed`     | At least one pod reported a failure for any path    |

Transitions are monotonic: once `Complete` or `Failed`, the phase does not change.
`completedAt` is set when the final pod reports.

### Per-pod results

Each chaperone pod writes one `PodResult` entry keyed by pod name. `success` is `true`
only if every path on that pod succeeded. `pathResults` records the per-path outcome so
partial failures can be diagnosed without consulting pod logs.

## Garbage Collection

The operator runs a leader-elected GC loop every 5 minutes that deletes resources
which are:

1. In `Complete` or `Failed` phase, **and**
2. Whose `status.completedAt + spec.ttl` is in the past (default TTL: `1h`).

Resources stuck in `Pending` or `InProgress` are never GC'd — investigate the target
Gateway's pods if a VCI does not progress.

## Printer columns

`kubectl get vcinv` shows:

```
NAME                TYPE    HOSTNAME           STATUS     AGE
purge-user-pages    Purge   api.example.com    Complete   42s
```

## RBAC

The operator needs `get`/`list`/`watch`/`update`/`delete` on
`varnishcacheinvalidations` and `varnishcacheinvalidations/status`. The chaperone
needs `get`/`list`/`watch` on the resource and `update` on its status subresource.
Both are granted by the standard Helm chart and `deploy/` manifests.

## Examples

### Purge a handful of URLs

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: VarnishCacheInvalidation
metadata:
  name: purge-product-42
spec:
  gatewayRef:
    name: public-gateway
  type: Purge
  hostname: www.example.com
  paths:
    - /products/42
    - /products/42/reviews
    - /api/products/42
```

### Ban everything under a path prefix

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: VarnishCacheInvalidation
metadata:
  name: ban-api-v1
spec:
  gatewayRef:
    name: public-gateway
  type: Ban
  hostname: api.example.com
  paths:
    - "^/v1/.*"
  ttl: 15m
```

### Trigger from kubectl

```bash
# Apply and watch progress
kubectl apply -f invalidation.yaml
kubectl get vcinv purge-product-42 -w

# Inspect per-pod results
kubectl get vcinv purge-product-42 -o yaml | yq '.status.podResults'
```

## Related

- [Cache Invalidation guide](../guides/cache-invalidation.md)
- [VarnishCachePolicy reference](varnishcachepolicy.md)
- [Caching model](../concepts/caching-model.md)
