# Changelog

All notable changes to Varnish Gateway are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
