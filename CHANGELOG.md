# Changelog

All notable changes to Varnish Gateway are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **`cacheRuleOverrides` on `VarnishCachePolicy`.** Per-response TTL, grace
  and keep overrides selected by path, method, request headers, query
  parameters, response status (`404`, `4xx`, `500-504`) and response
  headers. First matching rule wins; unset fields fall back to the
  top-level values. A negative `defaultTTL`/`forcedTTL` means "do not
  cache". See the VarnishCachePolicy reference.

- **`GatewayPort8080` conformance support (#30).** The multi-listener
  architecture already mapped listener ports straight through to Service and
  container ports, so the feature is now declared in the conformance suite.
  This also enables the `HTTPRouteRedirectPortAndScheme` tests.

### Changed

- **Explicit `grace: 0s` / `keep: 0s` now means zero.** Previously an
  explicit zero was treated as unset and Varnish's `default_grace` (10s)
  still applied. Unset still leaves the Varnish defaults. VCPs with an
  explicit `grace: 0s` will no longer serve stale for 10s.
- **A top-level `forcedTTL` no longer applies to ghost-generated
  responses** (e.g. the "no backends" 500). Those responses carry
  `Cache-Control: no-store` and were being cached past recovery.
- **`grace` and `keep` are capped at 10 years** (top level and in rules).
  Larger values used to be accepted by the operator but broke every ghost
  reload on the gateway.
- **Sub-second `defaultTTL`/`forcedTTL` values are now rejected.** Any TTL
  with a magnitude under 1s (for example `500ms`) used to be accepted and
  rounded to whole seconds (`200ms` became 0). It now fails validation. After
  the operator upgrade, a VarnishCachePolicy using such a value is no longer
  Accepted and its routes stop caching. Change those values to 1s or more
  before upgrading.
- **Internal `X-Ghost-*` cache headers no longer reach origins.** The
  per-request cache directive is now taken off the backend request in
  `vcl_backend_fetch` (new `router.backend_fetch()` in the generated VCL)
  instead of after the response arrived.

### Removed

- **Ghost no longer sets the `X-Ghost-Default-TTL`, `X-Ghost-Forced-TTL`,
  `X-Ghost-Grace` and `X-Ghost-Keep` request headers.** User VCL that reads
  them must change: the cache policy values are now applied to `beresp` by
  `router.backend_response()`, so read `beresp.ttl`, `beresp.grace` and
  `beresp.keep` in your `vcl_backend_response` instead. The four headers are
  still stripped from client requests in this release.

### CRD changes

The `VarnishCachePolicy` CRD gains `spec.cacheRuleOverrides` and has updated
field descriptions (including `defaultTTL`/`forcedTTL`, which may now be
negative). Helm does not upgrade CRDs, so apply them before `helm upgrade`:

```bash
helm pull oci://ghcr.io/varnish/charts/varnish-gateway --version vX.Y.Z --untar
kubectl apply -f varnish-gateway/crds/
```

Installs from `deploy/` get the CRDs from `kubectl apply -f deploy/`. Without
the CRD update, clients that do not use strict field validation have
`cacheRuleOverrides` pruned silently by the API server: the policy is
accepted and the overrides never take effect.

### Upgrade notes

During a rolling upgrade to this release, old and new components briefly
coexist:

- New pods reading a `routing.json` written by the old operator see
  `grace_seconds: 0` / `keep_seconds: 0` on every policy (the old operator
  always emitted them), so VCPs that left grace/keep unset get zero grace/keep
  until the new operator rewrites `routing.json` (normally seconds after it
  starts).
- Old pods cannot parse a `routing.json` containing a negative top-level
  `defaultTTL`/`forcedTTL`. The old chaperone retries the failed reload for
  about 3.5s and then exits. The restarted pod cannot load the new
  `main.vcl` (it calls `router.backend_fetch()`, which the old VMOD lacks),
  so it never becomes ready. Every old pod hits this at the same time, so
  they all go down and stay down until replaced by new pods. Do not set a
  negative top-level TTL until the rollout has finished. Negative TTLs in
  `cacheRuleOverrides` alone do not trigger this: the old chaperone drops
  `overrides`.
- Old pods refuse the new generated VCL (`router.backend_fetch()` is unknown
  to the old ghost). The hot reload is rejected, so running pods keep serving
  with their current VCL, but an old pod that restarts mid-rollout will not
  start Varnish until it is replaced by a new one.
- Roll the operator back together with the gateway image, and rebuild any
  custom gateway image (`GatewayClassParameters.spec.image`) before upgrading
  the operator. See [Upgrades](docs/operations/upgrades.md).

### Fixed

- **Redirect port now derives from the Gateway listener, not the `Host`
  header.** A `RequestRedirect` filter with no `scheme` and no `port` was
  building the `Location` port from the client's `Host` header, so a request
  to a listener on a non-default port with a portless `Host` redirected to
  port 80/443. The listener socket name is now authoritative, per spec
  ("if redirect scheme is empty, the redirect port MUST be the Gateway
  Listener port"). Behaviour change for deployments that port-translate in
  front of the Gateway (external LB, gateway-behind-gateway): a portless
  redirect now emits the listener's port rather than the client-facing one.
  Set `requestRedirect.port` explicitly to pin the old value.
- **`kind-metallb.sh` address pool is now inside the kind subnet.** The pool
  was derived from the first two octets and hardcoded `.255.x`, assuming the
  /16 kind usually creates; on a /24 (OrbStack) that produced an off-network
  pool and unroutable LoadBalancer IPs, so every traffic-based conformance
  test timed out locally.
- **RequestRedirect: redirect port when the scheme is unchanged.** A
  `requestRedirect` filter that set `scheme` without `port` only substituted the
  scheme's well-known port when the redirect scheme differed from the request
  scheme. The Gateway API spec assigns the well-known port (http → 80,
  https → 443) for any non-empty redirect scheme. On a listener bound to a
  non-standard port, `scheme: http` with no `port` emitted
  `http://example.org:8080/` instead of `http://example.org/`. A redirect
  scheme with no well-known port now falls back to the listener port, as the
  spec recommends. The conformance suite does not exercise this case.

## [v0.23.0 - 2026-07-24]

### Added

- **Operator: Gateway API inventory and status metrics.** The operator now
  exposes domain metrics on its existing `/metrics` endpoint (port 8080)
  alongside the controller-runtime and client-go metrics: managed Gateway
  count by GatewayClass (`varnish_gateway_gateways`), per-Gateway Accepted /
  Programmed status and listener / attached-route counts
  (`varnish_gateway_gateway_accepted`, `..._programmed`, `..._listeners`,
  `..._attached_routes`), managed HTTPRoute totals
  (`varnish_gateway_httproutes`, `..._httproutes_accepted`), and a build-info
  gauge (`varnish_gateway_info`). Metrics are snapshotted from the controller
  cache at scrape time, so deleted objects drop out with no stale series. No
  new endpoint or ServiceMonitor is required. The Operator Grafana dashboard
  gains an inventory/status row.
- **RequestRedirect/ResponseRedirect: 303, 307, and 308 status codes.** The
  redirect filter now supports 303, 307, and 308 in addition to 301/302, and
  the ghost redirect backend clamps `status_code` to the allowed set.

### Security

- **Strip client-spoofable internal headers at VCL ingress.** `X-Ghost-*` and
  `X-Gateway-*` headers are now unset before `router.recv()` and every ghost
  producer unsets before setting, so clients can no longer forge routing,
  cache-key, or listener/route metadata.
- **Bind operational surfaces to loopback.** The chaperone dashboard binds to
  `127.0.0.1`; pprof/debug moves to a loopback `:6060` server; the
  varnishadm reverse-mode listener and `-M` target bind to `127.0.0.1`.
- **Token-gate `/drain`.** The drain endpoint requires a per-Gateway,
  UID-derived token compared in constant time; CORS wildcard removed and
  SSE-safe HTTP server timeouts added.
- **Harden VCL generation.** `vcl.inline` uses an unguessable per-call heredoc
  delimiter; generated VCL names carry a monotonic counter to avoid
  same-millisecond collisions.

### Fixed

- **AllowedRoutes namespace policy enforced per listener** (namespace +
  hostname intersection on the same listener), for both status and the
  computed socket set. No-intersection and unresolvable `sectionName` no
  longer masquerade as a catch-all; a parentRef filter that resolves to no
  listener now matches nothing instead of failing open to all listeners.
- **parentRef changes prune correctly.** Detaching a route from a Gateway
  regenerates `routing.json` and prunes stale `status.Parents`; old and new
  parents are both enqueued.
- **GatewayClass and infrastructure-hash reconciliation.** A Gateway created
  before its GatewayClass (or a later `parametersRef`) now reconciles instead
  of stalling. Container `Resources`, operator `LogLevel`, and varnishd/logging
  extra args (hashed in user order) are included in the infrastructure hash so
  those edits trigger a rolling restart.
- **BackendTLSPolicy selection and scoping.** GEP-713 precedence now matches
  the Conflicted-status winner; policies are keyed by
  namespace/service/sectionName so a port-scoped policy no longer applies to
  all Service ports; policies with unresolvable CA refs are skipped instead of
  enabling unverifiable backend TLS.
- **VarnishCachePolicy conflict healing and namespace scoping.** Sibling VCPs
  are re-evaluated on every reconcile (including deletes) so a Conflicted loser
  heals when the winner is removed; Gateway/Route-scoped VCPs match only within
  the target's own namespace.
- **Reserved listener ports rejected** (1969/6060/6082/8081/9000) with a
  `PortUnavailable` condition instead of crash-looping the data plane.
- **Weighted routing correctness.** Default `backendRef` weight is 1 per spec;
  `backendTLS` is dropped from the ghost merge key so mixed TLS/plaintext
  backends merge into one route with correct weighted groups; backend-group
  weights are summed as u64 to avoid overflow.
- **Chaperone resilience.** varnishadm no longer strands `Exec` callers on
  connection drop; the drain loop tolerates transient `varnishstat` errors
  (bounded by a consecutive-error cap + deadline); the varnishlog SSE path
  checks `scanner.Err()` and kills the child so an over-long line can't block
  `cmd.Wait()`; TLS reload triggers on the Kubernetes atomic-writer `..data`
  symlink swap. `EventBus.Unsubscribe` no longer closes subscriber channels
  (send-on-closed panic).
- **Cache invalidation.** `VarnishCacheInvalidation` must target a gateway in
  its own namespace; PURGE `404 "Not in cache"` is treated as success
  (idempotent); the processed-UID map is evicted on CR delete to stay bounded.
- **Ghost backend naming.** `sanitize_backend_name` appends a key hash so
  distinct keys can't collapse to one Varnish backend name; `list_json`
  reports the actual route count.
- **Docker/chart packaging.** Dropped a stale `ghost/patches` copy from the
  chaperone image; normalized the default image tag to the v-prefixed
  registry form.

### Changed

- Upgraded the `varnish` Rust crate to 0.7.0 and dropped the local patch.
- Simplified `internal/controller` and enforced `gofmt` in CI.

## [v0.22.0 - 2026-06-04]

### Added

- **GatewayClassParameters: configurable data-plane Service shape (#70).**
  New `spec.service` field exposes Service `type`, `annotations`, `labels`,
  `loadBalancerClass`, `loadBalancerSourceRanges`, and
  `externalTrafficPolicy`. Defaults preserve the existing `Type: LoadBalancer`
  behavior, so existing deployments need no changes. Per-Gateway overlay
  for labels and annotations via the Gateway API v1.1+
  `Gateway.spec.infrastructure` field. Cross-field validation via CEL at
  admission time. Annotations added by cloud controllers (e.g., AWS LB
  Controller, MetalLB) are preserved across reconciles via a managed-keys
  sentinel on the Service. Unblocks gateway-behind-gateway, bare-metal
  MetalLB, and internal-cache deployment patterns. Service shape changes
  do not trigger pod restarts.
- **HTTPRoute `parentRef` port matching.** The implementation now declares
  conformance support for `HTTPRouteParentRefPort`: an HTTPRoute can attach to
  a specific Gateway listener by port via `parentRefs[].port`.

### Docs

- Document supported and unsupported HTTPRoute filters
  (new `docs/reference/httproute-filters.md`).

## [v0.21.5 - 2026-05-26]

### Fixed

- **varnishadm: Exec stranding and orphan execution on connection drop.**
  When the admin connection dropped mid-flight, the pending `Exec`
  caller's `responseChan` was never written to, so `Exec` hung for the
  full `cmdTimeout` (30s default) before returning "command timed out".
  Concurrent callers blocked on the `reqCh` send were stuck
  indefinitely. Worse, requests left in `reqCh` after a disconnect were
  silently executed against the next varnishd connection —
  non-idempotent commands (`vcl.use`, `ban`, `vcl.discard`) would run
  twice while the caller had already been told they failed. The CLI
  client is reworked end-to-end:
  - `handleConnection`'s defer closes the session under lock first
    (newly arriving `Exec`s bail at queue time), then drains `reqCh`,
    delivering a definitive comms error to anything queued. `v.session`
    is left pointing at the closed channel so dead-window callers
    observe the drop instead of capturing nil and stalling until
    `cmdTimeout`.
  - `Exec`'s two-step select shares a single `time.NewTimer` so total
    latency is bounded by one `cmdTimeout` rather than two.
  - On `<-session` or `<-done`, `Exec` does a non-blocking `respCh`
    re-check so a command that actually succeeded just before the close
    isn't racily reported as a failure.
  - New `Server.done` channel closed when `Run()` exits gives `Exec` a
    distinct "server stopped" signal separate from per-connection drops.
  - `processRequest` is extracted with panic recovery so an internal
    panic delivers a result to the in-flight caller instead of stranding
    it.
  - `stateMu` (was `sessionMu`) now also guards banner/version/
    environment so concurrent getters during reconnects can't observe
    torn writes.
  Refs #51.
- **VCL reloader: `lastVCL` only recorded on successful reload.**
  Previously a failed reload would poison dedup and skip retries
  forever. A failed `VCLUse` now best-effort-discards the loaded VCL so
  inactive ones don't leak faster than `garbageCollect` reaps them.
- **TLS reloader: skip `TLSCertRollback` on comms-class errors.**
  Server-side transaction state after a `TLSCertLoad`/`TLSCertCommit`
  comms failure is ambiguous — the commit may have landed before the
  response was lost, so rolling back against the next-connected handler
  would undo unrelated state.

### Docs

- **Logo: neutral grey wordmark for dark-background legibility.**
  Switched the Varnish wordmark paths and "GATEWAY" text to `#808080`
  via a new `.st2` class so the logo stays readable on both light and
  dark surfaces. Dropped the unused PNG copy.

## [v0.21.4 - 2026-05-24]

### Security

- **`golang.org/x/net` bumped to v0.55.0.** Clears two reachable
  vulnerabilities reported by govulncheck (path:
  `internal/k8sutil/slowrequest.go`):
  - `GO-2026-5026` — `idna` fails to reject ASCII-only Punycode labels
    (fixed in x/net v0.55.0).
  - `GO-2026-4918` — HTTP/2 transport infinite loop on bad
    `SETTINGS_MAX_FRAME_SIZE` (fixed in x/net v0.53.0).
  `go mod tidy` carried along the usual x/* indirect bumps; no
  behavioural changes beyond what those modules ship.

### Fixed

- **Chaperone: ghost EndpointSlice watcher guards malformed cache keys.**
  `internal/ghost/watcher.go` now checks the result of
  `strings.SplitN(key, "/", 2)` before indexing `parts[1]`. A
  non-namespaced key would previously have panicked on the slice access;
  nilaway flagged it, and the guard makes the watcher resilient to
  unexpected informer key shapes.

### Internal

- **`make lint` umbrella target.** Adds `vet`, `staticcheck`, `nilaway`,
  and `govulncheck` targets to the Makefile, installing the tools on
  demand into `./bin` (same pattern as setup-envtest). nilaway runs
  without `-include-pkgs` and we grep-filter `vendor/` from the output —
  package filtering was dropping findings whose nil source lives in
  vendored k8s code (e.g. DeepCopy) even when the dereference is ours.
- **CLAUDE.md: release procedure now requires walking
  `git log <last-tag>..HEAD` and mapping each functional commit to a
  changelog bullet** (or justifying its omission). Added after v0.21.2
  shipped with three undocumented commits.
- **CHANGELOG.md: v0.21.2 entries backfilled** for the `LOG_LEVEL`
  feature, `DefaultKeepCount` bump, and chaperone startup-race fix that
  were missing from the original v0.21.2 release notes.

## [v0.21.3 - 2026-05-21]

### Fixed

- **chaperone image build** for `v0.21.2` failed because
  `docker/chaperone.Dockerfile` still tried to `COPY ghost/c_code ./c_code`
  after `ghost/c_code/` was removed in the v0.21.2 ghost refactor (stubs moved
  to `#[cfg(test)] mod test_stubs`). The v0.21.2 Helm chart got published but
  pointed at a chaperone image that was never produced. v0.21.3 drops the
  stale `COPY` and re-publishes a coherent chart + image pair. Skip v0.21.2.

## [v0.21.2 - 2026-05-21]

### Added

- **`LOG_LEVEL` env var for operator and chaperone.** Both binaries read
  `LOG_LEVEL` from the environment (`debug`/`info`/`warn`/`error`,
  case-insensitive); unknown values fall back to `info` with a warning. The
  operator also forwards its own `LOG_LEVEL` into every chaperone pod it
  reconciles, so flipping verbosity on the operator Deployment ripples
  through the data plane. Shared parser lives in `internal/logging`.

### Changed

- **`DefaultKeepCount` raised from 3 to 10.** Keep more loaded VCLs around
  before garbage-collecting, giving more headroom for rollback and
  post-incident inspection.

### Fixed

- **Chaperone: double VCL load race at startup.** The ConfigMap informer's
  `AddFunc` already loaded VCL on initial cache sync, but the startup
  goroutine also called `vclReloader.Reload()` directly. Both ran serially
  through varnishadm and pushed two distinct VCLs into the management
  process before the child was started; when varnishd then pushed both
  into the child, ghost's `vcl_fini` left entries in `vdire->directors`
  and `vcl_KillBackends` asserted, killing the child. Chaperone now waits
  on `vclReloader.Ready()`, which fires after the informer's first
  successful load. (The underlying symbol-shadowing root cause is fixed
  separately below; this removes the unnecessary double load regardless.)
- **Ghost VMOD: VCL discard crash.** `build.rs` was unconditionally linking
  the stubs from `c_code/test_stubs.c` into every build, including the
  production cdylib. Those local strong definitions shadowed the real
  libvarnishd `VRT_DelDirector`, `VRT_delete_backend`, and `VRT_Assign_Backend`
  at `dlopen` time (on macOS via two-level namespace, on Linux because the
  cdylib's internal references were bound at link time and `dlopen` does not
  re-resolve them). Every backend release silently became a no-op, every
  director's refcount only ever grew, and the first `vcl.discard` of any VCL
  tripped `vcl_KillBackends()`:

      Assert error in vcl_KillBackends(), cache/cache_vcl.c line 704:
        Condition(VTAILQ_EMPTY(&vdire->directors)) not true.

  This surfaced in production very early in a pod's life — chaperone's reload
  and probe traffic was enough to cycle through one discard. The cdylib link
  now keeps those three symbols undefined and varnishd resolves them at
  dlopen. The stubs that `cargo test --lib` needs were moved into a
  `#[cfg(test)] mod test_stubs` in `src/lib.rs`. A new
  `ghost/tests/test_vcl_discard.vtc` reproduces the original crash and
  guards against regressions.

## [v0.21.1 - 2026-05-20]

### Changed

- **Varnish base image bumped to 9.0.3.** The chaperone image now pulls
  `varnish` and `varnish-dev` 9.0.3 from packages.varnish-software.com. No
  functional change beyond what Varnish ships in 9.0.3.
- **External proxy backend rejects body-bearing methods with 405.**
  ExternalName routes are proxied through an in-VMOD reqwest client that does
  not yet stream request bodies. The initial implementation logged a warning
  and forwarded body-bearing requests with an empty body, which can succeed
  silently against forgiving upstreams. POST, PUT, PATCH, and DELETE are now
  rejected locally with `405 Method Not Allowed`, `Allow: GET, HEAD, OPTIONS`,
  and `Cache-Control: no-store`. The upstream is not contacted. Body
  forwarding will land in a follow-up.

## [v0.21.0] - 2026-05-19

### Breaking

- **VarnishCachePolicy: removed `spec.requestCoalescing`.** The knob mapped to
  `req.hash_ignore_busy` and was useful only in niche scenarios; flipping it off
  on a slow origin reintroduced exactly the thundering-herd problem Varnish is
  meant to prevent. Users who genuinely need to disable collapsed forwarding can
  set `req.hash_ignore_busy` in user VCL. Existing manifests that set the field
  will fail validation.

### Added

- **Gateway API v1.5.0 conformance.** Upgraded `sigs.k8s.io/gateway-api` to
  v1.5.0; passes the full v1.5.0 conformance suite (258 tests) and is listed
  on the official v1.5 implementations page.
- **ReferenceGrant enforcement for cross-namespace backend refs.** HTTPRoutes
  with backend refs in another namespace now require a matching
  ReferenceGrant; without one, the backend is excluded from `routing.json`
  and the route's `ResolvedRefs` condition is set to `RefNotPermitted`. The
  controller watches ReferenceGrants and re-reconciles affected routes when
  grants are created or deleted.
- **`topologySpreadConstraints` on GatewayClassParameters.** Allows operators
  to spread gateway pods across nodes/zones; changes trigger a rolling
  restart via the infrastructure hash. Plumbed through the bundled CRD.
- **ExternalName Service backends.** HTTPRoutes whose backendRef points at a
  Service of type ExternalName are now proxied through a `reqwest`-backed
  synthetic backend inside the ghost VMOD instead of being silently dropped
  from `ghost.json`. One synthetic Varnish backend is kept per (hostname,
  port, TLS) tuple — reqwest's connection pool and DNS resolver hide
  rotating cloud IPs underneath, so Varnish's per-backend VBE stats stay
  stable across IP rotation. Wire schema gains
  `BackendGroup.external_proxy` (mutually exclusive with `backends`);
  the operator detects ExternalName Services, fills the field, and infers
  TLS from `Service.spec.ports[].appProtocol == "https"`. Request bodies
  are not yet forwarded — non-idempotent methods log a warning in
  varnishlog. Closes the silent-drop bug where `ResolvedRefs=True` was
  reported but the route returned `500 no backends available`.

### Helm chart

A focused round of chart hardening and documentation:

- **`values.schema.json`** added — type and enum errors now surface as
  readable schema messages on `helm install`/`upgrade` instead of opaque
  template render failures.
- **Nil-safe guards** on `serviceMonitor` and `dashboards` blocks so
  `helm upgrade --reuse-values` no longer crashes with a nil-pointer when
  stored values predate them.
- **Varnish version pinning.** `chaperone.varnishVersion` in `values.yaml`
  is now the single source of truth for the Dockerfile build-arg, the
  Makefile, the CI build, and the chart image tag. The version is stamped
  onto the image as `org.opencontainers.image.varnish-version` — test and
  ship cannot drift.
- **`NOTES.txt`** branches on `.Release.IsUpgrade`. On upgrade it prints
  the recommended `--reset-then-reuse-values --force-conflicts`
  incantation, the bundled Varnish version, and a link to the upgrades
  doc.
- **`make helm-test`** (and a CI job) lints the chart and renders it
  against fixtures simulating older value shapes, catching nil-pointer
  regressions automatically.
- **Documentation fixes.** Replaced the non-existent
  `gatewayClass.defaultParams.userVCL.content` keys (the `--set-file`
  example was silently no-op) with the real `userVCL.configMap.{name,key}`
  flow. Added tables for `serviceMonitor`, `dashboards`,
  `gatewayClass.defaultParams.extra*`, `commonLabels`, and
  `commonAnnotations`. Clarified that `chaperone.image` overrides are
  supported but the user owns the operator/chaperone compatibility
  contract (they ship as a matched pair).
- **Chaos test suite (`test/chaos/`).** Nine scenarios (C01–C09) covering
  chaperone/operator pod kills, rapid backend scaling, HTTPRoute churn,
  apiserver partition, node drain, TLS cert rotation, simultaneous changes,
  and a long-horizon soak (C09) for resource-leak and reconcile-regression
  detection. Includes an in-cluster k6 runner, a `suite.sh` orchestrator,
  in-cluster soak as a Kubernetes Job (results on a PVC), per-snapshot
  metrics scraping with linear-regression fitting, pprof captures on
  threshold trips, and a kind setup script for harness dry runs. Run via
  `make chaos-suite`. See `docs/CHAOS-TEST.md`.

### Fixed

- Makefile kind-cluster cleanup now removes all created resources.

### Documentation

- Various accuracy fixes from a spec-vs-code audit: corrected `ghost.json`
  schema (backend groups with group-level weight), ghost reload method (GET,
  not POST), GatewayClassParameters reference (operator-set vs
  chaperone-fallback env values, real `CONFIGMAP_NAME`, overridden
  `VCL_PATH`/`HEALTH_ADDR`), and observability health port.
- VarnishCachePolicy `forcedTTL` docs clarified — `Cache-Control: no-store`
  and `private` responses cannot be overridden because
  `beresp.uncacheable` is write-once-to-true in Varnish.

### Internal

- Cleaned up pre-existing clippy lints in ghost; `cargo clippy --release
  --all-targets` is now clean.
- Excluded `test/` from codecov reports.

## [v0.20.0] - 2026-04-23

First public release of Varnish Gateway.

[v0.21.0]: https://github.com/varnish/gateway/releases/tag/v0.21.0
[v0.20.0]: https://github.com/varnish/gateway/releases/tag/v0.20.0
