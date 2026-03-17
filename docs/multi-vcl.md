# Per-Route VCL for VarnishCachePolicy

## Goal

Allow VarnishCachePolicy (VCP) to carry user VCL snippets scoped to specific routes, instead
of the current global-only userVCL on GatewayClassParameters.

## Approach: VRT_call Inline Dispatch (Recommended)

Ghost calls user VCL subroutines directly via the Varnish C API, keeping everything in a
single VCL.

### How It Works

1. User creates a VCP targeting an HTTPRoute with a `customVCL` field containing VCL code
2. Operator generates a named subroutine in the main VCL for each VCP snippet
3. Ghost learns about the subroutine association (via `vcl_init` registration or ghost.json)
4. At request time in `vcl_recv`: ghost resolves the route, sets the backend, then calls the
   user subroutine via `VRT_call(ctx, sub)`
5. The user subroutine runs inline with full access to `req`, the already-set backend, etc.

### VCL Output Example

The operator generates named subroutines from VCP snippets and the ghost preamble wires
them up:

```vcl
import ghost;

# Generated from VCP "basic-caching" targeting HTTPRoute "default/my-route"
sub vcp_default_my_route_recv {
    # User-provided VCL from VCP customVCL field
    set req.http.X-Custom = "hello";
    # ... arbitrary user VCL ...
}

sub vcl_init {
    # ghost initialization, router creation...
    ghost.route_sub("default/my-route", vcp_default_my_route_recv);
}

sub vcl_recv {
    ghost.recv();
    # ghost internally: resolve route → set backend → VRT_call(ctx, sub) if registered
}
```

### Varnish C API

Two functions make this possible:

- `VRT_call(ctx, sub)` — executes a VCL subroutine from within VMOD code
- `VRT_check_call(ctx, sub)` — validates the subroutine is callable from the current
  context (returns NULL if OK, error string otherwise)

Ghost already has `ctx` access in `vcl_recv`. After resolving the route and setting the
backend, it checks for a registered subroutine and calls it inline. No VCL switching, no
request restart, no header dance.

### What the User's Subroutine Can Do

The subroutine runs in `vcl_recv` context with the backend already set:

- Inspect and modify `req.http.*` headers
- Override the backend with `set req.backend_hint = ...`
- Call `return(pass)`, `return(pipe)`, `return(synth(...))` etc.
- Access any VMOD imported in the main VCL

### Multiple Subroutine Hooks

VCPs may need logic in multiple VCL phases (recv, backend_response, deliver). The operator
can generate multiple subroutines per VCP:

```vcl
sub vcp_default_my_route_recv {
    # recv-phase user VCL
}

sub vcp_default_my_route_backend_response {
    set beresp.ttl = 5m;
}

sub vcp_default_my_route_deliver {
    unset resp.http.X-Internal;
}
```

Ghost would call the appropriate subroutine in each VCL phase where it has a hook
(`vcl_recv`, `vcl_backend_response`, `vcl_deliver`). Ghost currently only operates in
`vcl_recv` and `vcl_backend_fetch`, so extending to other phases would require new VMOD
entry points or postamble-generated dispatch.

### Benefits

- **Single VCL** — no memory duplication, no N× reload, no VCL lifecycle management
- **No dispatch loop** — subroutine is called inline, not via `return(vcl())`
- **No header guards** — ghost calls the sub directly, no coordination via request headers
- **Route-scoped** — user VCL only runs for matching routes
- **Compilation safety** — a syntax error in a VCP snippet fails the VCL load, giving a
  clear error, but doesn't silently break other routes at runtime

### Costs

- **Ghost complexity** — ghost needs to accept subroutine registrations and call them at
  the right phase. Moderate implementation effort.
- **VCL compilation coupling** — a bad VCP snippet prevents the entire VCL from compiling.
  One broken VCP blocks all routes until fixed. This is the main operational risk.
- **Phase coverage** — ghost currently hooks `vcl_recv` and `vcl_backend_fetch`. Calling
  user subs in `vcl_backend_response` or `vcl_deliver` requires ghost to have entry points
  there, or the postamble must generate dispatch code for those phases.

### Mitigating VCL Compilation Failures

Since a bad snippet breaks the entire VCL load:

- Operator can validate VCL syntax before generating (using vclparser or similar)
- Chaperone can keep the previous working VCL active on reload failure (already does this)
- Status conditions on the VCP can report compilation errors back to the user

## Rejected: Multi-VCL Dispatch via return(vcl())

An earlier design considered using `return(vcl(name))` to dispatch to separate named VCLs
per VCP. This was rejected due to:

- **Target VCL needs ghost** — `return(vcl())` restarts the request in a completely
  independent VCL program with no access to VMODs or state from the originating VCL. The
  target VCL must have its own ghost preamble, creating N+1 ghost instances.
- **N× memory** — each ghost instance independently loads ghost.json with its own routing
  table and backend objects.
- **N× reload** — every ghost.json change requires reloading all VCLs. Partial failures
  leave inconsistent state.
- **Dispatch loop** — ghost in the target VCL would re-resolve the route and attempt
  another `return(vcl())`, requiring header-based guard mechanisms.
- **VCL lifecycle management** — chaperone must track, load, label, and discard VCLs as
  VCPs are created, updated, and deleted.

## Also Considered: Guarded Blocks in Main VCL

Wrapping user VCL in `if (req.http.X-Gateway-Route == "ns/name")` blocks within standard
subroutines (`vcl_recv`, `vcl_backend_response`, etc.).

Not recommended because it requires parsing user VCL to extract subroutine bodies and
inject them into the correct locations. Fragile — and a syntax error in one VCP's snippet
can break the entire VCL in non-obvious ways (same compilation coupling as the VRT_call
approach, but with more complex code generation).

## Implementation Path

1. **Phase 1**: Structured VCP fields only (TTL, grace, bypass, cache key). Ghost
   implements these natively without user VCL. Covers 90% of use cases.
2. **Phase 2**: Add `customVCL` field to VCP with VRT_call dispatch. Ghost registers and
   calls per-route subroutines. Requires ghost entry points in additional VCL phases.
