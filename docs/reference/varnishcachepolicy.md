# VarnishCachePolicy Reference

## Overview

Varnish Gateway ships with **caching disabled by default**. Every request passes straight through to the backend — no caching, no request coalescing. This is the safe default for Kubernetes, where blue/green deployments, canary rollouts, and traffic splitting all assume the proxy faithfully forwards requests.

**VarnishCachePolicy (VCP)** is how you opt in to caching. Attaching a VCP to a Gateway or HTTPRoute says: "I understand caching, and I want it here." No VCP, no caching — simple, explicit, auditable.

## Spec

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: my-cache-policy
  namespace: default
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute          # or Gateway
    name: my-route
    # sectionName: my-rule   # optional: target a specific named rule

  # TTL — exactly one of defaultTTL or forcedTTL is required
  defaultTTL: 5m
  # forcedTTL: 1h

  grace: 30s                 # serve stale while revalidating (unset: Varnish default_grace, 10s)
  keep: 24h                  # serve stale when backend is down (unset: Varnish default_keep, 0s)

  cacheKey:
    headers:
      - Accept-Language
    queryParameters:
      include:                # allowlist (mutually exclusive with exclude)
        - page
        - filter
      # exclude:              # denylist
      #   - utm_source

  bypass:
    headers:
      - name: Authorization
      - name: Cookie
        valueRegex: "session_id|admin_token"
```

### Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `targetRef` | PolicyTargetReference | required | Gateway, HTTPRoute, or HTTPRoute rule to attach to |
| `targetRef.sectionName` | string | — | Name of a specific rule within the targeted HTTPRoute |
| `defaultTTL` | Duration | required* | TTL when origin sends no Cache-Control. Origin headers take precedence |
| `forcedTTL` | Duration | required* | Forced TTL, ignores origin Cache-Control entirely |
| `grace` | Duration | unset | Serve stale while revalidating (equivalent to `stale-while-revalidate`). Unset leaves Varnish's `default_grace` (10s); `0s` disables grace. Max 10 years (`87600h`) |
| `keep` | Duration | unset | Keep stale objects for revalidation or backend failure. Unset leaves Varnish's `default_keep` (0s); `0s` disables keep. Max 10 years (`87600h`) |
| `cacheRuleOverrides` | []CacheRuleOverride | `[]` | Per-response TTL/grace/keep overrides, first match wins (max 64). See [cacheRuleOverrides](#cacheruleoverrides) |
| `cacheKey.headers` | []string | `[]` | Request headers to include in cache key |
| `cacheKey.queryParameters.include` | []string | all | Allowlist of query params in cache key |
| `cacheKey.queryParameters.exclude` | []string | none | Denylist of query params from cache key |
| `bypass.headers` | []HeaderCondition | `[]` | Headers that trigger cache bypass |

*Exactly one of `defaultTTL` or `forcedTTL` must be set.

A TTL may be negative (for example `-1s`), which means "do not cache"; see [Negative TTL](#negative-ttl). Zero and sub-second values are rejected.

Ghost-generated responses (for example the 404 for an unknown vhost, redirects, or the 500 returned when a route has no backends) are never cached by policy: no TTL, grace or keep from the VCP is applied to them.

## Hierarchy and Inheritance

VCP is an **Inherited Policy** per Gateway API conventions. The most specific policy wins:

```
Gateway                  ← VCP provides defaults for all routes through this gateway
  └── HTTPRoute          ← VCP overrides gateway defaults for all rules in this route
       └── Rule (named)  ← VCP overrides route defaults for this specific rule
```

- **No VCP anywhere**: caching disabled (pass mode)
- **VCP on Gateway only**: all routes through that gateway use the gateway policy
- **VCP on HTTPRoute**: all rules in that route use this policy, ignoring gateway defaults
- **VCP on a named rule**: that rule uses its own policy; other rules fall back to the HTTPRoute-level or Gateway-level VCP

Override is **complete replacement**, not field-level merging. An HTTPRoute VCP does not inherit the gateway VCP's `grace` or `cacheKey` settings — it uses exactly what you specified.

## defaultTTL vs forcedTTL

The two TTL modes encode different trust relationships with your origin:

### defaultTTL — Origin wins

Used when the origin doesn't send Cache-Control headers. Origin headers always take precedence.

| Origin sends | Result |
|---|---|
| No Cache-Control | TTL = `defaultTTL` value |
| `Cache-Control: max-age=60` | TTL = 60s (origin wins) |
| `Cache-Control: s-maxage=120` | TTL = 120s (origin wins) |
| `Cache-Control: no-store` | Not cached (origin wins) |
| `Cache-Control: private` | Not cached (origin wins) |
| `Set-Cookie` present | Not cached (Varnish default) |

### forcedTTL — Operator wins

Overrides origin Cache-Control headers, including `no-store` and `private`.
Use when you know better than the origin.

| Origin sends | Result |
|---|---|
| No Cache-Control | TTL = `forcedTTL` value |
| `Cache-Control: max-age=60` | TTL = `forcedTTL` value (ignored) |
| `Set-Cookie` present | TTL = `forcedTTL` value (header stripped) |
| `Cache-Control: no-store` | TTL = `forcedTTL` value (header stripped) |
| `Cache-Control: private` | TTL = `forcedTTL` value (header stripped) |

A positive `forcedTTL` removes `Cache-Control`, `Set-Cookie` and `Expires`
from the stored response before Varnish's built-in `vcl_backend_response`
runs, so `no-store` and `private` do not make the response uncacheable
(verified in `ghost/tests/test_cache_rule_overrides.vtc`).

## cacheRuleOverrides

The top-level fields apply to every response on the targeted route. `cacheRuleOverrides` changes TTL, grace and keep for specific responses, selected by request and response properties such as status code or response headers. Typical uses are caching a 404 for a second instead of the route default, or never caching 5xx responses.

```yaml
spec:
  targetRef: {...}
  defaultTTL: 5m
  grace: 30s
  keep: 24h
  cacheRuleOverrides:
    - match:
        status: ["404"]
        path: {type: PathPrefix, value: /live/}
      forcedTTL: 1s
      grace: 0s
    - match:
        status: ["5xx"]
      forcedTTL: -1s
    - match:
        responseHeaders:
          - {name: Content-Type, type: RegularExpression, value: "^image/"}
      defaultTTL: 1h
```

Caveat: a negative TTL creates a 120s hit-for-miss object. When a background fetch during grace returns an error that matches such a rule, that hit-for-miss object shadows the stale graced object, and clients get the error instead of stale content. To keep serving stale content on backend errors, abandon the background fetch in user VCL instead:

```vcl
sub vcl_backend_response {
    if (bereq.is_bgfetch && beresp.status >= 500) {
        return (abandon);
    }
}
```

### Rule fields

| Field | Type | Description |
|-------|------|-------------|
| `match` | object | Conditions, all ANDed. An empty or omitted `match` matches every response |
| `match.path` | HTTPPathMatch | `Exact`, `PathPrefix` or `RegularExpression`, as in `HTTPRouteMatch.path` |
| `match.method` | HTTPMethod | As in `HTTPRouteMatch.method`, except `GET` and `HEAD` match each other |
| `match.requestHeaders` | []HTTPHeaderMatch | `Exact` or `RegularExpression`, as in `HTTPRouteMatch.headers` |
| `match.queryParams` | []HTTPQueryParamMatch | As in `HTTPRouteMatch.queryParams` |
| `match.status` | []string | Response status. The condition holds if any entry matches |
| `match.responseHeaders` | []HTTPHeaderMatch | Like `requestHeaders`, evaluated against the backend response |
| `defaultTTL` | Duration | TTL used when the origin sent no Cache-Control. Mutually exclusive with `forcedTTL` |
| `forcedTTL` | Duration | TTL that overrides origin caching headers. Mutually exclusive with `defaultTTL` |
| `grace` | Duration | Grace for matching responses. Between `0s` and 10 years (`87600h`) |
| `keep` | Duration | Keep for matching responses. Between `0s` and 10 years (`87600h`) |

A rule must set at least one of `defaultTTL`, `forcedTTL`, `grace` or `keep`. A VCP may have at most 64 rules.

Status entries are one of:

- an exact code: `"404"`
- a class: `"4xx"` (400-499)
- an inclusive range: `"500-504"`

### Evaluation

- Rules are evaluated in order and the **first matching rule wins**. Later rules are not consulted.
- If no rule matches, the top-level fields apply as if there were no overrides.
- Fields the matching rule leaves unset fall back to the top-level values. The TTL falls back **as a unit**: if the rule sets either `defaultTTL` or `forcedTTL`, it replaces the top-level TTL entirely (whichever kind that was); if it sets neither, the top-level `defaultTTL` or `forcedTTL` applies. `grace` and `keep` fall back individually.
- Request-side conditions (`path`, `method`, `requestHeaders`, `queryParams`) are evaluated in `vcl_recv`, on the request that misses the cache, before HTTPRoute filters such as `URLRewrite` or `RequestHeaderModifier` are applied. On `return(restart)` they are re-evaluated against the current request. They are not part of the cache key: the resulting TTL belongs to the cached object and applies to every later client that hits it. For example, a rule matching `requestHeaders: [{name: X-Preview, value: "1"}]` with `forcedTTL: 1s` gives a 1-second TTL to the object fetched by the first request that carried `X-Preview: 1`, and all other clients get that short-lived object too. To vary caching per client, add the header to `cacheKey.headers` or use `bypass`.
- `HEAD` and `GET` are both matched as `GET`, because Varnish fetches a `HEAD` miss as a `GET`.
- When a header appears more than once, `requestHeaders` and `responseHeaders` match against the first occurrence only, the same as Varnish's `req.http` and `beresp.http`.
- Overrides are evaluated when the backend response arrives, so `status` and `responseHeaders` are available. A VCP version in use by an in-flight request stays available for 5 minutes after a config reload changes or removes it, so a request is always judged by the policy it started with. If a fetch outlives that, the response is not cached.

### Negative TTL

A negative `defaultTTL` or `forcedTTL`, in a rule or at the top level, means "do not cache". The response is marked uncacheable with a 120s hit-for-miss entry, so concurrent requests are not queued behind each other and a later cacheable response for the same object (for example a 200 once the content exists) is cached normally. The magnitude of the negative value is ignored. The policy's `grace` and `keep` (rule or top level) are not applied to these hit-for-miss entries; they keep Varnish's defaults, as with Varnish's built-in hit-for-miss.

- `forcedTTL: -1s` never caches matching responses.
- `defaultTTL: -1s` does not cache matching responses only when the origin sent no Cache-Control, the same condition under which a positive `defaultTTL` applies.

Zero is rejected, as is any TTL with a magnitude under one second.

### Positive forcedTTL and origin headers

When a positive `forcedTTL` applies, ghost removes `Set-Cookie`, `Cache-Control` and `Expires` from the stored response, so the TTL is not undone downstream.  This includes origin `no-store` and `private`, which a positive `forcedTTL` overrides.

### Grace and keep with short TTLs

Grace is not reset by a rule that sets only a TTL. A rule with `forcedTTL: 1s` inherits the top-level `grace`, so with `grace: 30s` the 1-second object can be served stale for 30 more seconds, defeating the purpose of the short TTL. Set `grace: 0s` (and `keep: 0s` if needed) in the rule to prevent this.

Unset and `0s` differ. Leaving `grace` or `keep` unset at every level leaves Varnish's defaults in place (`default_grace` 10s, `default_keep` 0s). An explicit `0s` sets the value to zero. Existing VCPs with an explicit `grace: 0s` or `keep: 0s` therefore now get 0s instead of the 10s grace they silently received before.

### Example: live video

Live streams publish chunks at predictable URLs. A request for the next chunk often arrives before the origin has it, and the origin answers 404. The chunk appears a second or two later. The route should cache chunks for a long time but cache the 404 only briefly, with no stale serving:

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: live-video
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: live
  defaultTTL: 1h
  grace: 30s
  cacheRuleOverrides:
    - match:
        status: ["404"]
      forcedTTL: 1s
      grace: 0s
      keep: 0s
```

Without `grace: 0s` the 404 would be served stale for up to 30s, long after the chunk exists.

## Status and Conditions

VCP reports status following Gateway API policy conventions:

| Condition | Reason | Meaning |
|-----------|--------|---------|
| Accepted=True | Accepted | Policy is valid and active |
| Accepted=False | TargetNotFound | Referenced HTTPRoute/Gateway doesn't exist |
| Accepted=False | Invalid | Spec validation failed (e.g., include+exclude both set) |
| Accepted=False | Conflicted | Another VCP already targets the same route |

Conflict resolution: if two VCPs target the same HTTPRoute, the oldest (by creation timestamp) wins.

## Examples

### Cache Static Assets

Two HTTPRoutes serve the same hostname. Static assets get cached; the API does not.

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: static-assets
spec:
  parentRefs:
    - name: my-gateway
  hostnames:
    - www.example.com
  rules:
    - matches:
        - path: { type: PathPrefix, value: /static }
      backendRefs:
        - name: cdn-origin
          port: 80
---
apiVersion: gateway.varnish-software.com/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: cache-static
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: static-assets
  defaultTTL: 1h
  grace: 5m
```

Result: `/static/logo.png` is cached for 1h with 5m stale-while-revalidate. API routes without a VCP remain in pass mode.

### Gateway-Wide Defaults with Per-Route Override

```yaml
# Conservative caching for all routes through this gateway
apiVersion: gateway.varnish-software.com/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: gateway-defaults
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
# Product catalog gets aggressive caching (full replacement — no inheritance)
apiVersion: gateway.varnish-software.com/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: cache-catalog
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

The `product-catalog` route uses 30m TTL with no bypass rules. The gateway-level Authorization/Cookie bypass does **not** apply — the route VCP is a complete replacement.

### Per-Rule Targeting

Different caching per rule within a single HTTPRoute, using `sectionName`:

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
apiVersion: gateway.varnish-software.com/v1alpha1
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
apiVersion: gateway.varnish-software.com/v1alpha1
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

Result:
- `/static/*` — forced 24h TTL (origin headers ignored)
- `/api/*` — no VCP, pass mode (no caching)
- `/*` — 5m default TTL, respects origin Cache-Control, bypasses for session cookies

### UTM Parameter Stripping

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: VarnishCachePolicy
metadata:
  name: cache-marketing
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

`/pricing?utm_source=google` and `/pricing?utm_source=twitter` share the same cache entry.

## Interaction with Traffic Splitting

Caching and weighted traffic splitting are in tension. If a route with multiple weighted backends has a VCP attached, the first response gets cached and all subsequent requests serve that cached version — the weight split becomes meaningless for cached paths.

**Recommendation:** Don't attach a VCP to routes with active traffic splitting unless the backends return identical content.
