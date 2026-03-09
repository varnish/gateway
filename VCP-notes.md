# VCP Implementation Notes

Issues identified during implementation. Items marked FIXED have been addressed.

## 1. [FIXED] `beresp.uncacheable = false` is a no-op

**File:** `internal/vcl/preamble.vcl`

Removed the `beresp.uncacheable = false` line. Added comment documenting that forced TTL
only affects responses Varnish considers cacheable. Also added `unset beresp.http.Expires`
to the forced TTL block (was missing).

## 2. [HIGH] Regex compiled on every request

**File:** `ghost/src/vhost_director.rs`

Bypass header regex is compiled from the pattern string on every request. This is expensive
and unnecessary since patterns don't change between reloads.

**Fix:** Pre-compile regexes during config load in `config.rs`. Store `Option<Regex>`
alongside or instead of the string pattern in `BypassHeaderConfig`.

## 3. [HIGH] CachePolicy missing from route merge key

**File:** `internal/ghost/generator.go:90-99`

The `routeKey` struct used in `mergeRoutesByMatchCriteria` doesn't include cache policy.
Two routes with identical path/method/header match criteria but different cache policies
will be merged into one, and only the first route's cache policy survives.

**Fix:** Add a serialized cache policy field to `routeKey`. Or verify that the policy is
already resolved before merging and document that same-match routes always share the same
policy.

## 4. [HIGH] isVCPAccepted returns true for unreconciled VCPs

**File:** `internal/controller/varnishcachepolicy_controller.go`

A VCP with no status (never reconciled, or status cleared) is treated as accepted. This
means a VCP that targets a nonexistent gateway or has validation errors will be applied
until its controller reconciles and rejects it.

**Fix:** Invert the default — treat no-status as not-accepted. The VCP controller
reconciles first and sets status before the HTTPRoute controller picks it up.

## 5. [MEDIUM] Annotation-based HTTPRoute re-reconciliation

**File:** `internal/controller/varnishcachepolicy_controller.go`

The VCP controller triggers HTTPRoute re-reconciliation by updating an annotation. This
creates unnecessary etcd writes and can conflict with other controllers.

**Alternative:** Use controller-runtime's `EnqueueRequestsFromMapFunc` to directly enqueue
HTTPRoute reconciliation when a VCP changes.

## 6. [FIXED] Forced TTL doesn't strip Expires header

Added `unset beresp.http.Expires;` in the forced TTL block (see item 1).

## 7. [LOW] Custom PolicyTargetReference vs Gateway API standard

**File:** `api/v1alpha1/varnishcachepolicy_types.go`

We define a custom `PolicyTargetReference` instead of using the Gateway API standard type.
Revisit when policy attachment graduates to beta/stable.

## 8. [FIXED] Pass and hash_ignore_busy used header workarounds

Ghost now uses `ctx.set_pass()` and `ctx.set_hash_ignore_busy()` directly via the Varnish
C API instead of setting `X-Ghost-Cache-Pass` and `X-Ghost-Hash-Ignore-Busy` headers for
VCL to interpret. The VCL preamble no longer needs to check for these headers.

Default behavior: all routes get `hash_ignore_busy = true` and `pass = true` (no caching,
no coalescing) unless a VarnishCachePolicy opts in to caching.
