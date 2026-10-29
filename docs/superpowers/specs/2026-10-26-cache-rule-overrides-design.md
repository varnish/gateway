# Cache Rule Overrides for VarnishCachePolicy — Design

Date: 2026-10-26
Status: draft, pending approval

## Intent

Today a `VarnishCachePolicy` (VCP) applies a single TTL / grace / keep to
every cacheable response on the routes it covers. Operators need to vary
caching behavior based on the **response** (status code, response headers),
optionally combined with request properties.

Motivating example (one of many): live video. A player eagerly requests
`chunk562.ts` before the origin has produced it, gets a 404, and Varnish
caches that 404 with the same 5-minute TTL as real chunks. Once the chunk
exists and the manifest advertises it, every viewer still gets the cached
404 for minutes. The operator wants 200s cached for 5m and 404s for ~1s.

The feature is deliberately generic: "when the request/response looks like
X, cache it with these settings instead".

## Goals

- New ordered list `cacheRuleOverrides` in `VarnishCachePolicySpec`.
- Each entry: a `match` plus any of `defaultTTL`, `forcedTTL`, `grace`,
  `keep`, with the same meaning as the top-level fields.
- `match` can test the request (HTTPRouteMatch-like) and the response
  (status codes/ranges, response headers).
- A negative TTL disables caching for matching responses.
- Lives inside VCP and follows existing VCP inheritance unchanged.

## Non-goals

- No new CRD.
- No changes to cache key composition or request bypass per rule (those stay
  top-level, request-time decisions).
- No field-level merging across the VCP hierarchy (Gateway → HTTPRoute →
  rule stays complete replacement, overrides list included).
- Cannot force-cache a response Varnish has already marked uncacheable
  (existing limitation, see `internal/vcl/preamble.vcl` comment on
  `beresp.uncacheable`).

## Anti-goals

- A short-TTL override that still lives on through the top-level grace
  without the user being able to prevent it. (Users prevent it by setting
  `grace: 0s` in the rule; see fallback below. Docs must call this out.)
- Breaking existing VCPs: a VCP without `cacheRuleOverrides` must behave
  exactly as today.
- Leaking internal `X-Ghost-*` headers to backends or clients, or letting
  clients spoof them.

## API

```yaml
spec:
  targetRef: {...}
  defaultTTL: 5m            # top-level: baseline, unchanged
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
      forcedTTL: -1s          # negative: don't cache
    - match:
        responseHeaders:
          - {name: Content-Type, type: RegularExpression, value: "^image/"}
      defaultTTL: 1h
```

### `match` (all present conditions ANDed)

| Field | Type | Semantics |
|-------|------|-----------|
| `path` | HTTPPathMatch (Exact / PathPrefix / RegularExpression) | Same as Gateway API `HTTPRouteMatch.path` |
| `method` | HTTPMethod | Same as `HTTPRouteMatch.method` |
| `requestHeaders` | []HTTPHeaderMatch (Exact / RegularExpression) | Same as `HTTPRouteMatch.headers`, renamed for symmetry |
| `queryParams` | []HTTPQueryParamMatch | Same as `HTTPRouteMatch.queryParams` |
| `status` | []string | Response status; entry matches if **any** list item matches (OR within the list) |
| `responseHeaders` | []HTTPHeaderMatch (Exact / RegularExpression) | Like `requestHeaders`, evaluated against the backend response |

Reuse Gateway API Go types for path/method/header/query matches where
practical so validation and semantics match what users already know.

An empty `match` (or omitted) matches everything — useful as a final
catch-all, though equivalent to top-level fields.

**Status syntax** (decided by Claude, not the user — user asked for ranges,
syntax is our call): each item is a string, one of:
- exact code: `"404"`
- class wildcard: `"4xx"` (any of 400–499)
- inclusive range: `"500-504"`

Validate with a CRD pattern (e.g. `^([1-5][0-9]{2}|[1-5]xx|[1-5][0-9]{2}-[1-5][0-9]{2})$`)
plus controller validation that range start ≤ end.

**Request-side matching target** (Claude's call): request conditions are
evaluated against the request as the client sent it (before HTTPRoute
filters such as URLRewrite / RequestHeaderModifier), consistent with what
`HTTPRouteMatch` means everywhere else. How ghost preserves that at
`vcl_backend_response` time is left to the builder (it may need to capture
the needed values in `vcl_recv`, or evaluate request-side conditions there
and carry a per-rule bitmask; builder's choice).

### Rule fields

`defaultTTL`, `forcedTTL`, `grace`, `keep` — `metav1.Duration`, all optional.

- `defaultTTL` and `forcedTTL` are mutually exclusive within a rule.
- A rule must set at least one of the four fields (Claude's call: reject
  no-op rules at validation).
- `grace` and `keep` must be ≥ 0.

### Evaluation (user-decided)

- Rules are evaluated **in order; first matching rule wins**. Later rules
  are not consulted.
- If no rule matches, top-level fields apply exactly as today.
- **Fallback:** any field the matching rule leaves unset falls back to the
  VCP's top-level value. This keeps behavior consistent with how users think
  of inheritance.
  - Claude's call: the TTL pair falls back **as a unit**. If the rule sets
    either `defaultTTL` or `forcedTTL`, that replaces the top-level TTL
    (whichever kind it was). If it sets neither, the top-level TTL (default
    or forced) applies. `grace` and `keep` fall back individually.

### Unset vs zero for grace/keep (user-decided)

Today an omitted `grace`/`keep` and an explicit `0s` are indistinguishable:
`specToCachePolicy` maps both to `0`, ghost only emits `X-Ghost-Grace` /
`X-Ghost-Keep` when `> 0`, so Varnish's `default_grace` (10s) /
`default_keep` apply. That contradicts the docs ("Default: 0 (disabled)")
and would defeat the anti-goal (a 1s 404 served stale for ~10s).

Decision:
- `grace` and `keep` become **optional (unset vs set)** through every layer:
  Go `ghost.CachePolicy` (pointers), routing.json / ghost.json (absent vs
  present), Rust `CachePolicy` (`Option`).
- An explicit `0s` means **zero**, at both rule level and top level, and is
  applied to `beresp.grace` / `beresp.keep`.
- Unset (in the rule and at top level) means "don't touch": Varnish's
  defaults apply. Docs must state this accurately.
- This changes behavior for existing VCPs with explicit top-level
  `grace: 0s` / `keep: 0s` (10s → 0s). Treat as a bug fix; CHANGELOG entry
  must call it out.

### Ghost-generated responses (user-decided)

Ghost's synthetic backends (`internal_error_backend.rs`,
`not_found_backend.rs`, `redirect_backend.rs`, the synthetic path in
`external_backend.rs`) set `Cache-Control: no-store`. A forced TTL would
strip that and cache e.g. "no backends available" past recovery (the bug
`ghost/tests/test_cached_error_race.vtc` guards against).

Decision: **no cache policy TTL/grace/keep is ever applied to
ghost-generated responses** — neither overrides nor top-level fields. This
also changes today's top-level `forcedTTL` behavior for those responses
(fixes a latent bug); CHANGELOG must mention it. How ghost recognizes its
own responses at `vcl_backend_response` is left to the builder.

### Negative TTL (user-decided: negative TTL disables caching)

- Applies to `defaultTTL` and `forcedTTL`, in rules **and** at top level
  (Claude's call, for consistency; top-level validation currently rejects
  `<= 0` and must be relaxed to reject only `== 0`).
- `forcedTTL < 0`: matching response is not cached.
- `defaultTTL < 0`: response not cached **when origin sent no
  Cache-Control** (same condition as positive `defaultTTL`).
- Zero TTL remains rejected (ambiguous meaning).
- Implementation: set `beresp.uncacheable = true` with a hit-for-miss
  lifetime, so concurrent requests aren't serialized on the waiting list and
  a later cacheable response (e.g. the 200 once the chunk exists) can be
  cached normally. Claude's call: magnitude of the negative value is
  ignored; use Varnish's builtin hit-for-miss duration (120s). Builder may
  propose otherwise if there's a good reason.

## How it runs (user-decided direction)

Ghost must evaluate overrides at `vcl_backend_response`, since status and
response headers only exist there.

- In `vcl_recv`, ghost already resolves the route and its `CachePolicy`. It
  records which policy applies (content key) and which rules' request-side
  conditions matched in an internal bereq-bridging header (e.g.
  `X-Ghost-Cache-Policy`), per the "headers only for cross-subroutine
  bridging" rule in AGENTS.md.
- A new VMOD function, e.g. `ghost.backend_response()`, called from the
  preamble's `vcl_backend_response`, looks up the policy from ghost's loaded
  config, evaluates overrides against bereq/beresp, and sets
  `beresp.ttl/grace/keep/uncacheable` directly via the Varnish API.
- The new header must be added to the client-input strip list at the top of
  the preamble's `vcl_recv` and removed before the backend request leaves /
  after use.
- **Config reload between recv and backend_response** (user-decided):
  ghost reloads on every EndpointSlice change, so this is routine during
  rollouts/scaling, and a backend fetch can take seconds. The request's
  rule bitmask is only meaningful against the exact policy version that
  computed it. Therefore:
  - Compiled policies are keyed by a **hash of their content**, not by
    reload generation or position. Reloads that don't change a policy
    (the vast majority — endpoint changes) keep the same key, so lookup
    succeeds.
  - Policies that a reload changes or removes are **retained for 5
    minutes** (Claude's call on the duration) after they leave the live
    config, so in-flight requests complete with the exact version they
    started with. A request always gets recv-time policy semantics.
  - **Last resort**, if the key is still unknown (fetch outlived
    retention): the response is not cached (hit-for-miss), never the
    top-level TTL. An override must never be silently skipped.
  - 64-bit hash collisions between two different policies are ignored
    (probability negligible).
- Whether the existing `X-Ghost-Default-TTL` / `Forced-TTL` / `Grace` /
  `Keep` headers are kept or replaced entirely by the new path is left to the
  builder; observable behavior for VCPs without overrides must not change.
- Regexes (path, header) compile once at config load, like bypass headers
  today.
- On 304 revalidation, Varnish presents the merged object (status of the
  stale object, typically 200) in `vcl_backend_response`; matching uses
  `beresp` as Varnish presents it. No special-casing.

Current plumbing to extend:
- API: `api/v1alpha1/varnishcachepolicy_types.go` (+ deepcopy, CRD manifests
  in `charts/` and `deploy/`).
- Validation: `validateSpec` in
  `internal/controller/varnishcachepolicy_controller.go`.
- Translation: `specToCachePolicy` (same file) → `ghost.CachePolicy` in
  `internal/ghost/config.go` → routing.json / ghost.json.
- Ghost: `CachePolicy` in `ghost/src/config.rs` (TTL fields are `u32`;
  must become signed), `apply_cache_policy_headers` in
  `ghost/src/vhost_director.rs`, VMOD entry points in `ghost/src/lib.rs`.
- VCL: `internal/vcl/preamble.vcl` `vcl_backend_response` and recv strip
  list.
- Route grouping in `internal/ghost/generator.go` serializes CachePolicy for
  grouping; overrides must be included in that serialization.

## Testing expectations

- Go unit tests: validation (mutual exclusion per rule, status syntax and
  range order, no-op rule rejection, zero TTL rejection, negative allowed),
  spec→config translation.
- Rust tests: matcher evaluation (each match type, AND semantics, status
  OR, first-match-wins, fallback rules including TTL-as-unit).
- VCL / varnishtest-level test(s) proving end-to-end: 200 cached with
  top-level TTL, 404 cached with override TTL, negative TTL response not
  cached and a subsequent 200 is cached, grace fallback behavior.
- Grace/keep: explicit `0s` yields zero grace/keep; unset leaves Varnish
  defaults; rule fallback picks up top-level value.
- Ghost-generated 500 (no backends) is never cached even with a top-level
  `forcedTTL` or a matching `5xx` override.
- Existing tests for VCPs without overrides must pass unchanged, except
  where they encode the two deliberate behavior changes above.

## Docs

- `docs/reference/varnishcachepolicy.md`: new section with field table,
  status syntax, evaluation order, fallback, negative TTL, the grace
  footgun, and the live-video example.
- `docs/concepts/caching-model.md` if it describes TTL resolution.
- `CHANGELOG.md` entry, including the two behavior changes (explicit
  `0s` grace/keep now means zero; policies never apply to ghost-generated
  responses).

## Left to the builder

- Exact Go/Rust type names and JSON field names in routing.json/ghost.json.
- Policy identifier scheme and lookup structure in ghost.
- How request-side values are preserved to backend-response time.
- Whether legacy `X-Ghost-*` TTL headers remain.
- Max list length / CEL validation details.
