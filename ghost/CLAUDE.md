# Ghost VMOD

Rust vmod for Varnish Gateway API routing. See `ghost-vmod.md` for full plan and `API.md` for auto-generated API
reference.

## Monorepo Structure

This is part of a monorepo where all components (operator, chaperone, ghost) are always deployed together and updated in
sync.

**Backward compatibility is not required:**

- No need for compatibility shims, feature flags, or gradual migrations
- Breaking changes can be made freely across component boundaries
- Unused code should be deleted completely (no renaming to `_unused`, re-exporting, or `// removed` comments)
- Interface changes between components can be made atomically in a single commit

## Architecture

### Two-Tier Director Pattern

Ghost uses a **two-tier director architecture** for efficient routing:

1. **GhostDirector** (meta-director) - Matches hostname (exact or wildcard) and delegates to the appropriate VhostDirector
2. **VhostDirector** (per-vhost) - Handles route matching (path, method, headers, query params) and backend selection for a single virtual host

This separation provides:
- Clean separation of concerns (hostname vs route matching)
- Efficient per-vhost statistics tracking
- Lock-free hot-reload via ArcSwap (atomic pointer swaps)
- Easy scaling to thousands of routes per vhost

### Native Backends

Ghost uses **Varnish native backends** instead of a custom HTTP client. This provides:

- Battle-tested HTTP client and connection pooling from Varnish
- Lower latency and memory usage vs async Rust HTTP
- Simpler code with fewer dependencies
- Better integration with Varnish ecosystem (health checks, backend.list, etc.)

## Backend Lifecycle

Backends are created on-demand during config reload and stored in a per-director backend pool.
When a config is reloaded, backends no longer referenced in the routing configuration are
automatically removed from the pool to prevent memory leaks.

This cleanup is safe because:

- Varnish's VCL_BACKEND lifecycle management handles in-flight requests
- Backends are removed only after the new routing state is fully constructed
- The cleanup happens atomically via ArcSwap (lock-free atomic pointer swap)

## Build

Local (requires Varnish 8.0 installed):

```bash
cargo build --release
```

Full image (from repo root):

```bash
docker build -f docker/chaperone.Dockerfile -t varnish-gateway .
```

## Test

```bash
# Unit tests (work in debug and release mode)
cargo test --lib -- --skip run_vtc_tests

# VTC integration tests (must use release mode - see DEBUG_MODE_LIMITATIONS.md)
cargo test --release run_vtc_tests

# All tests in release mode (recommended)
cargo test --release
```

**Important**: VTC tests require increased stack size (`-p thread_pool_stack=160k`) for regex support in debug mode. All
VTC test files include this parameter. See `DEBUG_MODE_LIMITATIONS.md` for details.

## Key Files

- `src/lib.rs` - VMOD entry points, ghost_backend object, reload endpoint
- `src/config.rs` - JSON config parsing and validation (routing.json, ghost.json)
- `src/director.rs` - GhostDirector (meta-director), hostname matching, compiled match types
- `src/vhost_director.rs` - VhostDirector (per-vhost), route matching, backend selection, filter application
- `src/backend_pool.rs` - Native backend creation and management, automatic cleanup
- `src/not_found_backend.rs` - Synthetic 404 backend for undefined vhosts
- `src/stats.rs` - Per-vhost and per-backend statistics tracking
- `src/format.rs` - Formatting utilities for backend.list JSON output

## Key Dependencies

- `varnish` - VMOD bindings and native backend support
- `arc-swap` - Lock-free atomic swaps for hot-reload (main routing state)
- `parking_lot` - RwLock for error message storage
- `rand` - Weighted random backend selection
- `regex` - Path/header/query parameter regex matching
- `serde/serde_json` - Configuration parsing and filter serialization

## Status

**Phase 3 Complete** - Advanced request matching and Gateway API filters fully implemented:

- Virtual host routing (exact and wildcard hostnames)
- Advanced path matching (exact, prefix, regex)
- HTTP method matching
- Header matching (exact and regex)
- Query parameter matching (exact and regex)
- Priority-based route selection with specificity bonuses
- Request header filters (add, set, remove)
- Response header filters (add, set, remove)
- URL rewrite filters (ReplaceFullPath, ReplacePrefixMatch with intelligent fallback)

See parent directory's CLAUDE.md for full project roadmap.

## Conventions

- Use `cargo fmt` and `cargo clippy`
- Error handling via `VclError`
- Config hot-reload via `ArcSwap` for lock-free atomic swaps
- Compiled match types (regex, path patterns) pre-compiled during config load
- Backends owned by director instances (not global)
- Avoid allocations in hot path (use borrowed data where possible)
- Comprehensive rustdoc comments for public functions and complex logic
