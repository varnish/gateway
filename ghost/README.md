# Ghost VMOD

Ghost is a Rust VMOD for Varnish that implements Kubernetes Gateway API routing. It handles hostname matching, path/header/query routing, weighted backend selection, and configuration hot-reloading — all inside Varnish's request processing pipeline.

For the VCL API reference, see [API.md](API.md).

## How It Works

### Two-Tier Director Architecture

Ghost uses a two-tier director design:

1. **GhostDirector** (meta-director) — receives every request and matches the `Host` header against configured virtual hosts. Supports exact hostnames (`api.example.com`) and wildcards (`*.staging.example.com`). Delegates to the matching VhostDirector.

2. **VhostDirector** (per-vhost) — handles route matching within a single virtual host. Evaluates path (exact, prefix, regex), HTTP method, headers, and query parameters. Routes are scored by priority with additive specificity bonuses, matching Gateway API precedence rules. Once a route is matched, a backend is selected via weighted random selection.

This separation keeps hostname resolution cheap and isolated from per-vhost route complexity. Each vhost tracks its own statistics independently.

### Native Backends

Ghost uses Varnish's built-in HTTP client and connection pooling rather than an async Rust HTTP client. This means:

- Varnish manages all TCP connections, keepalive, and retries
- Backend health checks work through standard Varnish mechanisms
- No Tokio runtime, no extra threads — ghost runs entirely within Varnish worker threads
- Backends show up in `varnishadm backend.list` like any other Varnish backend

### Request Flow

```
incoming request
  │
  ├─ GhostDirector: match Host header
  │   ├─ exact match?     → VhostDirector
  │   ├─ wildcard match?  → VhostDirector
  │   └─ no match         → synthetic 404
  │
  └─ VhostDirector: match route
      ├─ evaluate all routes (path + method + headers + query params)
      ├─ pick highest-priority match
      ├─ apply request filters (header modification, URL rewrite)
      ├─ select backend by weight
      └─ return native Varnish backend → Varnish handles the HTTP fetch
```

### Hot Reload

Configuration reloads are lock-free. The routing state is held behind an `ArcSwap` — on reload, a new state is built from the updated `ghost.json`, then atomically swapped in. In-flight requests continue using the old state until they complete; new requests immediately see the new state.

Reload is triggered by an HTTP request to `/.varnish-ghost/reload` (localhost only). Chaperone sends this whenever routing or endpoints change.

### Configuration

Ghost reads a single `ghost.json` file that maps hostnames to routes with resolved backend addresses. This file is produced by chaperone, which merges:

- **routing.json** (from the operator) — hostname-to-service mappings derived from HTTPRoute resources
- **EndpointSlice data** (from Kubernetes) — actual pod IPs for each service

The result is a flat config with real IP addresses that ghost can use directly to create Varnish backends.

### Stack Requirements

Rust code (especially debug builds and regex operations) needs more stack than Varnish's default 80kB. Configure Varnish with:

```
varnishd -p thread_pool_stack=160k
```

## Build & Test

```bash
cargo build --release           # build
cargo test --lib                # unit tests
cargo test --release            # all tests including VTC integration tests
```

VTC tests must run in release mode due to Varnish thread-local storage constraints in debug builds. See `DEBUG_MODE_LIMITATIONS.md` for details.
