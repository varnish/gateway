# Ghost VMOD

Rust vmod for Varnish Gateway API routing. See `ghost-vmod.md` for full plan and `README.md` for auto-generated API documentation.

## Architecture

Ghost uses **Varnish native backends** with the director pattern for routing. This provides:
- Battle-tested HTTP client and connection pooling from Varnish
- Lower latency and memory usage vs async Rust HTTP
- Simpler code with fewer dependencies
- Better integration with Varnish ecosystem

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
LD_LIBRARY_PATH=/opt/homebrew/lib cargo test --lib -- --skip run_vtc_tests
```

VTC integration tests require the vmod to be built and installed first.

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
