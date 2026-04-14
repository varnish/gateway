# Caching Model

Varnish is a cache, but Varnish Gateway is first a Gateway API implementation.
That shapes the defaults: routing is the base contract, caching is opt-in
per route.

## Default behavior: no caching

Without any `VarnishCachePolicy` attached, the Varnish Gateway sets `pass` on every
request and the gateway behaves as a plain reverse proxy — no cached
objects, no request coalescing, no stale serving.

The motivation is that blue/green deployments, canary rollouts, and weighted
traffic splitting all assume the proxy faithfully forwards every request.
Turning caching on by default would silently break those patterns. Making it
explicit also makes it auditable: you can tell whether a route is cached by
looking for a `VarnishCachePolicy` that targets it, not by inspecting
generated VCL.

## Opting in: VarnishCachePolicy

A `VarnishCachePolicy` (VCP) attaches to a `Gateway`, an `HTTPRoute`, or a
named rule within an HTTPRoute. It describes the cache behavior for the
targeted scope:

- **TTL** — `defaultTTL` (origin Cache-Control wins if present) or
  `forcedTTL` (origin headers ignored).
- **Stale serving** — `grace` (stale-while-revalidate) and `keep`
  (stale-if-error).
- **Request coalescing** — whether concurrent requests for the same
  uncached object collapse into a single origin fetch.
- **Cache key** — which request headers and query parameters participate.
- **Bypass** — request conditions (e.g., `Authorization` header, session
  cookie) that force pass mode on a per-request basis.

VCP follows the Gateway API _Inherited Policy_ pattern: Gateway-level
defaults can be overridden by HTTPRoute-level policies, which in turn can
be overridden at the rule level. Override is complete replacement, not
field merging — the winning VCP's spec is used as-is, and unset fields
(e.g., `grace`, `keep`, `bypass`, `cacheKey`) fall back to their defaults
rather than inheriting from a less-specific VCP.

See [reference/varnishcachepolicy.md](../reference/varnishcachepolicy.md)
for the full field reference and worked examples.

## How ghost communicates cache decisions to VCL

Ghost hooks into several VCL subroutines:

- `vcl_init` — construct the router (`ghost.init`, `new router = ghost.ghost_backend()`).
- `vcl_recv` — the main routing decision (`router.recv()`), which picks
  the backend and sets the per-request cache policy.
- Backend fetch — the router acts as a VCL backend; ghost's
  `VclBackend` implementation is invoked by Varnish when the request
  proceeds to the origin, and can also produce synthetic responses
  (e.g., 404 for unknown vhosts) without going through `vcl_synth`.
- `vcl_deliver` — `ghost.deliver()` runs cleanup on the response path.

The routing decision in `vcl_recv` makes per-request choices (pass vs
cache, effective TTL, cache key adjustments) that need to reach later
subroutines — notably `vcl_backend_response`, where TTL is applied to
the response object. VCL subroutines share state only through the
request/bereq headers and a few well-known variables, so ghost uses
request headers as the bridge:

| Header                    | Purpose                                                                                                                                |
| ------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `X-Ghost-Pass`            | Set when ghost decides the request should bypass cache. The VCL postamble checks this and calls `return(pass)` after user VCL has run. |
| `X-Ghost-Default-TTL`     | Suggested TTL used only if origin Cache-Control is absent.                                                                             |
| `X-Ghost-Forced-TTL`      | TTL that overrides origin Cache-Control.                                                                                               |
| `X-Ghost-Grace`           | Grace period for stale-while-revalidate.                                                                                               |
| `X-Ghost-Keep`            | Keep period for stale-if-error.                                                                                                        |
| `X-Ghost-Cache-Key-Extra` | Additional data hashed into the cache key (e.g., serialized header/query selections).                                                  |

These are internal headers stripped before the response leaves Varnish.

Ghost signals the pass decision via the `X-Ghost-Pass` header rather
than issuing `return(pass)` itself. A `return(pass)` from the preamble
`vcl_recv` would terminate the subroutine immediately, and the user VCL
concatenated between the preamble and the postamble would never run. By
writing the decision into a header, ghost lets user VCL execute first;
the postamble's `vcl_recv` (appended last) then reads the header and
issues the actual `return(pass)`. A VMOD function also has no direct way
to emit a VCL control-flow directive, so the header is the natural
bridge.

## Cache key model

The base cache key is hostname plus URL, as in stock Varnish. A VCP's
`cacheKey` stanza extends this:

- **Headers** — listed request headers are appended to the hash, so
  `Accept-Language: en` and `Accept-Language: de` cache separately.
- **Query parameters** — either an allowlist (`include`) or a denylist
  (`exclude`). Everything not listed is excluded (for `include`) or
  included (for `exclude`). This is how tracking parameters like
  `utm_source`, `fbclid`, and `gclid` can be stripped from the key so they
  don't fragment the cache.

The two query-parameter modes are mutually exclusive; a VCP uses one or
the other, not both.

## Invalidation

There are two mechanisms available in Varnish, each with different performance and semantics:

### PURGE — exact-URL removal

Targets a single cached object by hostname + URL. The operation is O(1)
against the hash table and takes effect immediately. Use PURGE when you
know exactly which URL changed.

### BAN — regex pattern invalidation

Invalidates any cached object whose URL matches a regex. The ban is
evaluated against cached objects lazily by the **ban lurker**, a
background thread that walks the cache applying outstanding bans. This
keeps the request path fast but means "banned" objects may remain in
memory for an arbitrary time window after the ban is issued.

Ghost's VCL preamble cooperates with the lurker by writing two headers
onto each cached object's metadata in `vcl_backend_response`:

- `x-cache-host` — the request hostname
- `x-cache-url` — the request URL

These are "lurker-friendly" fields, meaning the lurker can evaluate bans
against them without having the corresponding request object in memory. They are
stripped from the client-visible response in `vcl_deliver`.

### VarnishCacheInvalidation CRD

Both PURGE and BAN are exposed through the cluster-scoped
`VarnishCacheInvalidation` CRD. It is a one-shot resource: you create
one, every chaperone in the target gateway executes it against its local
Varnish, the operator aggregates per-pod results into `status.podResults`,
and the operator garbage-collects completed resources after a TTL
(default 1h).

A single resource can carry multiple paths, which batches nicely — a
deployment that invalidates many URLs at once produces one CR rather than
hundreds.

See [reference/varnishcacheinvalidation.md](../reference/varnishcacheinvalidation.md)
and [guides/cache-invalidation.md](../guides/cache-invalidation.md)
for the full reference and usage.

## Caveats

### Caching interacts badly with traffic splitting

If a VCP is attached to a route with weighted backends, the first
response wins: subsequent requests serve the cached object regardless of
weight. The configured split becomes meaningless for cached paths.

### Set-Cookie is not cached by default

Varnish's default behavior is to not cache responses carrying
`Set-Cookie`. This applies under `defaultTTL` as well — the origin's
implicit "this response is per-user" signal wins. Under `forcedTTL`, the
`Set-Cookie` header is stripped and the response is cached anyway, which
is usually not what you want for anything but CDN-style asset caches.

### No VCL is generated for error handling

The gateway deliberately does not emit `vcl_synth` or
`vcl_backend_error`. These subroutines are commonly customized by users,
and a generated `return` statement there would silently skip user VCL.
Error handling is therefore an application concern — ghost returns
through normal VCL flow and user VCL can intercept.

## See also

- [VarnishCachePolicy reference](../reference/varnishcachepolicy.md)
- [VCL merging](vcl-merging.md)
- [Cache invalidation guide](../guides/cache-invalidation.md)
