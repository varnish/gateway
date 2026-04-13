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

  grace: 30s                 # serve stale while revalidating (default: 0)
  keep: 24h                  # serve stale when backend is down (default: 0)
  requestCoalescing: true    # collapsed forwarding (default: true)

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
| `grace` | Duration | `0` | Serve stale while revalidating (equivalent to `stale-while-revalidate`) |
| `keep` | Duration | `0` | Serve stale when backend is down (equivalent to `stale-if-error`) |
| `requestCoalescing` | bool | `true` | Collapsed forwarding for concurrent requests to the same uncached object |
| `cacheKey.headers` | []string | `[]` | Request headers to include in cache key |
| `cacheKey.queryParameters.include` | []string | all | Allowlist of query params in cache key |
| `cacheKey.queryParameters.exclude` | []string | none | Denylist of query params from cache key |
| `bypass.headers` | []HeaderCondition | `[]` | Headers that trigger cache bypass |

*Exactly one of `defaultTTL` or `forcedTTL` must be set.

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

Ignores all origin Cache-Control headers. Use when you know better than the origin.

| Origin sends | Result |
|---|---|
| No Cache-Control | TTL = `forcedTTL` value |
| `Cache-Control: max-age=60` | TTL = `forcedTTL` value (ignored) |
| `Cache-Control: no-store` | TTL = `forcedTTL` value (ignored) |
| `Set-Cookie` present | TTL = `forcedTTL` value (header stripped) |

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
