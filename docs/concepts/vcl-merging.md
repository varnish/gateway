# VCL Merging

Varnish Gateway generates the VCL it needs to drive ghost, and lets
users append their own VCL on top. The merge is plain textual
concatenation and it relies on one VCL language feature:
when the same subroutine is defined more than once,
Varnish concatenates the bodies at compile time and runs them in
declaration order.

## How merging works

A compiled gateway VCL is three pieces, in order:

1. **Preamble** — imports and the subroutines ghost needs (routing,
   cache policy application, ban lurker headers). Runs first.
2. **User VCL** — whatever you supply through
   `GatewayClassParameters.userVCLConfigMapRef`. Runs in the middle.
3. **Postamble** — a short `vcl_recv` that executes the deferred
   `return(pass)` decision. Runs last.

If you define `sub vcl_recv` in your VCL, Varnish fuses the
preamble's, yours, and the postamble's bodies into a single
subroutine that runs top-to-bottom. Same for every other subroutine.

See [guides/custom-vcl.md](../guides/custom-vcl.md) for how to attach
your VCL.

## What this means for your VCL

**Early `return`s skip everything after them.** A `return` in your
`vcl_recv` prevents the postamble from running, which means the
deferred `return(pass)` check won't fire. In practice that only
matters when ghost wanted to pass and you pre-empted it with your own
return — at that point you have made an explicit decision and it
sticks.

**`vcl_synth` and `vcl_backend_error` are yours.** The preamble's
`vcl_synth` only decorates responses on the internal
`/.varnish-ghost/reload` endpoint; `vcl_backend_error` is not
generated at all. Both are free for error pages, retries, and
synthetic responses. Ghost's own synthetic responses (e.g., 404 for
unknown vhosts) come from a VMOD backend, not from `vcl_synth`.

**Your VCL runs after ghost has chosen a backend.** Routing happens
in the preamble's `vcl_recv`. By the time your `vcl_recv` runs,
`req.backend_hint` is already set and the cache-policy headers
(`X-Ghost-*`) are already on the request. You can override them, but
you cannot re-enter ghost's router — there is no call back in.

**User VCL is global.** The same VCL applies on every listener and
every route. Branch on `X-Gateway-Listener` or `X-Gateway-Route` for
listener- or route-specific behavior; see
[multi-listener.md](multi-listener.md).

## See also

- [Custom VCL guide](../guides/custom-vcl.md) — attaching user VCL.
- [Caching model](caching-model.md) — the `X-Ghost-*` headers and
  the deferred-pass pattern.
- [Reload paths](reload-paths.md) — how a VCL change reaches
  varnishd.
