# Cache Rule Overrides Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a VarnishCachePolicy override TTL/grace/keep per response (status, response headers) and per request (path, method, headers, query), with negative TTL meaning "don't cache".

**Architecture:** The operator translates `cacheRuleOverrides` into routing.json. Ghost compiles each route's policy at reload. In `vcl_recv` it evaluates the request-side conditions on the original (pre-filter) request, then writes a JSON directive (`X-Ghost-Cache-Policy`: content-hash policy key, request bitmask) to the request. Compiled policies live in a lock-free table keyed by content hash; policies a reload removes stay retained 5 minutes so in-flight fetches use their recv-time version. A new `router.backend_response()` method, called from the preamble's `vcl_backend_response`, evaluates the response side and sets `beresp.ttl/grace/keep/uncacheable` through the Varnish C API. The VCL TTL logic and the four `X-Ghost-{Default-TTL,Forced-TTL,Grace,Keep}` headers are deleted.

**Tech Stack:** Go (controller-runtime, Gateway API types, controller-gen), Rust (varnish-rs 0.7 VMOD, serde, regex), VCL, varnishtest.

**Spec:** `docs/superpowers/specs/2026-10-26-cache-rule-overrides-design.md`

## Global Constraints

- Override evaluation: first matching rule wins; no match → top-level settings.
- Fallback: TTL pair (`defaultTTL`/`forcedTTL`) falls back as a unit; `grace` and `keep` fall back individually.
- `defaultTTL` and `forcedTTL` are mutually exclusive, both at top level and inside a rule.
- TTL `0` is rejected. Negative TTL = don't cache. For `defaultTTL`, that only applies when origin sent no `Cache-Control`.
- A negative TTL becomes `beresp.uncacheable = true` with a hit-for-miss lifetime of 120s. The magnitude of the negative value is ignored.
- `grace`/`keep`: unset = leave Varnish defaults untouched; explicit `0s` = set to 0. Negative values are rejected.
- Status syntax: `"404"`, `"4xx"`, `"500-504"` (inclusive, start ≤ end). Codes 100–599.
- Max 64 rules per policy (request bitmask is `u64`).
- Request-side conditions are evaluated against the request before HTTPRoute filters (URLRewrite, RequestHeaderModifier).
- No cache-policy TTL/grace/keep is ever applied to ghost-generated responses (not_found, redirect, internal_error, external-proxy 405).
- Policies keyed by 64-bit content hash; policies that leave the live config are retained 300s (`POLICY_RETENTION`); unknown key at backend_response → hit-for-miss (not cached), never the top-level TTL.
- A rule must set at least one of defaultTTL/forcedTTL/grace/keep.
- Ghost conventions (`ghost/CLAUDE.md`): no backward-compat shims; delete replaced code outright; `cargo fmt` + `cargo clippy` clean.
- Go conventions (`AGENTS.md`): stdlib `testing`, `log/slog`, vendored deps, errors as `fmt.Errorf("x(%s): %w", ...)`.

## Review Focus

1. **Sub-second durations** (`500ms`, `-400ms`) round to 0 seconds in translation. Expected: rejected at validation with a clear message, never silently becoming "uncached" or "unset". Test in Task 1.
2. **Response header name case** (`content-type` in the rule vs `Content-Type` from origin). Expected: matches case-insensitively. Test in Task 5 VTC.
3. **Ghost reload between recv and backend_response** (an endpoint-only reload, a changed policy, a removed policy, and retention expiry). Expected: an unchanged policy keeps its key; a changed or removed policy still resolves within retention using the recv-time version; after expiry the response isn't cached. No panics. Test in Task 4 (`PolicyTable` unit tests) and Task 5 VTC case (g).
4. **Route with a URLRewrite filter**, where the rule's `path` targets the client-facing path. Expected: the rule matches the original path, not the rewritten one. Test in Task 5 VTC.
5. **Client sends its own `X-Ghost-Cache-Policy` header** on a route with no VCP, or on a passed request. Expected: it's ignored and caching is unchanged. Test in Task 5 VTC; strip list in Task 6.

---

### Task 1: API types and validation

**Files:**
- Modify: `api/v1alpha1/varnishcachepolicy_types.go`
- Regenerate: `api/v1alpha1/zz_generated.deepcopy.go`, `charts/varnish-gateway/crds/gateway.varnish-software.com_varnishcachepolicies.yaml` (and any `deploy/` copy) via `make generate manifests`
- Modify: `internal/controller/varnishcachepolicy_controller.go` (`validateSpec`, ~line 198)
- Test: `internal/controller/varnishcachepolicy_controller_test.go` (`TestValidateSpec`)

**Interfaces:**
- Produces (package `v1alpha1`):
  ```go
  // in VarnishCachePolicySpec:
  // +kubebuilder:validation:MaxItems=64
  // +optional
  CacheRuleOverrides []CacheRuleOverride `json:"cacheRuleOverrides,omitempty"`

  type CacheRuleOverride struct {
      Match      CacheRuleMatch   `json:"match,omitempty"`
      DefaultTTL *metav1.Duration `json:"defaultTTL,omitempty"`
      ForcedTTL  *metav1.Duration `json:"forcedTTL,omitempty"`
      Grace      *metav1.Duration `json:"grace,omitempty"`
      Keep       *metav1.Duration `json:"keep,omitempty"`
  }
  type CacheRuleMatch struct {
      Path            *gatewayv1.HTTPPathMatch       `json:"path,omitempty"`
      Method          *gatewayv1.HTTPMethod          `json:"method,omitempty"`
      RequestHeaders  []gatewayv1.HTTPHeaderMatch    `json:"requestHeaders,omitempty"`
      QueryParams     []gatewayv1.HTTPQueryParamMatch `json:"queryParams,omitempty"`
      // +kubebuilder:validation:items:Pattern=`^([1-5][0-9]{2}|[1-5]xx|[1-5][0-9]{2}-[1-5][0-9]{2})$`
      Status          []string                       `json:"status,omitempty"`
      ResponseHeaders []gatewayv1.HTTPHeaderMatch    `json:"responseHeaders,omitempty"`
  }
  ```
- Produces (package `controller`): `func parseStatusMatch(s string) (lo, hi int, err error)`. Task 3 does not need it; the Rust side has its own parser.

- [ ] **Step 1: Write failing `TestValidateSpec` cases** (table rows, `wantErr` substring):
  - top-level `ForcedTTL: -1s` → valid; `DefaultTTL: -1s` → valid
  - top-level `DefaultTTL: 0` → `"defaultTTL must not be zero"`
  - top-level `DefaultTTL: 500ms` → `"defaultTTL must be at least 1s in magnitude"` (rounds to 0)
  - rule with only `Match.Status: ["404"]` and no fields → `"cacheRuleOverrides[0]: must set at least one of defaultTTL, forcedTTL, grace, keep"`
  - rule with both TTLs → `"cacheRuleOverrides[0]: defaultTTL and forcedTTL are mutually exclusive"`
  - rule `ForcedTTL: 0` → `"cacheRuleOverrides[0]: forcedTTL must not be zero"`
  - rule `Grace: -1s` → `"cacheRuleOverrides[0]: grace must be non-negative"`
  - rule status `"504-500"` → `"cacheRuleOverrides[0]: status \"504-500\": range start must be <= end"`
  - rule status `"600"` / `"4x"` → `"cacheRuleOverrides[0]: status \"600\": invalid"` (controller validates too; envtest isn't the only guard)
  - rule `Path: RegularExpression "("` → `"cacheRuleOverrides[0]: invalid path regex"`; same for header/query regexes (`requestHeaders`/`responseHeaders`/`queryParams` in message)
  - valid: two rules, `{status:[404], forcedTTL:1s, grace:0s}`, `{status:[5xx], forcedTTL:-1s}`
- [ ] **Step 2:** `go test ./internal/controller -run TestValidateSpec` → FAIL (compile error on new fields).
- [ ] **Step 3: Add the types, then run `make generate manifests`.** Change the `validateSpec` TTL checks from `<= 0` to: reject when `math.Round(d.Seconds()) == 0`, using the error texts above. Add per-rule validation, prefixing errors with `cacheRuleOverrides[%d]: `. Compile regexes with Go `regexp`. Ghost uses Rust `regex`, but the syntax is close enough for an early check.
- [ ] **Step 4:** `go test ./internal/controller -run TestValidateSpec` → PASS. `make verify-manifests` → clean.
- [ ] **Step 5: Commit** `feat(api): add cacheRuleOverrides to VarnishCachePolicy`.

### Task 2: Go translation to routing.json

**Files:**
- Modify: `internal/ghost/config.go` (`CachePolicy`, ~line 139)
- Create: `internal/vcl/match_convert.go`; Modify: `internal/vcl/generator.go:257-330` to call it
- Modify: `internal/controller/varnishcachepolicy_controller.go` (`specToCachePolicy`)
- Test: `internal/controller/varnishcachepolicy_controller_test.go` (`TestSpecToCachePolicy`), `internal/vcl/match_convert_test.go`, fix `internal/ghost/generator_traffic_split_test.go` for pointer grace

**Interfaces:**
- Consumes: Task 1 types.
- Produces (package `ghost`), JSON names are the routing.json/ghost.json contract for Task 3:
  ```go
  type CachePolicy struct {
      DefaultTTLSeconds *int `json:"default_ttl_seconds,omitempty"`
      ForcedTTLSeconds  *int `json:"forced_ttl_seconds,omitempty"`
      GraceSeconds      *int `json:"grace_seconds,omitempty"`   // was int
      KeepSeconds       *int `json:"keep_seconds,omitempty"`    // was int
      CacheKey          *CacheKeyConfig       `json:"cache_key,omitempty"`
      BypassHeaders     []BypassHeaderConfig  `json:"bypass_headers,omitempty"`
      Overrides         []CacheRuleOverride   `json:"overrides,omitempty"`
  }
  type CacheRuleOverride struct {
      Match             CacheRuleMatch `json:"match"`
      DefaultTTLSeconds *int `json:"default_ttl_seconds,omitempty"`
      ForcedTTLSeconds  *int `json:"forced_ttl_seconds,omitempty"`
      GraceSeconds      *int `json:"grace_seconds,omitempty"`
      KeepSeconds       *int `json:"keep_seconds,omitempty"`
  }
  type CacheRuleMatch struct {
      PathMatch       *PathMatch        `json:"path_match,omitempty"`
      Method          *string           `json:"method,omitempty"`
      RequestHeaders  []HeaderMatch     `json:"request_headers,omitempty"`
      QueryParams     []QueryParamMatch `json:"query_params,omitempty"`
      Status          []string          `json:"status,omitempty"`
      ResponseHeaders []HeaderMatch     `json:"response_headers,omitempty"`
  }
  ```
- Produces (package `vcl`): `ConvertPathMatch(*gatewayv1.HTTPPathMatch) *ghost.PathMatch` (keeps the "PathPrefix / → nil" rule), `ConvertHeaderMatches([]gatewayv1.HTTPHeaderMatch) []ghost.HeaderMatch`, `ConvertQueryParamMatches([]gatewayv1.HTTPQueryParamMatch) []ghost.QueryParamMatch`.

- [ ] **Step 1: Write failing tests:**
  - `TestSpecToCachePolicy` rows: `Grace: 0s` → `GraceSeconds` is non-nil `0`; grace unset → `nil`; same for keep; `ForcedTTL: -1s` → `-1`; one override `{Match{Status:["404"], Path: PathPrefix /live/}, ForcedTTL:1s, Grace:0s}` → `Overrides[0]` with `Status ["404"]`, `PathMatch{PathPrefix,"/live/"}`, `ForcedTTLSeconds 1`, `GraceSeconds 0`; `RequestHeaders`/`ResponseHeaders` with `Type` nil → `MatchTypeExact`.
  - `TestConvertPathMatch`: nil type → PathPrefix; `PathPrefix "/"` → nil; Regex preserved.
- [ ] **Step 2:** `go test ./internal/controller ./internal/vcl -run 'TestSpecToCachePolicy|TestConvert'` → FAIL.
- [ ] **Step 3: Implement.** Move the inline conversions out of `CollectHTTPRouteBackends` into `match_convert.go`, with no behaviour change. Extend `specToCachePolicy` to fill the new fields. Fix compile errors where `GraceSeconds`/`KeepSeconds` are used as `int`. Route grouping (`serializeCachePolicy`) already serializes the whole struct, so it needs no change.
- [ ] **Step 4:** `make test-go` → PASS (the existing generator/conformance-shaped tests still pass).
- [ ] **Step 5: Commit** `feat(operator): translate cacheRuleOverrides into routing config`.

### Task 3: Ghost config parsing and validation

**Files:**
- Modify: `ghost/src/config.rs` (`CachePolicy` ~line 182, `validate` ~line 287)
- Test: `ghost/src/config.rs` `mod tests`

**Interfaces:**
- Consumes: the JSON contract from Task 2.
- Produces:
  ```rust
  pub struct CachePolicy {
      pub default_ttl_seconds: Option<i64>,
      pub forced_ttl_seconds: Option<i64>,
      pub grace_seconds: Option<u32>,
      pub keep_seconds: Option<u32>,
      pub cache_key: Option<CacheKeyConfig>,
      #[serde(default)] pub bypass_headers: Vec<BypassHeaderConfig>,
      #[serde(default)] pub overrides: Vec<CacheRuleOverride>,
  }
  pub struct CacheRuleOverride {
      #[serde(default, rename = "match")] pub match_: CacheRuleMatch,
      pub default_ttl_seconds: Option<i64>, pub forced_ttl_seconds: Option<i64>,
      pub grace_seconds: Option<u32>, pub keep_seconds: Option<u32>,
  }
  #[derive(Default)] pub struct CacheRuleMatch {
      pub path_match: Option<PathMatch>, pub method: Option<String>,
      #[serde(default)] pub request_headers: Vec<HeaderMatch>,
      #[serde(default)] pub query_params: Vec<QueryParamMatch>,
      #[serde(default)] pub status: Vec<String>,
      #[serde(default)] pub response_headers: Vec<HeaderMatch>,
  }
  // All of CachePolicy, CacheRuleOverride, CacheRuleMatch, CacheKeyConfig,
  // BypassHeaderConfig, PathMatch, PathMatchType, MatchType, HeaderMatch,
  // QueryParamMatch additionally derive `Hash, PartialEq, Eq` (Task 4 keys policies by content hash).
  pub const MAX_CACHE_OVERRIDES: usize = 64;
  pub fn parse_status_match(s: &str) -> Result<(u16, u16), String>; // "404"→(404,404), "4xx"→(400,499), "500-504"→(500,504)
  ```

- [ ] **Step 1: Write failing tests:** `test_cache_policy_overrides_parsing` (full JSON round-trip of one override with every match field); `test_cache_policy_grace_zero_vs_absent` (`"grace_seconds": 0` → `Some(0)`, absent → `None`); `test_parse_status_match` (the three forms plus errors for `"600"`, `"4x"`, `"504-500"`, `"099"`); `test_validate_rejects_too_many_overrides` (65 rules → `Err` containing `"overrides"`); `test_validate_rejects_override_both_ttls`; `test_validate_rejects_top_level_both_ttls`; `test_validate_rejects_bad_override_status`.
- [ ] **Step 2:** `cd ghost && cargo test --lib -- --skip vtc_ config::` → FAIL.
- [ ] **Step 3: Implement.** In `validate`, for each route's `cache_policy`, check the count limit and that the two TTLs aren't both set (top level and per rule), and run `parse_status_match`. Reuse `validate_path_match` / `validate_method` / `validate_header_match` / `validate_query_param_match`, with context `"{route_ctx} override {i}"`.
- [ ] **Step 4:** Same command → PASS. `cargo clippy` is clean, though callers of `grace_seconds` in `vhost_director.rs` break until Task 5. If the crate must compile now, change those call sites minimally to `.unwrap_or(0) > 0`. Task 5 deletes them.
- [ ] **Step 5: Commit** `feat(ghost): parse cache rule overrides from ghost.json`.

### Task 4: Ghost policy compilation and evaluation (pure logic)

**Files:**
- Create: `ghost/src/cache_policy.rs` (register `mod cache_policy;` in `lib.rs`)
- Modify: `ghost/src/director.rs`: make `PathMatchCompiled::from_config`, `HeaderMatchCompiled::from_config` and `QueryParamMatchCompiled::from_config` `pub(crate)`. Add `HeaderMatchCompiled::name(&self) -> &str` and `HeaderMatchCompiled::matches_value(&self, value: Option<&str>) -> bool`, and have `matches(&HttpHeaders)` delegate to them.
- Test: `ghost/src/cache_policy.rs` `mod tests`

**Interfaces:**
- Consumes: Task 3 config types.
- Produces:
  ```rust
  pub const CACHE_POLICY_HEADER: &str = "X-Ghost-Cache-Policy";
  pub const SYNTHETIC_HEADER: &str = "X-Ghost-Synthetic";
  pub const HIT_FOR_MISS_SECONDS: f64 = 120.0;

  #[derive(Debug, Clone, Copy, PartialEq, Serialize, Deserialize)]
  pub enum Ttl { Default(i64), Forced(i64) }

  #[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
  pub struct CacheSettings { pub ttl: Option<Ttl>, pub grace: Option<u32>, pub keep: Option<u32> }

  pub const POLICY_RETENTION: Duration = Duration::from_secs(300);

  /// Serialized as JSON into CACHE_POLICY_HEADER. k = policy content key, m = request mask.
  #[derive(Debug, Clone, Serialize, Deserialize)]
  pub struct CacheDirective { pub k: u64, pub m: u64 }

  /// Content key: std `DefaultHasher` over `CachePolicy`'s derived Hash (deterministic in-process).
  pub fn policy_key(cp: &CachePolicy) -> u64;

  pub struct CompiledCachePolicy { pub key: u64, pub base: CacheSettings,
      pub cache_key: Option<CacheKeyConfig>, /* private */ overrides: Vec<CompiledOverride> }

  /// Lock-free-read table (held in ArcSwap by GhostDirector). Live entries have no expiry;
  /// entries absent from the latest config carry the Instant they expire.
  pub struct PolicyTable { /* HashMap<u64, (Arc<CompiledCachePolicy>, Option<Instant>)> */ }
  impl PolicyTable {
      pub fn empty() -> Self;
      /// Live = `live`; keys in `prev` but not in `live` are kept with expiry
      /// (prev expiry if already retired, else now + retention), dropped once expired.
      pub fn rebuild(prev: &PolicyTable, live: Vec<Arc<CompiledCachePolicy>>,
                     now: Instant, retention: Duration) -> PolicyTable;
      pub fn get(&self, key: u64) -> Option<&Arc<CompiledCachePolicy>>;
  }

  impl CompiledCachePolicy {
      pub fn compile(cp: &CachePolicy) -> Result<Self, String>; // key = policy_key(cp)
      /// Bit i set when rule i's request-side conditions all hold (rules without any → always set).
      pub fn request_mask<'h>(&self, path: &str, method: &str, query: Option<&str>,
                              header: impl Fn(&str) -> Option<&'h str>) -> u64;
      /// First rule whose bit is in `mask` and whose status + response headers match; fallback per Global Constraints.
      pub fn resolve<'h>(&self, mask: u64, status: u16,
                         header: impl Fn(&str) -> Option<&'h str>) -> CacheSettings;
  }

  /// Unknown key → `CacheSettings { ttl: Some(Ttl::Forced(-1)), grace: None, keep: None }` (hit-for-miss).
  pub fn select_settings<'h>(table: &PolicyTable, d: &CacheDirective, status: u16,
                             header: impl Fn(&str) -> Option<&'h str>) -> CacheSettings;

  #[derive(Debug, PartialEq)]
  pub enum TtlAction { Leave, Set(f64), HitForMiss }
  /// Returns the action plus whether to strip Set-Cookie/Cache-Control/Expires (positive forced TTL only).
  pub fn plan_ttl(ttl: Option<Ttl>, origin_has_cache_control: bool) -> (TtlAction, bool);
  ```
  Request-side query matching mirrors `match_routes`: if the rule has query conditions and `query` is `None`, the rule doesn't match.

- [ ] **Step 1: Write failing tests** (build policies from `CachePolicy` literals; header closures over a `HashMap<&str,&str>` keyed lowercase):
  - `test_first_match_wins`: rules `[404→forced 1s, 4xx→forced 10s]`, status 404 → `ttl Some(Forced(1))`.
  - `test_no_match_uses_base`: base `Default(300), grace Some(30)`, status 200 → equals base.
  - `test_ttl_falls_back_as_unit`: base `Default(300)`, rule sets only `grace Some(0)` → `ttl Some(Default(300)), grace Some(0)`.
  - `test_grace_keep_fall_back_individually`: base `grace 30, keep 3600`, rule `keep 0` → `grace Some(30), keep Some(0)`.
  - `test_status_range_and_class`: `"500-504"` matches 503 but not 505; `"5xx"` matches 599.
  - `test_and_semantics`: rule with status `404` and response header `content-type` Exact `video/mp2t`. 404 with `text/html` → base.
  - `test_request_mask_bits`: rule0 path Prefix `/live/`, rule1 none, rule2 method `POST`; GET `/live/a` → mask `0b011`.
  - `test_request_mask_query_absent`: rule with query param → bit clear when `query` is `None`.
  - `test_resolve_respects_mask`: rule0 would match the response, but bit 0 is clear → base.
  - `test_policy_key_stable_and_sensitive`: two equal `CachePolicy` values → same key; changing one override's `grace_seconds` → different key.
  - `test_table_rebuild_unchanged_policy_stays_live`: rebuild with the same policy → `get(key)` present, no expiry.
  - `test_table_rebuild_retains_removed`: policy A live, rebuild with only B at `t0` → A still found at `t0 + 299s` (rebuild again at that instant without A, keeps original expiry), gone after a rebuild at `t0 + 301s`.
  - `test_table_rebuild_changed_policy_keeps_old_version`: A then A′ (different grace) → both keys resolvable; old key resolves to old settings.
  - `test_select_settings_unknown_key` → `ttl Some(Forced(-1))`, grace/keep `None`.
  - `test_plan_ttl`: `Forced(5)` → `(Set(5.0), true)`; `Forced(-1)` → `(HitForMiss, false)`; `Default(5)` with CC → `(Leave,false)`, without → `(Set(5.0),false)`; `Default(-1)` with CC → `Leave`, without → `HitForMiss`; `None` → `Leave`.
  - `test_directive_json_roundtrip`.
- [ ] **Step 2:** `cargo test --lib -- --skip vtc_ cache_policy::` → FAIL.
- [ ] **Step 3: Implement** in `cache_policy.rs`. Status entries compile to `(u16,u16)` with `parse_status_match`. Header names compile lowercase (as `HeaderMatchCompiled` already does). The closures receive that lowercase name.
- [ ] **Step 4:** Same command → PASS; `cargo clippy` clean.
- [ ] **Step 5: Commit** `feat(ghost): compile and evaluate cache rule overrides`.

### Task 5: Ghost wiring: recv directive, backend_response, synthetic marker

**Files:**
- Modify: `ghost/src/director.rs`: `RouteEntry.cache_policy: Option<Arc<CompiledCachePolicy>>` (replacing `Option<CachePolicy>`). `build_vhost_directors` compiles each distinct policy once (dedupe by `policy_key`, sharing the `Arc` across routes) and also returns the `Vec<Arc<CompiledCachePolicy>>` of live policies. `GhostDirector` gains `policies: ArcSwap<PolicyTable>`. `reload` stores `PolicyTable::rebuild(&old, live, Instant::now(), POLICY_RETENTION)` **before** swapping `vhost_directors`, so a recv on the new map always finds its key. Add `pub fn cache_settings<'h>(&self, d: &CacheDirective, status: u16, header: impl Fn(&str) -> Option<&'h str>) -> CacheSettings`, which calls `select_settings` on the current table.
- Modify: `ghost/src/vhost_director.rs`: in `route_request`, compute `request_mask` right after `match_routes`, **before** any filter runs. Replace the TTL/grace/keep header code in `apply_cache_policy_headers` with: always `unset_header(CACHE_POLICY_HEADER)` first, then, if not passing, set it to the JSON `CacheDirective { k: policy.key, m: mask }`. Cache-key and bypass logic stay as they are, reading `cache_key` from the compiled policy. `RouteMatchResult.cache_policy` changes type to match.
- Modify: `ghost/src/lib.rs`: declare FFI next to `VRT_r_local_socket`: `VRT_l_beresp_ttl`, `VRT_l_beresp_grace`, `VRT_l_beresp_keep` (`VCL_DURATION`) and `VRT_l_beresp_uncacheable` (`VCL_BOOL`). Check the signatures against Varnish 8 `vrt_obj.h` / the `varnish::ffi` types. Add the method `pub fn backend_response(&self, ctx: &mut Ctx)` on `ghost_backend`, with this behaviour:
  1. If beresp has `SYNTHETIC_HEADER`: unset it, unset bereq `CACHE_POLICY_HEADER`, return.
  2. Read then unset bereq `CACHE_POLICY_HEADER`. If it's missing or the JSON is invalid, return without changes.
  3. `settings = ghost_director.cache_settings(&d, beresp status, beresp header lookup)`.
  4. `plan_ttl(settings.ttl, beresp has Cache-Control)`. `Set(s)` → ttl=s. `HitForMiss` → ttl=`HIT_FOR_MISS_SECONDS`, then uncacheable=true. If the strip flag is set, unset `Set-Cookie`, `Cache-Control` and `Expires` on beresp.
  5. `Some(g)` grace → set; `Some(k)` keep → set.
- Modify: `ghost/src/not_found_backend.rs`, `redirect_backend.rs`, `internal_error_backend.rs`, and the 405 path in `external_backend.rs` (~line 192): `beresp.set_header(SYNTHETIC_HEADER, "1")`.
- Update `ghost/API.md` (it's generated from doc comments; run whatever regenerates it in the build).
- Test: create `ghost/tests/test_cache_rule_overrides.vtc`. Extend `ghost/tests/test_cached_error_race.vtc`. Fix the `RouteEntry` literals in existing unit tests.

**Interfaces:**
- Consumes: Task 4 (`CompiledCachePolicy`, `policy_key`, `PolicyTable`, `CacheDirective`, `select_settings`, `plan_ttl`, constants).
- Produces: VCL method `router.backend_response()` (for Task 6) and the internal headers `X-Ghost-Cache-Policy` and `X-Ghost-Synthetic`.

- [ ] **Step 1: Write the failing VTC** `test_cache_rule_overrides.vtc`. Its VCL mimics the preamble: `vcl_recv` reload handling, `set req.backend_hint = router.recv(); if (req.http.X-Ghost-Pass) { return (pass); }`, and `sub vcl_backend_response { router.backend_response(); }`. Cases, using separate hosts/paths so objects don't collide:
  - (a) Base `default_ttl_seconds 300`; origin 200 with no CC; two requests → server sees 1, `varnish v1 -expect cache_hit >= 1`.
  - (b) Override `{status:["404"], forced_ttl_seconds:1, grace_seconds:0}`, base grace 30. Origin 404 then 200. Request → 404; immediate repeat → 404 from cache; `delay 1.5`; request → **200** (proves grace 0, not a stale 404).
  - (c) Override `{status:["5xx"], forced_ttl_seconds:-1}`. Origin 503, 503, then 200. Three requests → 503, 503 (both reach origin), 200; a fourth request is a cache hit.
  - (d) Response header case: rule `response_headers [{name:"content-type", type:Exact, value:"video/mp2t"}]`, forced 1s. Origin sends `Content-Type: video/mp2t`. After `delay 1.5` the origin is hit again.
  - (e) Route with `url_rewrite` ReplaceFullPath `/internal`, and a rule `path_match Exact /public` with forced -1. Client hits `/public` twice → origin sees 2 requests.
  - (g) Reload mid-fetch: origin `s1` delays its 404 by 1s (`delay 1` before `txresp`). Client `c1` requests in the background (`-start`). Meanwhile, rewrite ghost.json so the policy's override changes (`forced_ttl_seconds` 1 → 2) and reload. After `c1 -wait`, `delay 1.5` and request again → the origin is hit (the recv-time 1s rule applied, not the top-level 300s).
  - (f) Spoofing: route with no `cache_policy`; client sends `X-Ghost-Cache-Policy: {"k":1,"m":1}`. Origin sees both requests (pass), and its `rxreq` asserts `req.http.X-Ghost-Cache-Policy == <undef>`.
  - Extend `test_cached_error_race.vtc`: give the route `cache_policy {forced_ttl_seconds: 300}` and add `vcl_backend_response { router.backend_response(); }`. The 500 must still not be cached and recovery still passes.
- [ ] **Step 2:** `cd ghost && cargo test --release vtc_` → FAIL (`backend_response` unknown).
- [ ] **Step 3: Implement** the changes listed under Files.
- [ ] **Step 4:** `cargo test --release` (unit + VTC) → all PASS; `cargo clippy --release` clean; `cargo fmt --check` clean.
- [ ] **Step 5: Commit** `feat(ghost): apply cache rule overrides in vcl_backend_response`.

### Task 6: Preamble VCL

**Files:**
- Modify: `internal/vcl/preamble.vcl`:
  - In the `vcl_recv` strip list, replace `X-Ghost-Forced-TTL`, `X-Ghost-Default-TTL`, `X-Ghost-Grace` and `X-Ghost-Keep` with `X-Ghost-Cache-Policy`.
  - In `vcl_backend_fetch`, update the comment.
  - In `vcl_backend_response`, replace lines 123–153 (the TTL/grace/keep block and its unsets) with `router.backend_response();`. Keep it after the x-cache-host/url and filter-context lines.
- Modify: `AGENTS.md`, section "Ghost ↔ VCL Communication": swap the four TTL headers for `X-Ghost-Cache-Policy` and add `router.backend_response()`.
- Test: the existing `internal/vcl` tests. If none assert the removed lines, add `TestPreambleCallsBackendResponse` (asserts `router.backend_response();` is present and `X-Ghost-Forced-TTL` is absent).

**Interfaces:**
- Consumes: `router.backend_response()` from Task 5.

- [ ] **Step 1:** Write `TestPreambleCallsBackendResponse` → `go test ./internal/vcl -run TestPreamble` FAIL.
- [ ] **Step 2:** Edit the preamble and AGENTS.md.
- [ ] **Step 3:** `make test` (Go + Rust) → PASS.
- [ ] **Step 4:** If a cluster is available: `make docker` + `make test-conformance` → PASS. If not, say so in the hand-off. Don't claim it.
- [ ] **Step 5: Commit** `feat(vcl): delegate cache TTL decisions to ghost.backend_response`.

### Task 7: Docs and changelog

**Files:**
- Modify: `docs/reference/varnishcachepolicy.md`: add a `cacheRuleOverrides` section (field table, status syntax, first-match-wins, fallback rules incl. TTL-as-unit, negative TTL, max 64 rules, request-side matching uses the pre-filter request, the grace footgun with the live-video example). Fix the `grace`/`keep` defaults: "unset = Varnish default (`default_grace` 10s / `default_keep` 0s); `0s` = disabled". Note that ghost-generated responses are never cached by policy.
- Modify: `docs/concepts/caching-model.md` where it describes TTL resolution.
- Modify: `CHANGELOG.md` `[Unreleased]`: one Added bullet. Two Changed bullets for the behaviour changes: explicit `grace: 0s`/`keep: 0s` now means zero; a top-level `forcedTTL` no longer applies to ghost-generated responses (e.g. the "no backends" 500).

- [ ] **Step 1:** Write the docs.
- [ ] **Step 2:** `mkdocs build --strict` if it's installed (`requirements-docs.txt`). Otherwise check the links by hand.
- [ ] **Step 3: Commit** `docs: document cacheRuleOverrides`.
