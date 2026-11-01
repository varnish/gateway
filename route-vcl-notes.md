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
