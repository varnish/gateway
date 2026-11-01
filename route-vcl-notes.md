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
| G5 | Unassigned | **Compose.** A route VCL is `preamble + gateway user VCL + route VCL + postamble`. Rationale: gateway VCL is where shared normalisation and security headers live, and a route owner must not be able to opt out of them simply by attaching a policy. Cost: a gateway-VCL change recompiles every route VCL. Reversible by changing one function in `internal/vcl`. |

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
