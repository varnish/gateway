# Per-HTTPRoute VCL (#94) — implementation notes

Working notes kept alongside the code while pushing the epic through. One
section per phase, newest at the bottom of each. Decisions that the issue left
open are recorded here as *assumptions* so they can be reversed.

## Gate decisions assumed in this branch

| Gate | Decision in #94 | What this branch does |
| --- | --- | --- |
| G1 | Everyone; VCL not inspected | As decided. `GatewayClassParameters.spec.routeVCL.enabled` is the kill switch, default off. |
| G2 | Labels | As decided. One labelled VCL per slot. |
| G3 | Open, waits on #96 | Measured locally (see P1b below). Default and ceiling set from those numbers. |
| G4 | Proposed | **Accepted as proposed.** Source: `VarnishVCLPolicy.spec.vclConfigMapRef` in the policy's own namespace. Delivery: a separate `{gateway}-route-vcl` ConfigMap, one key per slot. |
| G5 | Unassigned | **Compose.** A route VCL is `preamble + route VCL + gateway user VCL + postamble` (order swapped in P8; it was gateway user VCL first, which let a gateway `return` in `vcl_recv` starve the route VCL). Rationale: gateway VCL is where shared normalisation and security headers live, and a route owner must not be able to opt out of them simply by attaching a policy. Cost: a gateway-VCL change recompiles every route VCL. Reversible by changing one function in `internal/vcl`. |

Other assumptions:

- `VarnishVCLPolicy.targetRef` accepts `HTTPRoute` only in this first cut. The
  issue says "Gateway or HTTPRoute"; a Gateway target would duplicate
  `userVCLConfigMapRef` and is left out until someone asks for it.
- `sectionName` on the target is rejected (`Invalid`). The slot is signalled per
  matched route, and a rule-level slot would need ghost to carry rule names
  into the slot map. Possible later; not needed for the goal in #94.
- Cross-namespace ConfigMap references are refused outright (no ReferenceGrant).

## Environment notes

- Local `varnishd` is 9.0.0 (`/usr/local/sbin/varnishd`); the issue's probe ran
  against 9.0.1. Rust toolchain and Go 1.26 present. `wrk` and `k6` available.
- The kubectl context on this machine points at a **production** cluster. No
  phase in this branch touches a cluster; cluster-level verification (P6 "done
  when", P8 conformance/load) is left for a kind cluster or the user.
- Commits on this branch are unsigned by request (hardware key unavailable).
- Target Varnish version is **9.1.0** (`chaperone.varnishVersion` in the chart;
  the image is built from it). The local varnishd used for P1b measurements and
  the `varnishd -C` compile tests is 9.0.0. The mechanism used here
  (`return(vcl(label))`, `vcl.label`, labels always warm, `max_vcl`) predates
  both. P8 must re-run the P1b numbers and the compile tests on 9.1.0 before
  the defaults are called final.

## P2 — API and status (#97)

- `VarnishVCLPolicy` (shortName `vvp`) and `GatewayClassParameters.spec.routeVCL`
  added. Controller sets `Accepted` only; `Programmed` is reserved (P5/P6).
- Check order (first failure wins): Invalid (spec) → TargetNotFound →
  ConfigMapNotFound / Invalid (document > 256 KiB) → Disabled → Conflicted.
- `routeVCL` is deliberately not in the infra hash (reflect test + envtest).
- `zz_generated.deepcopy.go` was hand-written, so `make generate` silently did
  nothing. Added `+kubebuilder:object:generate=true` to `doc.go` and
  regenerated. Side effect: fixes two shallow copies in the old file
  (`GatewayClassParametersSpec.Resources`, `VarnishCachePolicyAncestorStatus.AncestorRef`).
- Operator skips the VVP controller with a warning when the CRD is not
  installed (Helm does not upgrade CRDs).

## P3 — ghost sets X-Ghost-VCL-Slot (#98)

- Wire format: optional integer `vcl_slot` on route objects in routing.json
  (`Route`) and ghost.json (`RouteBackends`); Go `*int` with `omitempty`,
  Rust `Option<u32>` with `serde(default)`. Absent/null = no route VCL.
- `mergeRoutesByMatchCriteria` carries `VCLSlot` from the first route and
  includes it in the merge key.
- Ghost `recv()` sets request header `X-Ghost-VCL-Slot: <decimal slot>` (no
  padding) only when the matched route has a slot (threaded through
  `RouteEntry` -> `RouteMatchResult` -> `RouteRequestResult` like `route_name`).
  The default_backends synthetic route has no slot.
- Preamble `vcl_recv` unsets client-supplied `X-Ghost-VCL-Slot` before ghost
  runs. No dispatch block yet (later phase).
- VTC: `ghost/tests/test_vcl_slot.vtc`.

## Varnish facts probed before P4 (local 9.0.0, scripts in the session scratchpad `p4probe/`)

- `varnishd -C -f master.vcl` **fails** for a master that references labels
  (`Symbol not found: 'gwvcl-7'`). Compile tests must drive a manager process
  (`varnishd -f '' -F -n <dir>`, child never started) and load placeholder,
  labels, then master through the CLI.
- `vcl.load`/`vcl.label`/`vcl.use` all work with no child running.
- A labelled VCL cannot be discarded (`Cannot discard labeled VCL program`),
  cannot be set cold (`A labeled VCL cannot be set cold`), and a label the
  master references cannot be discarded (`other VCLs depend on it`).
- `return (vcl(...))` after a restart fails the request with
  `VCL_Error Illegal return(vcl): Not after restarts` (503). The dispatch
  block must be guarded with `req.restarts == 0` (and `req.esi_level == 0`
  for good measure).
- A VCL that itself contains `return (vcl(gwvcl-N))` compiles and can be
  labelled as long as it does not point at itself (`would create a loop`).
  So a route VCL *can* name another slot's label; the second hop fails at
  runtime. Caveat for P7, acceptable under G1.
- **Latent bug:** `internal/varnishadm.parseVCLList` was written for the
  pre-7.x text format (`auto/warm`). Varnish 9 prints five columns
  (`status state temp busy name`), so every parsed `Name` comes back as
  `"0 <name>"` (`"- <name>"` with no child) and the reloader's
  `garbageCollect` never matches the `vcl_` prefix. P5 switches to
  `vcl.list -j` and fixes this.

## P1b — capacity spike (#96), measured locally

Run on this workstation (16 cores, Varnish 9.0.0, no cgroup limits, loopback
backend), **not in a pod**. Scripts and raw outputs live in the session
scratchpad (`p1b/`); the full report is reproduced in
`docs/development/route-vcl-capacity-spike.md`. Absolute times will be
worse under a pod CPU limit; deltas are the portable numbers.

| Measurement | Result |
| --- | --- |
| One route VCL compile (`vcl.inline`, ~200 lines) | ~220 ms median; ~160 ms fixed (cc fork + vmod load) + ~0.5 ms/line |
| Populated route VCL | +~125 KB child RSS, +~40 VSC stat lines, ~0.22 s compile |
| Unpopulated label | free: ~1 ms to create, no RSS, no stats |
| 64 populated slots | +9.7 MB RSS, +2,580 stat lines, 14 s serial bootstrap |
| 256 populated slots | +32 MB RSS, +10,260 stat lines, 57 s serial bootstrap, 0.6 s master compile |
| Jump cost with a real backend | within noise (<1%) |
| Jump cost, synthetic 900k req/s | 20–30 ns CPU; p50 +~18 µs under saturation |
| 256-entry linear dispatch, last slot | +330 ns/request (-23% synthetic throughput); binary tree is flat |
| `max_vcl` | default 100, warn-only (`max_vcl_handling=1`); **labels do not count** |
| Connection pools | shared across VCLs by endpoint: the jump path opened 0 new backend connections after the master path warmed them, and vice versa |
| varnishadm secret | manager and child both run as uid 1000 locally and in the container (`USER varnish`, no `-j`), so the child can read `$WORK_DIR/secret`. Route VCL can therefore reach the CLI secret; this is within the G1 trust grant and must be in the P8 docs |

**Decisions (G3):**

- Default `routeVCL.slotPoolSize` = **64**. Ceiling = **256** (largest measured).
- The master dispatch is a **balanced binary tree** on `std.integer()` so slot
  position never matters.
- Chaperone sets `max_vcl` to `2 * poolSize + 10` (route VCLs count, labels
  and the master do not matter much; replaced generations linger until
  discarded).
- Compile cost is ~0.2 s CPU per route VCL on this host. P7's throttle should
  assume several times that under the default 100m CPU request, and P8 must
  re-measure in a pod on 9.1.0.

## P4 — VCL generation: master, route VCL, placeholder (#99)

- New in `internal/vcl` (`routevcl.go`): `PlaceholderVCLName`
  (`gwvcl-placeholder`), `DispatchHeader` (`X-Ghost-VCL-Slot`),
  `SlotLabel(n)` (`gwvcl-<n>`, decimal, matches ghost), `GenerateMaster(n,
  gatewayUserVCL)`, `GenerateRouteVCL(gatewayUserVCL, routeVCL)`,
  `GeneratePlaceholder()`. Controller untouched; `Generate()` and the
  two-argument `Merge` call keep working.
- `preamble.vcl` has a sentinel line `    # {{ROUTE_VCL_DISPATCH}}` right
  after `set req.backend_hint = router.recv();`. `generatePreamble` replaces
  it with the dispatch block, or removes the whole line when the pool is 0,
  so `GenerateMaster(0, u) == Merge(Generate(), u)` byte for byte (tested).
- **Feature-off output did change by one line**, as asked: the preamble's
  `vcl_backend_fetch` now also does `unset bereq.http.X-Ghost-VCL-Slot;` (a
  route VCL's ghost sets the header again with no dispatch to consume it).
- Dispatch is a **balanced binary tree** on `std.integer(req.http.X-Ghost-VCL-Slot, -1)`
  (coordinator change after P1b: a 256-entry linear chain cost +330 ns for
  the last slot; the tree is flat). Same tree for every N >= 1, depth
  ceil(log2 N), leaves are `== N) { return (vcl(gwvcl-N)); }`. Outer guard
  `req.http.X-Ghost-VCL-Slot && req.restarts == 0 && req.esi_level == 0`;
  the block ends with `unset req.http.X-Ghost-VCL-Slot;` for out-of-range or
  non-numeric values (they fall through a failing leaf). A restarted or ESI
  request on a slotted route keeps the header on `req` in the master (it is
  still stripped from `bereq` in `vcl_backend_fetch`) and runs master VCL,
  not route VCL. That is a functional limitation for P7 docs: route VCL
  does not apply after `return (restart)` in the master.
- `Merge(generated string, users ...string)`: version from generated only,
  generated imports first, then every user doc's imports deduplicated
  across all documents (previously only against the generated imports, so a
  single user doc with a repeated import now gets it once; harmless), bodies
  in order, postamble once. Empty user docs are skipped.
- Route VCL = `Merge(preamble without dispatch, gatewayUserVCL, routeVCL)`,
  so the trust-boundary unset block is repeated (tested: required because
  Req_Rollback restores client headers on the jump).
- Ghost config path for tests: unexported `generatePreamble/generateMaster/
  generateRouteVCL(path, ...)`. The path is rejected if empty or containing
  `"`, `\`, `\n`, `\r`; production passes `DefaultGhostConfigPath` through
  `mustGeneratePreamble`, which panics only if the constant is edited into
  something unsafe.
- New helper package `internal/varnishtest` (non-test files, for P5):
  `Start(t)` runs `varnishd -n <tmp> -a 127.0.0.1:0 -T 127.0.0.1:0 -S <secret>
  -s malloc,8m -f '' -F -p thread_pool_stack=160k` in its own process group,
  polls `varnishadm ping` (5 s), then `param.set vmod_path
  <repo>/ghost/target/release:<build default>` (overriding vmod_path at
  startup would hide `std`/`directors`). Skips when varnishd/varnishadm are
  missing, `varnishd -V` major < 7, or `libvmod_ghost.so` is not built.
  Methods: `Adm`, `LoadVCL`, `Label`, `Use`, `Start`, `ListenAddr`
  (`debug.listen_address`, fields 2 and 3 joined), `Log`, `WorkDir`;
  `GhostVMODDir()` exported. Cleanup SIGKILLs the process group and polls
  until it is empty: a graceful SIGTERM took the full 5 s timeout with a
  running child.
- `TestGeneratedVCLCompiles` (internal/vcl): placeholder, labels
  `gwvcl-0..63`, master (N=64) with a gateway user VCL, route VCL (own
  `import std; import directors;`, `vcl_recv`, `vcl_backend_response`,
  `vcl_deliver`) labelled `gwvcl-2`; a route VCL with a syntax error fails
  `vcl.load` and `vcl.list -j` still shows master and every label unchanged.
  Then child start and HTTP checks against an httptest backend: slot route
  jumps (`X-Which-VCL: route`, route `vcl_recv`/`vcl_backend_response` ran,
  gateway user VCL ran inside the route VCL, backend never sees
  `X-Ghost-VCL-Slot`); no-slot route stays in master; client-supplied
  `X-Ghost-VCL-Slot: 2` on the no-slot route does not jump; client `0` on
  the slot route is replaced by ghost's `2`; slot 1 (placeholder) answers 503
  with `X-Ghost-Route-VCL: unprogrammed`; slot 999 (outside the pool) stays
  in master. ~1.3 s locally. Mutation check: dropping the new `bereq` unset
  makes the "backend never sees the header" assertions fail.
- **CI skips the varnishd test** (no varnishd in the Go job; verified it
  skips cleanly with varnishd off PATH). **P8 should add a CI job** with
  varnishd 9.1 installed and the ghost VMOD built (`cargo build --release`)
  that runs `go test ./internal/vcl/...` so the compile test actually runs.

## Review of P2–P4 (read-only pass before P5 landed)

Acted on:

- VVP controller no longer writes sibling status from one reconcile
  (`reevaluateSiblings` removed). A second watch on VarnishVCLPolicy maps
  create/retarget/delete to same-target siblings so each policy writes its own
  status with normal retry. `checkConflict` now returns a List error instead
  of failing open to "no conflict", and compares group+kind+name.
- `generatePreamble` errors if the dispatch sentinel is missing while a pool
  size > 0 is requested (a master with no dispatch would silently keep every
  slotted route in the master).
- The preamble gained a `vcl_pipe` that strips every internal `X-Ghost-*`
  header; `vcl_backend_fetch` does not run for pipe.

Accepted as-is, must be documented in P7/P8:

- **Route VCL can poison or purge other routes' cache objects.** The object
  cache is global: a route VCL can set `req.http.host`/`req.url` to another
  route's values, pick its own backend, and have the response cached under the
  other route's key; it can `std.ban()` anything; it can `return (vcl(...))`
  into another slot (second hop fails, but the attempt is not blocked).
  Partitioning by `hash_data(slot)` in the route preamble would separate the
  hash space but break master-side PURGE for route objects; #94 lists
  per-route cache partitioning as a non-goal, so this stays a documented
  consequence of G1.
- The older policy wins a conflict even when it is itself Invalid /
  ConfigMapNotFound / Disabled (parity with VarnishCachePolicy).
- Accepted-condition messages name Gateways/GatewayClasses from the route's
  parentRefs, including ones in other namespaces. HTTPRoute status already
  exposes parent acceptance, so this adds little; left as is.
- Gateway user VCL is compiled into every route VCL (compose), so gateway
  level `vcl_init` objects, probes and backends are instantiated once per
  populated slot. P7 doc line.

## P5 — chaperone multi-VCL lifecycle and label pool (#100)

### Operator -> chaperone contract (as implemented, `internal/vcl/routevcl_contract.go`)

- `{gateway}-vcl` ConfigMap: `MainVCLKey` (`main.vcl`) = `GenerateMaster(N, gatewayUserVCL)`
  and `PoolSizeKey` (`route-vcl-pool-size`) = decimal N, written in the same
  update. Absent, empty or `"0"` = feature off (master-only, today's
  behaviour). Non-numeric, negative or > 1024 (sanity cap, operator ceiling is
  256) is logged as an error and the ConfigMap version is ignored.
- `{gateway}-route-vcl` ConfigMap (`GATEWAY_NAME + RouteVCLConfigMapSuffix`;
  chaperone env `ROUTE_VCL_CONFIGMAP_NAME` overrides; empty when
  `GATEWAY_NAME` is unset = never watched): `ManifestKey` (`manifest.json`) =
  `vcl.RouteVCLManifest{Slots: map["<slot>"]RouteVCLSlot{Policy, Generation, Hash}}`
  and `SlotKey(n)` (`slot-<n>.vcl`) = the full `GenerateRouteVCL` document.
  Missing ConfigMap = empty manifest. **P6 must compute `Hash` with
  `vcl.RouteVCLHash(doc)`** (lowercase hex SHA-256 of the document bytes): the
  chaperone verifies it and refuses a slot whose document does not match.
- `ParseRouteVCLManifest` / `ParsePoolSize` are exported for P6 tests.
- Manifest entries the chaperone skips (reported `programmed:false` with a
  message, label untouched): slot key not a canonical non-negative integer
  (reported with `slot: -1`), slot >= N, hash not 64 lowercase hex, document
  key missing, hash mismatch. A manifest that is not valid JSON changes
  nothing (logged) and still lets `SlotsReady` close.

### Reconciler (`internal/vcl/reloader.go`)

- One goroutine. The two informers (field-selected to one ConfigMap each)
  only store the latest object (route CM delete stores nil, vcl CM delete is
  ignored) and nudge a 1-buffered channel. Failed reconciles retry after
  5 s doubling to 30 s, plus every event and the 30 s informer resync.
- Every reconcile starts from `vcl.list -j` (`VarnishadmInterface.VCLListJSON`,
  parser `varnishadm.ParseVCLListJSON`); the text parser, `VCLListStructured`
  and `VCLListResult` are gone. Order: (1) if N > 0: `param.set max_vcl 2N+10`
  (only when the value changes), load the placeholder if absent, label every
  missing `gwvcl-i` to it (existing labels never moved here); (2) master
  `vcl.inline vcl_<ts>_<ms>_<seq6>` + `vcl.use` when main.vcl differs from
  the last applied one or no `vcl_*` is active; compile failure = previous
  master stays, reconcile returns an error (retried with backoff); (3)
  discard inactive masters beyond `DefaultKeepCount` (now **2**), or all of
  them when labels >= N exist; (4) shrink: for each `gwvcl-i`, i >= N, repoint
  to placeholder, discard its route VCL, discard the label (refusal = info,
  next reconcile); N == 0 also discards the placeholder; (5) slot pass; (6)
  discard orphan `route_*` VCLs (no label).
- Order deviation from the brief's test list: bootstrap is
  `max_vcl, placeholder, labels, master, use` (C.1's order), asserted in
  `TestReconcile_BootstrapOrder`.
- Route VCL load name `route_<slot>_<hash[:12]>`; if that name is still
  taken by a discarded-but-busy VCL, `_2`, `_3`, ... is appended. A label
  pointing at the base name or any `_k` variant counts as programmed.
- Compile (or label) failures are memoised per load name in memory: the same
  document is not recompiled on every resync (it cannot succeed); the slot is
  reported with the memoised message. The memo forgets names no longer in the
  manifest and is lost on restart. A transport error (CLI unreachable) is not
  memoised; it fails the reconcile.
- `Ready()` = first master active. `SlotsReady()` = first full slot pass
  after that (closed immediately with N == 0 / no route CM / bad manifest;
  a compile failure does not hold it). `cmd/chaperone` waits on both before
  `start` and logs the slot wait.
- Dashboard: route VCL loads/failures reuse `PublishVCLReload` /
  `PublishVCLReloadFail` (so `chaperone_vcl_reloads_total` and its failure
  counter now include route VCLs; P7 may want separate counters).
- `New()` lost the `vclPath` argument (file-based `Reload()`/`ReloadInline()`
  are gone; nothing used them). Chaperone no longer reads `VCL_PATH`; the
  operator still sets it in `resources.go` (left alone so this phase does not
  roll every gateway; P6 can drop it with its own pod template change). Docs
  table updated.

### Per-pod status (`internal/vcl/status.go`)

- `VarnishVCLPolicyStatus.PodResults []VarnishVCLPolicyPodResult`
  (`+listType=map`, `+listMapKey=podName`): `podName, slot, hash, programmed,
  message?, lastUpdate`. One entry per pod per policy.
- Written by a separate goroutine (latest-wins hand-off from the reconciler,
  so the API server never delays VCL work): dynamic client, GVR
  `vcl.VarnishVCLPolicyGVR`, get / replace-own-entry / UpdateStatus, up to 5
  attempts on conflict, skipped when the stored entry already matches
  (slot/hash/programmed/message), failures retried every 10 s. A missing
  policy or a `policy` that is not `<ns>/<name>` is skipped (logged).
- RBAC: `varnishvclpolicies` get + `varnishvclpolicies/status`
  get/update/patch in `deploy/02-chaperone-rbac.yaml` and the chart's
  `chaperone-rbac.yaml`. `internal/controller/resources.go` has no per-gateway
  role (it binds the shared ClusterRole `varnish-gateway-chaperone`), so
  nothing to change there or in `resources_test.go`.
- The VVP controller writes `Status.Ancestors` on the fetched object, which
  keeps `podResults` intact. **P6 must keep doing that** (never assign a
  fresh `VarnishVCLPolicyStatus{}`), and decide on GC of entries for pods
  that no longer exist and for gateways the policy left (the chaperone only
  forgets policies that leave its manifest; it never deletes their entry).
  `Programmed` should be derived from podResults whose `hash` equals the
  current manifest hash.

### Varnish facts learned in P5 (9.0.0, real varnishd)

- `vcl.list -j` entries: `status` (`active`/`available`/`discarded`),
  `state` (`auto`/`cold`/`warm`/`label`), `temperature`, `busy` (absent
  without a child), `name`, `labels` (absent when 0), and for labels
  `label: {name, refs}` (`refs` = VCLs that `return(vcl)` to it, absent when
  0). A failed `vcl.load` exits varnishadm with `Command failed with error
  code 106`; refusals (`Cannot discard labeled VCL program`, `... other VCLs
  depend on it`) are 300; unknown name is 106.
- **A discarded master stays listed** (`status: discarded`, `state: cold`,
  `temperature: busy`, `busy: N`) while idle keep-alive sessions still hold
  it; its label refs still show in `label.refs` but do **not** block
  discarding those labels. Discarded route VCLs were freed immediately in the
  test. The reconciler treats `discarded` entries as gone except that their
  names stay taken (hence the `_k` rename).
- `internal/varnishtest`: `Start` sets `Pdeathsig: SIGKILL`; `v.CLI()`
  implements `VarnishadmInterface` over the `varnishadm` tool (temp file +
  `vcl.load` for `VCLInline`, status codes parsed from its stderr). Only
  tests import the package.

### Tests

- Unit (`reloader_test.go`, strict `MockVarnishadm` that models varnishd's
  VCL state, refusals, label refs and busy-discarded VCLs; `status_test.go`
  with `k8s.io/client-go/dynamic/fake`, newly vendored).
- `TestReloaderAgainstVarnishd`: attach/change/move/detach, grow 4->8 with
  slot 7, shrink 8->2, syntax error in slot 0 beside a working slot 1, all
  checked via `vcl.list -j`, HTTP through ghost, and the fake dynamic
  client's VVP status. ~2.4 s. Skips like P4's test.

### For P6

- Write both `main.vcl` and `route-vcl-pool-size` in one update; never a
  master for N without the key. Changing N recompiles the master.
- Route VCL documents depend on the gateway user VCL (G5 compose), so a
  gateway VCL change changes every hash and recompiles every slot
  (~0.2 s each, serial, on every pod).
- Slot numbers in routing.json (ghost) and the manifest are independent
  writes; a slotted route whose VCL is not yet labelled answers 503 from the
  placeholder until the slot pass lands (by design; F in #100).
- A pod restart waits for every slot to compile before starting the child:
  64 populated slots ~14 s on this host, more under a CPU limit. Readiness
  probe timing may need a look.

### P5 review fixes

- **Failure memo** only holds genuine VCC failures (`vcl.inline` payload
  contains `VCC-compiler` or `VCL compilation failed`, which is what
  varnishd 9 prints and what the real-varnishd test sees). Any other
  `vcl.inline` refusal (max_vcl, ENOMEM, child-side failure) and every
  `vcl.label` refusal is not memoised: the slot reports the message, the
  reconcile returns an error so the 5-30 s backoff retries it, and
  `SlotsReady` is not held. A refused label keeps the compiled VCL loaded
  (`discardOrphans` now spares loads of wanted documents) and the next pass
  only retries `vcl.label` (`loadedAs`). Memo entries expire after 5 min
  (`failureMemoTTL`; `Reloader.now`/`memoTTL` are injectable) and the route
  memo is cleared when a master is applied or the pool size changes.
- **Failing master** is memoised by main.vcl content under the same TTL (no
  recompile / `PublishVCLReloadFail` every retry; a changed main.vcl is tried
  at once). The pass then continues: slots below
  `min(pool, activeMasterPool)` are reconciled (`activeMasterPool` = pool of
  the last applied master, 0 before the first), slots from there up to pool
  report "waits for the master VCL ..."; old-master discard and shrink are
  skipped; the master error is returned at the end. With no master active at
  all the reconcile still returns early and `SlotsReady` stays open.
- **Invalid pool size** before any master was applied: logged, main.vcl is
  loaded as pool 0 (so a pool-0 main.vcl comes up; one generated for a pool
  fails to compile and says so). Once a master is applied the ConfigMap is
  ignored as before. `cmd/chaperone` logs "waiting for route VCL slot pass"
  with the elapsed time every 10 s.
- **Placeholder** is loaded under `freeName(gwvcl-placeholder)`, i.e.
  `gwvcl-placeholder_2`, ... while a discarded-but-busy placeholder still
  holds the name (chosen up front from `vcl.list` rather than after a failed
  `vcl.inline`, same as route VCLs). Labels point at the live one;
  `isPlaceholderName` is used wherever the slot/shrink code compared with
  `PlaceholderVCLName`; pool 0 discards every unlabelled placeholder load.
- **Duplicate policy** in the manifest: one slot per policy per gateway is
  the contract (operator enforces it). If it happens anyway the chaperone
  logs a warning and reports programmed over valid-but-failed over invalid
  (ties: first in slot/key order).
- **Status writer**: the in-memory "written" cache is gone. Every flush runs
  `write()` (GET + compare + UpdateStatus only on difference) for every
  entry; flushes run when the results change (an identical 30 s reconcile no
  longer nudges), after a failure (10 s) and every 60 s (`statusResyncPeriod`),
  so a recreated policy or one whose podResults were dropped gets its entry
  back. Each entry is re-read under the lock right before its write and
  skipped if superseded. A policy that leaves this pod's results gets one
  final entry `{slot: <last slot>, hash: "", programmed: false, message:
  "not in manifest on this pod"}` (chosen over deleting the entry: same
  get/UpdateStatus path, and the operator still sees which pods had it). P6
  GC can treat such entries as stale.
- Mock: strict mode refuses `vcl.label L T` with status 300 ("would create a
  loop") when T, followed through `return (vcl(...))` labels, returns to L.

## P6 — operator wiring and stable slot assignment (#101)

### Contract additions (`internal/vcl/routevcl_contract.go`)

- `RouteVCLSlot.Route` (`<ns>/<name>` of the target HTTPRoute) and
  `RouteVCLManifest.Unassigned []RouteVCLUnassigned{Policy, Route, Reason}`
  with reasons `UnassignedNoSlotAvailable` / `UnassignedSlotOutsidePool` /
  `UnassignedConfigMapFull`. `ParseRouteVCLManifest` is plain
  `json.Unmarshal`, so unknown fields are ignored; tested in the chaperone
  package (`TestReconcile_ManifestWithOperatorFields`: route, unassigned and
  invented fields parse and the slot is programmed). **Note:** that test got
  swept into commit 8955105 (P5 review fixes) while P6 was uncommitted, so
  HEAD's `internal/vcl` tests only compile together with this phase's
  contract change.

### Gateway controller (`routevcl_slots.go`, `gateway_controller.go`)

- Pool size N = `routeVCLPoolSize(params)`: 0 unless `routeVCL.enabled`,
  else `slotPoolSize` or 64, clamped to 256.
- `main.vcl` = `vcl.GenerateMaster(N, userVCL)` and `route-vcl-pool-size` =
  N, **always written, "0" when off** (choice: explicit beats absent; the
  chaperone treats both the same). The `{gateway}-vcl` branch of
  `reconcileResource` compares/updates both keys (`gatewayOwnedVCLKeys`) in
  one Update and keeps `routing.json`. N == 0 output is byte-identical to
  `Merge(Generate(), userVCL)` (tested). `generateVCL` now does the same as
  the reconcile path. On operator upgrade every existing `{gateway}-vcl`
  gains the `"0"` key (one hot reload; main.vcl also differs by P4's one
  extra `unset` line).
- `{gateway}-route-vcl` is reconciled by its own function after the generic
  resource loop (not through `reconcileResource`): the previous manifest is
  read from the same object that gets updated, so a stale cache read fails
  with a conflict (requeue) instead of overwriting a newer assignment. It is
  always created (empty manifest when N == 0), carries the gateway labels
  (so `enqueueGatewaysForConfigMap` skips it) and a controller reference.
  Updated only when `Data` differs; the whole `Data` is replaced, so stale
  `slot-N.vcl` keys vanish.
- Eligibility: cluster-wide VVP list; `Accepted=True` in ancestors[0] under
  our controller name with `observedGeneration == generation`; target
  `HTTPRoute` in the policy namespace whose parentRefs include this Gateway
  (`routeHasParentGateway`); document present now (missing ConfigMap/key or
  > 256 KiB = left out this round). Two eligible policies on one route
  (stale status during a conflict): only the older is kept. CRD not
  installed (NoMatch) = no candidates.
- `assignRouteVCLSlots` (pure): candidates sorted by creation timestamp then
  `ns/name`; pass 1 keeps every previous slot `< N`; pass 2 gives everyone
  else the lowest free slot (freed slots reusable after retained ones).
  **Interpretation of "shrink below an assigned slot"**: the policy loses
  that slot and is treated like a new one in the same pass, i.e. it gets a
  free slot inside the smaller pool if there is one; only when none is free
  is it Unassigned `SlotOutsidePool` (literal "always unassigned" would flip
  to a slot on the next reconcile and break determinism). The reason is
  sticky while it waits (carried via the previous manifest's Unassigned
  list), so the manifest is a fixed point.
- Size guard: each document costs `len(key)+len(composed doc)+names+160`;
  over 900 KiB the policy is Unassigned `ConfigMapFull` and consumes no slot.
  Checked per document (a smaller later one can still fit) rather than
  "stop at the first overflow"; a retained policy whose document grows past
  the guard loses its slot. The Unassigned list is not counted.
- Warn once: `newlyUnassigned` = Unassigned entries not present (same
  policy+reason) in the previous manifest; logged at warn. Stateless, so an
  operator restart does not re-log.
- Watches: VVP (map: policy -> parent Gateways of its target route, old and
  new object, deletes) with predicate `vvpSlotRelevantChange` (generation or
  Accepted-current flips; podResults churn filtered). Added only when the
  VVP CRD is served (`varnishVCLPolicyCRDInstalled`), else a warning.
  `enqueueGatewaysForConfigMap` also maps a ConfigMap to the Gateways of
  every same-namespace VVP referencing it.

### Pod template (`resources.go`)

- `VCL_PATH` dropped; `startupProbe` HTTP `/health`, period 2 s,
  failureThreshold 90 (180 s). Neither is in the infra hash and
  `needsDeploymentUpdate` only copies the template on image or infra-hash
  change, so existing gateways pick them up at the next image bump (every
  operator release bumps the image) or infra change, not on their own. No
  test pinned the env list.

### HTTPRoute controller

- `attachRouteVCLSlots` after the other attach steps: reads
  `{gateway}-route-vcl`, sets `VCLSlot` where `RouteName == slot.Route`.
  Missing ConfigMap = no slots. **An unparseable manifest fails
  `updateConfigMap`** (retried) instead of dropping every slot: silently
  routing slotted traffic through the master could skip auth-style route VCL.
  The Gateway controller rewrites a bad manifest from scratch on its next
  reconcile.
- Watch: ConfigMaps passing `isRouteVCLConfigMap` (suffix + managed-by
  label) -> routes named in Slots and Unassigned (old and new object).

### VVP controller (`varnishvclpolicy_programmed.go`)

- `status.slots []{gateway "ns/name", slot}` (`listType=map`,
  `listMapKey=gateway`), printcolumn `Programmed`, reason constants
  `Programmed`, `Pending`, `CompileFailed`, `NoSlotAvailable`,
  `NotProgrammed`, `NotAccepted`.
- Per parent Gateway of our class with route VCL on: slot -> Programmed when
  every live pod has an entry with the manifest hash and `programmed`;
  CompileFailed when a live pod reports the manifest hash with
  `programmed=false` (first pod by name, message = its message);
  Pending otherwise (including zero live pods); Unassigned ->
  NoSlotAvailable (message = reason); not listed -> NotProgrammed.
  Aggregate: all programmed -> True; else CompileFailed > NoSlotAvailable >
  Pending; NotProgrammed (Unknown) only when no Gateway is programmed and
  none is in a worse state (with some programmed it becomes Pending).
  Message lists each contributing Gateway, capped at 8 KiB. Not accepted ->
  False/NotAccepted, slots cleared. No enabled parent -> Unknown/NotProgrammed.
- Live pods: `List(Pods, ns, MatchingLabels(gatewayLabels(gw)))` without
  `deletionTimestamp`, phase not Succeeded/Failed, for every parent Gateway
  of our class (enabled or not, so GC does not drop entries of a Gateway
  whose class just toggled the feature off). P5's final
  `"not in manifest on this pod"` entries have `hash: ""` and so count as
  stale (Pending), never as CompileFailed.
- GC: entries whose pod is not live on any parent Gateway are dropped in the
  same status write; entries of live pods are kept even when stale.
- Write guard covers Accepted, Programmed, Slots and the GC; conflict on
  `Status().Update` (a chaperone wrote) -> `RequeueAfter: 1s`.
- Triggers: `For` uses `vvpGenerationOrPodResultsChanged` (generation or
  `status.podResults` changed; sibling watch keeps
  GenerationChangedPredicate); `findVVPsForConfigMap` maps a route VCL
  ConfigMap to every policy in its manifest (any namespace); Deployment
  watch (operator-labelled gateway Deployments) -> policies in that
  Gateway's manifest. **No Pod watch.** Pods are listed through the
  manager cache, which `cmd/operator` now restricts with
  `cache.Options.ByObject{&corev1.Pod{}: label managed-by=varnish-gateway-operator}`,
  so the Pod informer only holds gateway pods. Operator ClusterRole gained
  `pods get/list/watch` (chart and `deploy/01-operator.yaml`).

### Metrics

- `varnish_gateway_route_vcl_slots{gateway_namespace,gateway_name,state=used|free}`
  (used = manifest slots `< N`) and `varnish_gateway_route_vcl_unassigned`,
  emitted for every managed Gateway (zeros when off or ConfigMaps missing).
  This is the exhaustion metric; there is no separate counter.

### Not done / deviations

- vclparser validation skipped (`github.com/perbu/vclparser` not vendored);
  varnishd in the chaperone stays authoritative and its errors surface as
  `CompileFailed`.
- Cluster-level "done when" from #101 not exercised (no kind cluster used;
  the local kube context is production).

### Tests

- Unit: `routevcl_slots_test.go` (pool size; stability across reconciles,
  document changes and creation-order changes; release/reuse with retained
  first; exhaustion + warn-once; shrink with full pool (sticky
  SlotOutsidePool) and with a free slot; composition + hash; pool 0;
  ConfigMap full; Gateway reconcile on/off with main.vcl + pool key, single
  write on resize, eligibility filters incl. cross-namespace route and
  duplicate route, stale slot keys removed; map funcs; predicate),
  `routevcl_httproute_test.go` (vcl_slot set, cleared, no ConfigMap, bad
  manifest is an error, ConfigMap -> routes), `varnishvclpolicy_programmed_test.go`
  (every reason, GC, no-op second reconcile per case, terminating/failed/
  other-gateway pods, multi-gateway aggregation, disabled parent, predicate,
  ConfigMap/Deployment maps), metrics `TestGatewayCollector_RouteVCL`,
  resources (no VCL_PATH, startup probe, pool key "0").
- Envtest `TestRouteVCL_EndToEnd_Envtest`: VVP Accepted -> Gateway reconcile
  creates `{gw}-route-vcl` (slot 0, hash, owner) and `{gw}-vcl` with pool 4
  and dispatch -> HTTPRoute reconcile gives `"vcl_slot": 0` -> pod without
  results = Pending + status.slots -> podResults write = Programmed True and
  a gone pod's entry GCed.

### For P7/P8

- A compile failure on a new slot leaves the placeholder (503) in place for
  that route on that pod; docs say so. Consider whether the HTTPRoute
  controller should withhold `vcl_slot` until Programmed (it does not now).
- Programmed follows the Deployment status for pod churn; a pod that dies
  without the Deployment status changing (rare) is only noticed on the next
  podResults/ConfigMap/Deployment event.
- Stale `"not in manifest"` entries for live pods stay until the pod goes.
- Size guard is per Gateway ConfigMap; at 256 slots x 256 KiB documents the
  900 KiB guard is the real limit, which the docs state.
- Re-measure startup probe headroom (180 s) on 9.1.0 in a pod with the
  default 100m CPU request: 256 populated slots took ~57 s unthrottled.

### P6 review fixes

- **Two-phase slot release.** `RouteVCLManifest.Released
  []RouteVCLReleased{Slot, Policy, Route, Hash, ReleasedAt metav1.Time}`
  (`released`, omitempty; the chaperone ignores it, its parse test still
  passes). Deviation from the brief: `Route` was added, it is needed for the
  "same policy and route gets its slot back" rule. Any previous slot whose
  occupant did not keep it (deleted, not eligible, retargeted, moved by a pool
  shrink, ConfigMapFull) is not free: it is listed in Released with
  `ReleasedAt = now` (RFC 3339, seconds), has no `slot-N.vcl` and is not in
  Slots, so the chaperone points the label at the placeholder and
  routing.json drops the slot (fail-closed 503). It becomes free when both
  `routeVCLReleaseQuarantine` (60 s) has passed and the previous policy is
  gone or none of its podResults for a live pod of this Gateway reports that
  slot `programmed: true`. **The pod check ignores the hash** (stricter than
  the brief): a pod still reporting the slot programmed with an older hash of
  the same policy still has the label on that policy's VCL; the chaperone's
  final `{hash: "", programmed: false}` entry clears it. A policy coming back
  with the same policy and route while its slot is Released gets it back at
  once (pass 1b, before new placements); a different route is treated as new.
  `assignRouteVCLSlots` takes `routeVCLRelease{now, cleared map[slot]bool}`
  and returns `requeueAfter` (remaining quarantine, else
  `routeVCLReleasePollInterval` = 10 s while pods still report the slot,
  since podResults writes are filtered from the Gateway controller's VVP
  watch). `reconcileResources` now returns `(time.Duration, error)` and
  `Reconcile` returns it as `RequeueAfter`. Clock: unexported
  `GatewayReconciler.clock` (nil = `time.Now`). Pool 0 still writes an empty
  manifest (Released dropped too; the chaperone discards all labels).
- **podResults GC vs release.** The VVP controller's GC now also keeps
  entries of live pods of Gateways whose manifest lists the policy in Slots
  or Released (`manifestHolderGateways`, a List of operator-labelled
  ConfigMaps). Without it a retargeted policy's entries on the old Gateway
  were GCed at once and the release check saw "cleared" while the pod still
  had the label. `manifestPolicyKeys` includes Released, so manifest changes
  re-reconcile those policies.
- **Status lag.** `vvpAcceptance` is tri-state (accepted / rejected / unknown
  for the current generation). A policy holding a slot in the previous
  manifest stays a candidate unless rejected at the current generation;
  others need accepted-current. Composed from the live spec. If a holder's
  ConfigMap/key is missing (or oversized) right now, its previous composed
  `slot-N.vcl` bytes and manifest generation are kept (warn); without a
  previous document the slot is released. `vvpSlotRelevantChange` compares
  the tri-state, so "unknown -> rejected" after a spec edit triggers the
  Gateway.
- **Programmed** matches pod entries on slot AND hash.
- **User VCL read errors**: `userVCLFromParams` returns `(string, error)`;
  non-NotFound Get errors fail the reconcile. Missing ConfigMap or key stays
  "" with a log line. Same fix applied to the GatewayClassParameters lookup in
  `reconcileResources` (previously an API error gave nil params, i.e. pool 0
  and an infra hash without the class settings, which would also roll the
  pods). `getUserVCL`/`generateVCL` (test-only helpers) still log and return
  "".
- **Attachment.** `routeAcceptedByGateway(route, gwKey)`: spec parentRef
  present AND a `status.parents` entry for that Gateway with our controller
  name and `Accepted=True`. Required for slot candidates (new and retained)
  and in the VVP controller's enablement: enabled-but-unattached parents give
  `Accepted=False` reason `RouteNotAttached` (new constant
  `VCLPolicyReasonRouteNotAttached`, documented). The VVP controller's
  HTTPRoute watch now passes `status.parents` changes
  (`routeGenerationOrParentsChanged`); the Gateway controller already watched
  HTTPRoutes unfiltered. Accepted message wording changed to "route VCL does
  not apply on some parent Gateways: ..." (it now also lists unattached ones).
- **Corrupt manifest**: a non-empty, unparseable `manifest.json` fails the
  Gateway reconcile (error logged with "delete the ConfigMap to start over")
  instead of reassigning from scratch. Empty/missing = fresh start. This
  supersedes the P6 note that "the Gateway controller rewrites a bad manifest
  from scratch": both controllers now wedge on it until a human acts.
- Tests: release not reused in the same round, reused after quarantine when
  cleared, held while a pod reports it (poll requeue), Gateway reconcile
  following podResults (live pod programmed holds; gone pod and final entry
  do not), deleted policy freed after quarantine only, returning policy gets
  its slot (same route) but not on another route, exhaustion waits for the
  release; status lag keeps the slot and uses the new document, rejection
  releases it; holder keeps previous document when its ConfigMap vanishes,
  new policy is left out; unattached / refused routes excluded; corrupt
  manifest is an error and left untouched; user VCL Get error fails without
  rewriting main.vcl or slot documents, missing user VCL ConfigMap is "";
  Programmed with a same-hash entry for an old slot (neither programmed nor
  failure counts); GC keeps a released holder's pods; predicates. Envtest:
  RouteNotAttached before the HTTPRoute controller attaches the route, and
  delete -> Released + requeue + `vcl_slot` gone from routing.json.
- Open: `varnish_gateway_route_vcl_slots{state="free"}` (internal/metrics,
  not touched here) counts released slots as free although they are not
  assignable yet. A deleted-and-recreated `{gateway}-route-vcl` ConfigMap
  still starts fresh without quarantine (pods may hold labels from before).

## P7 — throttling, metrics, dashboard, caveats (#102)

### Compile throttle (`internal/vcl/throttle.go`)

- `golang.org/x/time/rate` was already vendored (direct dependency); no new
  dependency. `compileThrottle` wraps a `rate.Limiter` and uses `ReserveN` with
  an injectable clock and sleep, so tests run on a virtual clock. Waiting
  honours ctx (the reservation is returned on cancel); a cancelled wait fails
  the reconcile with the context error.
- Only the route VCL `vcl.inline` in `reconcileSlots` waits. The master and the
  placeholder never do (tested). `Reloader.SetCompileThrottle(rate, burst)`;
  `New()` leaves the throttle off (so unit tests stay fast), the chaperone
  applies `ROUTE_VCL_COMPILE_RATE` (default 4, `0` = off) and
  `ROUTE_VCL_COMPILE_BURST` (default 8; < 1 raised to 1 while rate > 0).
  Invalid env values fail `loadConfig`.
- Honest arithmetic: the bucket starts full and refills at 4/s while a ~0.2 s
  compile runs, so each compile costs a net 0.2 tokens. The burst of 8 lasts
  ~40 compiles at local speed, after which compiles are spaced 0.25 s. At local
  speed the default therefore hardly slows anything: 64 slots ~14 s,
  256 slots ~60 s (vs 12.8 s / 57 s unthrottled). Under a CPU limit, where a
  compile takes > 0.25 s, the default never binds at all. It is a ceiling on
  compile rate (about 80% of a core at 0.2 s/compile), not a CPU cap; lowering
  `ROUTE_VCL_COMPILE_RATE` (1 ~ 20% of a core) is how an operator protects
  traffic during a gateway-VCL change.

### Events and metrics

- Route VCL loads publish `route_vcl_reload` / `route_vcl_reload_fail` (new
  `PublishRouteVCLReload(bus, slot, name, compile)` and
  `PublishRouteVCLReloadFail(bus, slot, err, result, compile)` in
  `internal/dashboard/routevcl.go`; signatures carry a compile duration and a
  result because the metrics need them). The master still uses
  `PublishVCLReload`/`Fail` and the old counters.
- `compiles_total{result}` is counted when the slot is labelled (`ok`) or fails
  (`compile_error` = VCC, `error` = other refusal or failed `vcl.label`). A
  compile that succeeds and is then retried at the label step counts `ok` once,
  when the label lands, and observes its duration only then.
- The histogram observes `vcl.inline` durations including failed compiles.
- `chaperone_route_vcl_master_reloads_total{result=ok|error}` is derived from
  the master's reload events (error does not distinguish compile from other).
- Slot gauges come from a new `route_vcl_slots` event carrying a JSON
  `RouteVCLState` snapshot (pool, master VCL name, populated slots). Published
  after a slot pass only when it differs from the last one. Gauge states:
  programmed, failed (every populated slot that is not programmed), placeholder
  (pool minus populated slots inside the pool). Metrics updater and
  `StateTracker.Run` replay the latest snapshot from the bus ring at start, so
  a snapshot published before they subscribe is not lost. `SlotResult` gained
  `Route` and `VCLName`.
- **Jumps/s is not exported.** Ghost's `VhostStats` live in plain Rust structs
  and are only surfaced through `backend.list` JSON; there is no VSC segment, so
  the varnishstat collector has nothing to pick up and no Rust was touched.
  Docs point at `varnishlog -q 'VCL_use ~ via'` and `MAIN.n_vcl`.
- Operator: `varnish_gateway_route_vcl_policies{programmed="true|false"}`,
  counted from `status.ancestors` under our controller name (Programmed=True is
  `true`, everything else `false`). Silent when the kind is not served or not
  registered.

### Dashboard

- New "Route VCL" panel (hidden while the pool is 0 / no snapshot yet): pool
  size, programmed, compile failed, placeholder counts, master VCL name and a
  table of slot, policy, route, VCL name, state, error. `/api/state` gains
  `routeVcl`. The snapshot event is not shown in the event log; route VCL
  reload events are ("Route VCL" / "Route VCL Fail").
- A programmed-then-failed slot (previous version still serving) is shown as
  "compile failed" with its previous VCL name.

### Docs

`docs/reference/route-vcl-caveats.md` (nav under Reference), links from
varnishvclpolicy.md; observability.md (chaperone and operator metrics,
dashboard, jumps/s note), reload-paths.md (third hot path + diagram),
gatewayclassparameters.md env table. `make docs-build` created `.venv-docs`
and built clean (strict).

### Open questions

- The throttle is a rate ceiling rather than a CPU cap; if the intent was to
  bound CPU use under a 100m request, the default rate should be much lower
  (about 0.5/s), at the price of ~2 minutes to compile 64 slots at startup.
  Needs the P8 in-pod measurement on 9.1.0.
- `master_reloads_total` duplicates the old counters with a label; drop one
  if that is unwanted.

## P8 — findings from the kind run

An end-to-end run on a kind cluster found two bugs that no unit test caught,
both because the test doubles disagreed with production.

### Bug A: a VCC failure taken for a transport error

`varnishadm.Server.Exec` returns an empty response and an error for any
non-200 CLI reply; `MockVarnishadm` and the `internal/varnishtest` CLI adapter
returned the response with its status and a nil error. The reloader was
written against the mock and treated every `err != nil` as a lost connection,
so on the real server a route document that failed to compile was retried
every backoff, never memoised, never written to `podResults`, and the policy
sat at `Programmed=False/Pending` instead of `CompileFailed`. Logged as
`VCL reconcile failed ... varnishadm.VCLInline(route_1_...): command failed:
Message from VCC-compiler: ...`, repeating. The master path had the same flaw
(a VCC failure was not memoised and no reload-fail event was published), as
did every `vcl.label` call.

Fix, keeping `Server.Exec`'s contract:

- `internal/varnishadm`: the non-OK error is now a typed
  `*CLIError{Status, Payload}` whose `Error()` is still
  `"command failed: <payload>"`, so log lines and the existing
  `protocol_test.go` expectation are unchanged. Callers in `internal/tls` and
  `cmd/chaperone` only test `err != nil` and are unaffected;
  `internal/invalidation` does not use varnishadm (HTTP only). Nothing else
  matched on the text.
- `internal/vcl/reloader.go`: `cliOutcome(resp, err) (status, payload,
  transport)` normalises both conventions: `errors.As` a `*CLIError` first,
  then a plain error starting with `"command failed: "` (status reported as
  `ClisCant`), anything else is transport. `cliErr` is built on it. Every VCL
  CLI call goes through one of the two: placeholder and master `vcl.inline`,
  route `vcl.inline`, all `vcl.label`, `vcl.use`, `vcl.discard`, and
  `param.set max_vcl`. Result: route VCC failures are memoised and reported
  with the compiler output, route label refusals are recorded as the slot
  message and retried through `pass.retry` (results still published), a
  master VCC failure keeps the previous master and is memoised with a
  `vcl_reload_fail` event, and only transport errors abort a reconcile.
- One behaviour change beyond the brief: a refused `vcl.label` while
  **clearing** a slot back to the placeholder used to abort the whole slot
  pass (results unpublished); it now goes into `pass.retry` and the pass
  continues. A transport error there still aborts.
- `internal/varnishtest/adm.go`: the CLI adapter now mirrors `Server.Exec`
  exactly (empty response + `*CLIError` for a failed command; `ParamSet` no
  longer adds its own error). `TestReloaderAgainstVarnishd` step 7 (syntax
  error in slot 0) therefore runs the real error path; with the old reloader
  and the new adapter it fails with the exact cluster log line, with the fix
  it passes (log: `route VCL failed to compile ... status=106`).
- `MockVarnishadm.SetServerErrorSemantics(bool)` (default false, existing
  tests untouched) makes `Exec`, and every command built on it, return the
  typed error for non-OK replies. `TestReconcile_ServerErrorSemantics` runs
  three cases with it on: route VCC failure memoised, reported in
  `SlotResults` and in `podResults` via the fake dynamic client, `SlotsReady`
  closed; label loop refusal reported and retried without recompiling; master
  VCC failure keeps the previous master and is memoised. All three fail
  against the pre-fix reloader. `TestCLIOutcome` covers the classification
  table.

### Bug B: a returning gateway `vcl_recv` starved the route VCL

`GenerateRouteVCL` composed `preamble + gateway user VCL + route VCL +
postamble`. VCL concatenates same-named subroutines in document order, so the
shipped default gateway VCL (`deploy/03-gatewayclass.yaml`,
`sub vcl_recv { return (pass); }`) ended `vcl_recv` before the route's
`vcl_recv` could run. Seen on the cluster: the route's `vcl_deliver` header
appeared, its `vcl_recv` `return (synth(403))` never fired.

Fix: compose `preamble + route VCL + gateway user VCL + postamble`
(`GenerateRouteVCL` and the test-only `generateRouteVCL`). Route subroutines
run first, the gateway VCL runs after them and has the last word unless the
route returns. A `return` in a route `vcl_recv` now skips the gateway VCL's
`vcl_recv` and the postamble (so ghost's deferred pass is skipped on
`return (hash)`), the same rule the gateway VCL has toward the postamble.

This weakens G5's rationale: a route owner can now skip the gateway VCL in a
subroutine by returning early. The master's dispatch runs in its preamble,
before the master's copy of the gateway VCL, so for a slotted route the gateway
VCL runs only inside the route VCL. That was never a security boundary (route
VCL is code execution, per the trust statement), and the alternative (gateway
first) silently breaks route VCL under the default install. The docs say so.

- Merge order also decides import order: route imports now come before the
  gateway's (deduplicated). No effect on the existing header assertion.
- Composed documents change, so every route VCL hash changes once on upgrade
  and every populated slot recompiles once. Nothing to migrate.
- Tests: `TestGenerateRouteVCL_Composition` now asserts preamble's
  `router.recv()` < route `vcl_recv` code, route < gateway < postamble, and
  equality with `Merge(Generate(), route, gw)`. New real-varnishd test
  `TestRouteVCLRunsBeforeReturningGatewayVCL` (sibling of
  `TestGeneratedVCLCompiles`): shipped gateway VCL + a route VCL whose
  `vcl_recv` returns `synth(403)`; `/slot` gets 403 from the route VCL,
  `/plain` gets 200 from the master. With the old order it fails with
  `GET /slot: status 200`, which is the cluster symptom.
  `TestGeneratedVCLCompiles` did not depend on the order (its gateway VCL has
  only `vcl_deliver`).
- Docs: `docs/guides/route-vcl.md` ("What goes in the document"),
  `docs/reference/varnishvclpolicy.md` (overview paragraph),
  `docs/reference/route-vcl-caveats.md` (one-paragraph summary and the compose
  caveat, with what to do instead), `docs/concepts/vcl-merging.md` (new short
  "Route VCL" section), CHANGELOG entry and the G5 row above.
  `mkdocs build --strict` is clean.
