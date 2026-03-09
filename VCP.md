# VarnishCachePolicy - Design Proposal

## Philosophy

Varnish Gateway ships with **caching disabled**. Every request passes straight through to the
backend — no caching, no request coalescing. This is the safe default for Kubernetes, where
blue/green deployments, canary rollouts, and traffic splitting all assume the proxy faithfully
forwards requests.

**VarnishCachePolicy (VCP)** is how you opt in. Attaching a VCP to an HTTPRoute says:
"I understand caching, and I want it here." No VCP, no caching — simple, explicit, auditable.

This mirrors how Kubernetes works elsewhere: NetworkPolicy (default-allow, opt into restriction),
PodDisruptionBudget (opt into availability guarantees), ResourceQuota (opt into limits). VCP
follows the same pattern: the powerful behavior exists, but you activate it deliberately.

## How It Works

### The Default: Pure Reverse Proxy

Without any VCP in the cluster, every route gets `return(pass)` in `vcl_recv`. Varnish acts as
a transparent reverse proxy:

- No cache lookups
- No cache storage
- No request coalescing
- Each request goes independently to the backend

This is implemented by having the ghost VMOD return `pass` for any route that has no cache
policy attached.

### Attaching a VCP: Enabling Caching

When you create a VCP targeting an HTTPRoute, that route switches from `pass` mode to normal
Varnish cache mode:

1. Requests go through the cache lookup
2. Cache misses fetch from the backend
3. Responses are stored according to the policy
4. Subsequent requests for the same object are served from cache
5. Request coalescing kicks in (configurable)

### Hierarchy and Inheritance

VCP is an **Inherited Policy** per Gateway API conventions:

```
Gateway                  ← VCP provides defaults for all routes through this gateway
  └── HTTPRoute          ← VCP overrides gateway defaults for all rules in this route
       └── Rule (named)  ← VCP overrides route defaults for this specific rule
```

- **No VCP anywhere**: caching disabled (pass mode)
- **VCP on Gateway only**: all routes through that gateway use the gateway policy
- **VCP on HTTPRoute**: all rules in that route use this policy, ignoring gateway defaults
- **VCP on a named rule**: that rule uses its own policy; other rules in the route fall back
  to the HTTPRoute-level or Gateway-level VCP
- Most specific wins (rule > route > gateway)

The override is **complete replacement**, not field-level merging. If you attach a VCP to an
HTTPRoute, it doesn't inherit the gateway VCP's `grace` or `cacheKey` settings — it uses
exactly what you specified. This is predictable and easy to reason about.

## Spec Reference

```yaml
apiVersion: gateway.varnish.org/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: my-cache-policy
  namespace: default
spec:
  # Target: Gateway or HTTPRoute
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute          # or Gateway
    name: my-route

  # When targeting a specific rule within an HTTPRoute:
  # sectionName: static-assets

  # TTL mode — choose ONE of defaultTTL or forcedTTL (mutually exclusive).
  #
  # defaultTTL: used when the origin does NOT send Cache-Control headers.
  # Origin Cache-Control takes precedence. This is the safe, HTTP-compliant option.
  defaultTTL: 5m
  #
  # forcedTTL: forced TTL, ignoring origin Cache-Control entirely.
  # Use when the origin misbehaves or you need operator-level control.
  # forcedTTL: 1h

  # Serve stale content while asynchronously revalidating in the background.
  # Equivalent to stale-while-revalidate in HTTP semantics.
  # Default: 0 (disabled)
  grace: 30s

  # How long to keep stale objects for serving when all backends are sick.
  # Equivalent to stale-if-error in HTTP semantics.
  # Default: 0 (disabled)
  keep: 24h

  # Enable collapsed forwarding: when multiple clients request the same
  # uncached object simultaneously, only one request goes to the backend.
  # Others wait and share the response.
  # Default: true
  requestCoalescing: true

  # Customize what makes a cache entry unique.
  cacheKey:
    # Include these request headers in the cache key.
    # Similar to Vary, but controlled by the operator, not the origin.
    headers:
      - Accept-Language
      - X-User-Tier

    # Control which query parameters are part of the cache key.
    queryParameters:
      # Allowlist mode: only these params matter for caching.
      include:
        - page
        - filter
      # OR denylist mode (mutually exclusive with include):
      # exclude:
      #   - utm_source
      #   - utm_medium
      #   - fbclid

  # Conditions under which caching is bypassed even when this policy is active.
  # Matching requests get pass-through behavior (no cache lookup, no storage).
  bypass:
    headers:
      - name: Authorization     # Any request with this header bypasses cache
      - name: Cookie
        valueRegex: "session_id|admin_token"  # Only bypass for specific cookies
```

### Field Details

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `targetRef` | PolicyTargetReference | required | Gateway, HTTPRoute, or HTTPRoute rule to attach to |
| `targetRef.sectionName` | string | optional | Name of a specific rule within the targeted HTTPRoute |
| `defaultTTL` | Duration | required* | TTL when origin sends no Cache-Control. Mutually exclusive with `forcedTTL` |
| `forcedTTL` | Duration | required* | Forced TTL, ignores origin Cache-Control. Mutually exclusive with `defaultTTL` |
| `grace` | Duration | `0` | Serve stale while revalidating (see note on grace/keep semantics below) |
| `keep` | Duration | `0` | Serve stale when backend is down (see note on grace/keep semantics below) |
| `requestCoalescing` | bool | `true` | Collapsed forwarding for concurrent requests |
| `cacheKey.headers` | []string | `[]` | Request headers to include in cache key |
| `cacheKey.queryParameters.include` | []string | all | Allowlist of query params in cache key (exact match) |
| `cacheKey.queryParameters.exclude` | []string | none | Denylist of query params from cache key (exact match) |
| `bypass.headers` | []HeaderCondition | `[]` | Headers that trigger cache bypass |

*Exactly one of `defaultTTL` or `forcedTTL` must be set. Validation rejects specs with both or neither.

**Note on grace/keep semantics:** `grace` and `keep` are always operator-set values. Varnish
does not natively parse `stale-while-revalidate` or `stale-if-error` from Cache-Control
headers, so these fields are not "defaults" — they are the authoritative values. We use the
Varnish names (`grace`/`keep`) rather than `defaultGrace`/`defaultKeep` because there is no
origin-wins path for these fields. If a future version adds parsing of `stale-while-revalidate`
and `stale-if-error` from origin headers, the naming should be revisited to match the
`defaultTTL`/`forcedTTL` symmetry.

## Examples

### Example 1: Cache a Static Assets Route

Two HTTPRoutes serve the same hostname. Static assets get cached; the API does not.

```yaml
# Route 1: Static assets
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: static-assets
  namespace: default
spec:
  parentRefs:
    - name: my-gateway
  hostnames:
    - www.example.com
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /static
      backendRefs:
        - name: cdn-origin
          port: 80
---
# Route 2: API (no VCP attached — pure proxy)
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: api
  namespace: default
spec:
  parentRefs:
    - name: my-gateway
  hostnames:
    - www.example.com
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /api
      backendRefs:
        - name: api-server
          port: 8080
---
# Cache policy — only targets the static route
apiVersion: gateway.varnish.org/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: cache-static
  namespace: default
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: static-assets
  defaultTTL: 1h
  grace: 5m
```

**Result:**
- `GET /static/logo.png` → cached for 1h, stale-while-revalidate for 5m
- `GET /api/users` → pass-through, no caching

### Example 2: Gateway-Wide Defaults with Per-Route Override

```yaml
# Gateway-level policy: conservative caching for everything
apiVersion: gateway.varnish.org/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: gateway-defaults
  namespace: default
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: Gateway
    name: my-gateway
  defaultTTL: 60s
  grace: 10s
  bypass:
    headers:
      - name: Authorization
      - name: Cookie
---
# Product catalog gets aggressive caching (overrides gateway defaults entirely)
apiVersion: gateway.varnish.org/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: cache-catalog
  namespace: default
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: product-catalog
  defaultTTL: 30m
  grace: 1h
  keep: 24h
  cacheKey:
    headers:
      - Accept-Language
    queryParameters:
      include:
        - page
        - category
```

**Result:**
- Routes through `my-gateway` without their own VCP: 60s TTL, bypass on Auth/Cookie
- `product-catalog` route: 30m TTL, 1h grace, no bypass rules (the gateway-level
  bypass for Authorization/Cookie does NOT apply — the route VCP is a full replacement)

### Example 3: Multi-Language Site with UTM Stripping

```yaml
apiVersion: gateway.varnish.org/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: cache-marketing-pages
  namespace: default
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: marketing-site
  defaultTTL: 15m
  grace: 1h
  keep: 6h
  cacheKey:
    headers:
      - Accept-Language
    queryParameters:
      exclude:
        - utm_source
        - utm_medium
        - utm_campaign
        - utm_content
        - utm_term
        - fbclid
        - gclid
```

**Result:**
- `/pricing?utm_source=google` and `/pricing?utm_source=twitter` share the same cache entry
- `/pricing` with `Accept-Language: en` and `Accept-Language: fr` are cached separately
- If all backends go down, stale content is served for up to 6 hours

### Example 4: Caching with Selective Bypass

```yaml
apiVersion: gateway.varnish.org/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: cache-app
  namespace: default
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: web-app
  defaultTTL: 5m
  grace: 30s
  requestCoalescing: true
  bypass:
    headers:
      - name: Authorization
      - name: Cookie
        valueRegex: "session_id|auth_token"
```

**Result:**
- Anonymous users get cached responses (fast)
- Requests with `Authorization` header always hit the backend
- Requests with cookies containing `session_id` or `auth_token` bypass cache
- Requests with innocuous cookies (analytics, consent) are still cached

## Interaction with Traffic Splitting

Caching and weighted traffic splitting (canary/blue-green) are fundamentally in tension.
Consider:

```yaml
rules:
  - matches:
      - path:
          type: PathPrefix
          value: /api
    backendRefs:
      - name: api-v1
        weight: 90
      - name: api-v2
        weight: 10
```

If this route has a VCP attached, the first request to `/api/foo` gets cached (from whichever
backend handled it). All subsequent requests serve that cached response — the 90/10 split
is meaningless for cached paths.

**Recommendation:** Don't attach a VCP to routes with active traffic splitting. The operator
should emit a **Warning** condition on the VCP status when it detects this, but not reject
the policy — the user might know what they're doing (e.g., the backends return identical
content and the split is for backend load testing).

## Interaction with Origin Cache-Control

The two TTL modes have fundamentally different relationships with origin headers:

### `defaultTTL` — Cooperative (origin wins)

| Origin sends | Result |
|---|---|
| No Cache-Control | TTL = 5m (defaultTTL applies) |
| `Cache-Control: max-age=60` | TTL = 60s (origin wins) |
| `Cache-Control: s-maxage=120` | TTL = 120s (s-maxage wins) |
| `Cache-Control: no-store` | Not cached (origin wins) |
| `Cache-Control: private` | Not cached (origin wins) |
| `Set-Cookie` present | Not cached (Varnish default) |

Safe to attach broadly. Origin stays in control; `defaultTTL` is just a safety net for
responses that forgot to set headers.

### `forcedTTL` — Authoritative (operator wins)

| Origin sends | Result |
|---|---|
| No Cache-Control | TTL = 1h |
| `Cache-Control: max-age=60` | TTL = 1h (ignored) |
| `Cache-Control: no-store` | TTL = 1h (ignored) |
| `Cache-Control: private` | TTL = 1h (ignored) |
| `Set-Cookie` present | TTL = 1h (ignored, header stripped) |

Use when you know better than the origin — e.g., a legacy backend that sends `no-store` on
static assets, or a third-party service whose headers you can't control. The operator is
explicitly taking responsibility for caching correctness.

## Status and Conditions

VCP reports status following Gateway API conventions:

```yaml
status:
  ancestors:
    - ancestorRef:
        group: gateway.networking.k8s.io
        kind: Gateway
        name: my-gateway
      controllerName: varnish-software.com/gateway
      conditions:
        - type: Accepted
          status: "True"
          reason: Accepted
          message: "Policy applied to 3 routes"
```

| Condition | Reason | Meaning |
|-----------|--------|---------|
| Accepted=True | Accepted | Policy is valid and active |
| Accepted=False | TargetNotFound | Referenced HTTPRoute/Gateway doesn't exist |
| Accepted=False | Invalid | Spec validation failed (e.g., include+exclude both set) |
| Accepted=False | Conflicted | Another VCP already targets the same route |
| Accepted=True | AcceptedWithWarning | Applied, but with caveats (e.g., traffic splitting detected) |

**Conflict resolution:** If two VCPs target the same HTTPRoute, oldest (by creation timestamp)
wins. The newer one gets `Conflicted`. This follows GEP-713 precedence rules.

## How It Flows Through the System

```
VarnishCachePolicy ──┐
                     ├──► Operator ──► routing.json (with cache_policy per route)
HTTPRoute ───────────┘         │
                               ▼
                          Chaperone ──► ghost.json (with cache_policy per route)
                               │
                               ▼
                          Ghost VMOD
                               │
                               ├── Route has cache_policy? → normal cache flow
                               │   (hash, lookup, fetch, store)
                               │
                               └── No cache_policy? → return(pass)
                                   (pure proxy, no caching)
```

### Changes to routing.json

Routes gain a `rule_name` field (from HTTPRouteRule.Name) and a `cache_policy` field when
a VCP is attached:

```json
{
  "hostname": "www.example.com",
  "path_match": {"type": "PathPrefix", "value": "/static"},
  "service": "cdn-origin",
  "namespace": "default",
  "port": 80,
  "weight": 100,
  "priority": 10700,
  "rule_index": 0,
  "rule_name": "static-assets",
  "cache_policy": {
    "forced_ttl_seconds": 86400,
    "grace_seconds": 0,
    "keep_seconds": 0,
    "request_coalescing": true
  }
}
```

A route using `defaultTTL` instead:

```json
{
  "cache_policy": {
    "default_ttl_seconds": 300,
    "grace_seconds": 30,
    "keep_seconds": 0,
    "request_coalescing": true,
    "cache_key": {
      "headers": ["Accept-Language"],
      "query_params_include": ["page", "filter"]
    },
    "bypass_headers": [
      {"name": "Authorization"},
      {"name": "Cookie", "value_regex": "session_id"}
    ]
  }
}
```

`forced_ttl_seconds` and `default_ttl_seconds` are mutually exclusive in the JSON — exactly one is
present. Routes without a VCP have no `cache_policy` field (null/absent). The ghost VMOD
treats this as "pass mode."

### Ghost VMOD Behavior

In `vcl_recv` (the `recv()` method):
- After route matching, if the matched route has no `cache_policy`, set
  `req.hash_always_miss = true` or signal pass mode.
- If it has a `cache_policy`, apply bypass rules (check request headers against bypass
  conditions). If bypass matches, signal pass.
- If `!request_coalescing`, the ghost VMOD sets `req.hash_ignore_busy` directly via
  the Varnish context so concurrent requests for the same uncached object each do
  their own backend fetch instead of waiting on the first one.

In `vcl_hash` (or via a new `hash()` method):
- If the route has `cache_key.headers`, add those request header values to the hash.
- If the route has `cache_key.queryParameters`, rewrite `req.url` to include only the
  specified query params before hashing.

In `vcl_backend_response`:
- If `beresp.ttl == 0` and the route has `default_ttl_seconds > 0`, set `beresp.ttl`.
- Set `beresp.grace` and `beresp.keep` from the policy.

## Design Decisions

### Per-Rule Targeting via `sectionName`

VCP supports targeting individual rules within an HTTPRoute using `sectionName`, which
references the rule's `name` field. This is the same mechanism Gateway API uses for targeting
individual listeners on a Gateway. GEP-995 (Named Route Rules) is Standard status — the
`name` field on HTTPRouteRule is stable and explicitly designed for policy attachment.

**Without `sectionName`** — targets the entire HTTPRoute (all rules):

```yaml
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: my-app
  defaultTTL: 5m
```

**With `sectionName`** — targets one named rule:

```yaml
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: my-app
    sectionName: static-assets
  defaultTTL: 1h
```

**Precedence** (most specific wins):
1. VCP targeting a specific rule (sectionName set)
2. VCP targeting the whole HTTPRoute (no sectionName)
3. VCP targeting the parent Gateway (inherited)

Rules without a name cannot be targeted individually. If a VCP references a `sectionName`
that doesn't match any rule name, the VCP gets `Accepted=False` with reason `TargetNotFound`.

#### Example: One HTTPRoute, Different Caching Per Rule

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: my-app
spec:
  parentRefs:
    - name: my-gateway
  hostnames:
    - www.example.com
  rules:
    - name: static-assets
      matches:
        - path: { type: PathPrefix, value: /static }
      backendRefs:
        - name: cdn-origin
          port: 80
    - name: api
      matches:
        - path: { type: PathPrefix, value: /api }
      backendRefs:
        - name: api-server
          port: 8080
    - name: pages
      matches:
        - path: { type: PathPrefix, value: / }
      backendRefs:
        - name: web-server
          port: 80
---
# Aggressive caching for static assets
apiVersion: gateway.varnish.org/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: cache-static
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: my-app
    sectionName: static-assets
  forcedTTL: 24h
---
# Short caching for pages
apiVersion: gateway.varnish.org/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: cache-pages
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: my-app
    sectionName: pages
  defaultTTL: 5m
  grace: 30s
  bypass:
    headers:
      - name: Cookie
        valueRegex: "session_id"
```

**Result:**
- `/static/*` → forced 24h TTL via `forcedTTL` (origin headers ignored)
- `/api/*` → no VCP, pass mode (no caching)
- `/*` → 5m default TTL, respects origin Cache-Control, bypasses for session cookies

This avoids splitting a natural "one hostname, three path prefixes" HTTPRoute into three
separate resources just for different cache behavior.

### Why Full Replacement, Not Merging?

When an HTTPRoute VCP overrides a Gateway VCP, it's a complete replacement. No field-level
inheritance. Reasons:

1. **Predictability**: you read one YAML and know exactly what caching does
2. **No surprises**: a gateway admin changing defaults won't silently alter route behavior
3. **Debugging**: "which fields came from where?" is never a question
4. Merging can be added later as an opt-in if there's demand

### Why Exactly One of `defaultTTL` or `forcedTTL`?

Making the user specify a TTL forces conscious decision-making. There's no safe universal
default — 60s might be too long for a stock ticker, too short for a product image.

The two fields encode different trust relationships:
- `defaultTTL`: "I trust my origins to set Cache-Control, but want a fallback."
- `forcedTTL`: "I don't trust my origins, or I need operator-level control."

They're mutually exclusive because the intent is unambiguous either way. There's no scenario
where you'd want both — if you're overriding, you're overriding.

### Why Keep Varnish Terminology (grace/keep)?

VarnishCachePolicy is a Varnish-specific CRD. Users choosing Varnish as their gateway likely
know Varnish concepts. We note the HTTP equivalents (stale-while-revalidate, stale-if-error)
in the CRD field descriptions, but the field names use Varnish terms because that's what
`beresp.grace` and `beresp.keep` are called in VCL.

## Possible Additions

These are **not** in v1. They're worth considering but not committed to.

### Cookie Stripping

The `bypass` mechanism is binary: if a cookie matches, the request skips cache entirely. This
is insufficient for real-world CMS workloads. WordPress, for example, sends cookies for
analytics, consent banners, security plugins (wordfence), and more — none of which affect
page content. The VCL approach is to strip harmless cookies before the cache lookup so the
request can still be cached.

A possible `cookies` field under `cacheKey`:

```yaml
cacheKey:
  cookies:
    # Allowlist mode: only these cookies affect cache identity.
    # All other cookies are stripped before lookup.
    include:
      - wordpress_logged_in_*
      - wp-settings-*
      - woocommerce_*
    # OR denylist mode (mutually exclusive with include):
    # exclude:
    #   - wfvt_*
    #   - wordfence_verifiedHuman
    #   - _ga
    #   - _gid
    #   - fbp
```

**Allowlist** (`include`): only these cookies are kept; everything else is stripped before
hashing. This is the safer default — new cookies don't pollute the cache.

**Denylist** (`exclude`): strip only these cookies; everything else is kept. Useful when
most cookies are relevant but a few known-harmless ones fragment the cache.

Stripped cookies are removed from `req.http.Cookie` before the cache lookup **and** remain
stripped in the request sent to the backend. This is essential for correctness: if a cookie
is not part of the cache key, the backend must not see it either, because the backend might
return personalized content based on that cookie. That personalized response would then be
cached and served to other users — a classic cache poisoning scenario. The rule is simple:
if a cookie doesn't affect cache identity, it shouldn't affect the response either.

Glob patterns (e.g., `wordpress_logged_in_*`) would be useful here since WordPress appends
a hash to cookie names. Whether to support globs or regex is an open question.

**Impact:** Without cookie stripping, any site that sets analytics or consent cookies will
see near-zero cache hit rates, since each unique cookie combination produces a different
cache entry. This is the single highest-impact addition for CMS use cases.

### Query String Sorting

Two URLs that differ only in parameter order (`?b=2&a=1` vs `?a=1&b=2`) are semantically
identical but produce different cache keys. Varnish has no built-in query string sort, so
this is typically handled by a VMOD or inline VCL.

A possible boolean field:

```yaml
cacheKey:
  sortQueryParameters: true
```

When enabled, query parameters are sorted lexicographically by key before hashing. This
improves cache hit rates for sites where clients or intermediaries reorder parameters
inconsistently.

**Trade-off:** Sorting adds CPU cost per request. For most sites the improvement in hit rate
more than compensates, but it's not free. The ghost VMOD would handle this in Rust, so the
cost should be minimal.

**Interaction with `queryParameters.include`/`exclude`:** Sorting happens after filtering.
If you allowlist `[page, filter]`, only those two parameters are kept, then sorted. The
combination is well-defined and useful.

## Future Considerations

These are explicitly **not** in v1, but the design accommodates them:

- **Glob/regex patterns for query parameters**: The `queryParameters.include`/`exclude`
  fields currently use exact match. Supporting glob patterns (e.g., `utm_*`) or regex
  would reduce verbosity for common cases like stripping all UTM parameters. The same
  applies to cookie stripping if/when that feature is added.

- **Response Set-Cookie stripping**: The `defaultTTL` mode follows Varnish's default
  behavior of not caching responses with `Set-Cookie`. For sites where the origin sends
  harmless `Set-Cookie` headers (analytics, consent) on otherwise cacheable responses,
  a mechanism to strip specific `Set-Cookie` headers by name would allow caching without
  leaking session cookies.

- **Cache purge API**: A mechanism to invalidate cached objects. Could be a separate CRD
  (`VarnishCachePurge`) or an annotation-based trigger. Varnish supports purge natively.

- **Cache metrics**: Expose per-route hit/miss ratios via Prometheus. The ghost VMOD could
  track these per-route and expose via a stats endpoint.
