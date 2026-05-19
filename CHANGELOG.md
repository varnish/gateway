# Changelog

All notable changes to Varnish Gateway are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.21.0] - Unreleased

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

[v0.21.0]: https://github.com/varnish/gateway/compare/v0.20.0...HEAD
[v0.20.0]: https://github.com/varnish/gateway/releases/tag/v0.20.0
