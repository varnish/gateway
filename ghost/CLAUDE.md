# Ghost VMOD

Rust vmod for Varnish Gateway API routing. See `ghost-vmod.md` for full plan and `README.md` for auto-generated API documentation.

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

- `src/lib.rs` - VMOD entry points, GhostBackend
- `src/config.rs` - JSON config parsing
- `src/routing.rs` - Host matching, backend selection
- `src/response.rs` - HTTP response handling

## Status

Phase 1 (Virtual Host Routing) complete. See `ghost-vmod.md` for roadmap.

## Conventions

- Use `cargo fmt` and `cargo clippy`
- Error handling via `VclError`
- Config hot-reload via `Arc<RwLock<>>`
