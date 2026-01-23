# Ghost VMOD

Rust vmod for Varnish Gateway API routing. See `ghost-vmod.md` for full plan and `README.md` for auto-generated API documentation.

## Monorepo Structure

This is part of a monorepo where all components (operator, chaperone, ghost) are always deployed together and updated in sync.

**Backward compatibility is not required:**
- No need for compatibility shims, feature flags, or gradual migrations
- Breaking changes can be made freely across component boundaries
- Unused code should be deleted completely (no renaming to `_unused`, re-exporting, or `// removed` comments)
- Interface changes between components can be made atomically in a single commit

## Architecture

Ghost uses **Varnish native backends** with the director pattern for routing. This provides:
- Battle-tested HTTP client and connection pooling from Varnish
- Lower latency and memory usage vs async Rust HTTP
- Simpler code with fewer dependencies
- Better integration with Varnish ecosystem

## Backend Lifecycle

Backends are created on-demand during config reload and stored in a per-director backend pool.
When a config is reloaded, backends no longer referenced in the routing configuration are
automatically removed from the pool to prevent memory leaks.

This cleanup is safe because:
- Varnish's VCL_BACKEND lifecycle management handles in-flight requests
- Backends are removed only after the new routing state is fully constructed
- The cleanup happens atomically under a write lock to maintain consistency

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

**Important**: VTC tests that use regular expressions fail in debug mode due to TLS conflicts between the regex crate and Varnish's threading model. This is a known limitation that only affects debug builds. See `DEBUG_MODE_LIMITATIONS.md` for details.

## Key Files

- `src/lib.rs` - VMOD entry points, ghost_backend object
- `src/config.rs` - JSON config parsing and validation
- `src/director.rs` - Host matching, weighted backend selection, director implementation
- `src/backend_pool.rs` - Native backend creation and management
- `src/not_found_backend.rs` - Synthetic 404 backend for undefined vhosts

## Key Dependencies

- `varnish` - VMOD bindings and native backend support
- `parking_lot` - RwLock for lock-free reads during hot-reload
- `rand` - Weighted random backend selection
- `serde/serde_json` - Configuration parsing

## Status

Phase 1 (Virtual Host Routing) complete. See `ghost-vmod.md` for roadmap.

## Conventions

- Use `cargo fmt` and `cargo clippy`
- Error handling via `VclError`
- Config hot-reload via `Arc<RwLock<>>`
- Backends owned by director instances (not global)
