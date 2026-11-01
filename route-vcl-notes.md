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
