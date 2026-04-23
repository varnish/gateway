# Cache Invalidation

## Overview

Varnish Gateway exposes cache invalidation through the `VarnishCacheInvalidation` CRD
(short name `vcinv`). Creating one fans the invalidation out to every chaperone pod
serving the target Gateway; each pod executes it against its local Varnish and reports
back into `status.podResults`.

Two methods are supported:

- **Purge** — removes one exact URL.
- **Ban** — regex pattern against cached URLs, swept lazily by Varnish's ban lurker.

For the mechanics (why PURGE is O(1), how the lurker uses `x-cache-host` /
`x-cache-url`, why caching is opt-in), see the
[caching model](../concepts/caching-model.md). For the full field reference, see
[reference/varnishcacheinvalidation.md](../reference/varnishcacheinvalidation.md).

## When to use which

| Scenario                                        | Method |
| ----------------------------------------------- | ------ |
| A single article was edited                     | Purge  |
| A product page and its two related URLs changed | Purge  |
| Every object under `/v1/` is now stale          | Ban    |
| All cached responses for one tenant must go     | Ban    |

Prefer Purge when you can enumerate the URLs — it is O(1) per path and takes effect
immediately. Ban is cheap to submit but pays a background cost as the lurker sweeps,
and banned objects may live in memory for a short window after the ban is accepted.

## Purging specific URLs

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: VarnishCacheInvalidation
metadata:
  name: purge-product-42
  namespace: default
spec:
  gatewayRef:
    name: public-gateway
  type: Purge
  hostname: www.example.com
  paths:
    - /products/42
    - /products/42/reviews
```

Each `paths` entry is the exact URL that was cached — same query string, same casing.
Varnish returns success whether the object was in cache or not, so a Purge for a URL
that was never cached is a no-op, not a failure.

## Banning by pattern

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: VarnishCacheInvalidation
metadata:
  name: ban-v1-api
spec:
  gatewayRef:
    name: public-gateway
  type: Ban
  hostname: api.example.com
  paths:
    - "^/v1/.*"
```

Paths are regexes matched against the cached URL. Anchor them (`^...`) to avoid
matching unrelated paths — `/v1` without an anchor also matches `/api/v1abc`.

## Lifecycle

| Phase        | Meaning                                         |
| ------------ | ----------------------------------------------- |
| `Pending`    | Created; no chaperone has reported yet          |
| `InProgress` | Some pods have reported, but not all            |
| `Complete`   | Every pod reported success for every path       |
| `Failed`     | At least one pod reported a failure on any path |

The phase is monotonic: once `Complete` or `Failed`, it does not change. Each chaperone
writes a `PodResult` with a per-path breakdown, so a partial failure can be diagnosed
without reading pod logs.

```bash
kubectl apply -f invalidation.yaml
kubectl get vcinv purge-product-42 -w
kubectl get vcinv purge-product-42 -o yaml | yq '.status.podResults'
```

## Retention and GC

Completed and failed resources stay around for `spec.ttl` (default `1h`) for
inspection, then the operator's GC loop (every 5 minutes) deletes them. Lower the TTL
for high-volume invalidation where you don't need the audit trail:

```yaml
spec:
  ttl: 5m
```

Resources stuck in `Pending` or `InProgress` are **never** GC'd. If a VCI does not
progress, inspect the chaperone pods of the target Gateway — a pod is failing to
report.

## Caveats

- **Uncached routes look like successful purges.** Without a `VarnishCachePolicy`, a
  route runs in pass mode (see the [caching model](../concepts/caching-model.md)) and
  nothing is cached to invalidate. The VCI will still report `Complete`. If you
  expected something in cache, check that a VCP targets the route.
- **Hostname must match the cached Host header.** If the object was cached with
  `Host: www.example.com` and you purge with `hostname: example.com`, nothing happens
  — and you still get a success result.
- **Bans are asynchronous.** A `Complete` VCI means every chaperone accepted the ban,
  not that the lurker has finished sweeping. Under heavy cache pressure this can take
  seconds to minutes.

## Troubleshooting

- **Phase stuck at `Pending`** — no chaperone reported. Verify `gatewayRef` points at
  an existing Gateway in the right namespace and its pods are running.
- **Per-path failure `PURGE returned HTTP 405` (or similar)** — the VCL preamble isn't
  handling the method. This should not happen with a standard gateway install; it
  suggests user VCL has intercepted `vcl_recv` before ghost's postamble can run.
- **Ban never seems to take effect** — verify the regex with a small anchored sample
  first, and remember the lurker sweeps asynchronously.

## Related

- [VarnishCacheInvalidation reference](../reference/varnishcacheinvalidation.md)
- [Caching model](../concepts/caching-model.md)
- [VarnishCachePolicy reference](../reference/varnishcachepolicy.md)
