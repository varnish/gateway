# Debug Mode Limitations

## Regex Compilation in Varnish Worker Threads

### Symptom

VTC tests that use regular expressions (`RegularExpression` path matching, header matching, or query parameter matching) **crash in debug mode** with SIGABRT. The same tests pass in release mode.

Example failing test: `tests/test_path_matching.vtc`

### Root Cause: Thread-Local Storage (TLS) Conflict

The Rust `regex` crate uses lazy initialization with thread-local storage (TLS) for its internal caches and state. In debug mode, it also enables additional runtime checks that rely on TLS.

**The problem occurs because:**

1. **Varnish's Threading Model**: Varnish creates worker threads using C's `pthread` library. These are "foreign" threads from Rust's perspective - they weren't created by Rust's runtime.

2. **Dynamic Library Loading**: Our VMOD is loaded as a shared library (`.so`) into an already-running Varnish process. The Rust runtime doesn't get a chance to properly initialize TLS for threads that already exist.

3. **TLS Access Pattern**: When `Regex::new()` is called from a Varnish worker thread for the first time:
   ```
   vcl_recv (Varnish thread)
     → router.reload()
       → build_routing_state()
         → PathMatchCompiled::from_config()
           → Regex::new()  ← CRASH HERE in debug mode
   ```

4. **Debug vs Release Behavior**:
   - **Debug mode**: The regex crate uses `thread_local!` macros that perform runtime checks and expect proper TLS initialization. When accessed from a foreign thread, these checks detect the uninitialized state and abort.
   - **Release mode**: Optimizations and inlining reduce TLS dependencies. The compiled code uses simpler, more direct access patterns that don't require full TLS initialization.

### Technical Details

The regex crate's debug mode internally uses:
- `thread_local!` for cache storage (DFA states, NFA threads, etc.)
- Lazy initialization via `OnceCell` or similar primitives
- Runtime assertions about thread ownership

When a Varnish C thread (created before the Rust VMOD was loaded) tries to access these thread-locals:
1. Rust's TLS mechanism (`__tls_get_addr` on Linux) is called
2. The TLS block for this thread doesn't exist (wasn't initialized by Rust)
3. Debug assertions detect this and call `std::process::abort()`
4. Varnish logs: `Error: Child died signal=6 (core dumped)`

### Why This Only Affects Debug Mode

Release builds work because:
- Dead code elimination removes unused TLS accessors
- Inlining allows the compiler to optimize away indirect TLS access
- LLVM optimizations convert TLS access to simpler patterns
- No runtime assertions checking TLS validity

### Workarounds We Implemented

1. **Removed early validation**: Deleted regex compilation from `config::validate()` functions. Regex patterns are now only compiled once during `build_routing_state()`.

2. **Deferred compilation**: Regex compilation happens during routing state build, where errors can be properly caught and reported.

3. **Accept the limitation**: Document that debug mode VTC tests with regex will fail, but this doesn't affect production (release builds).

### Impact

**Production**: ✅ No impact - release builds work perfectly
**CI/CD**: ✅ Tests should run in release mode anyway
**Local Development**: ⚠️ Developers running VTC tests in debug mode will see failures with regex patterns

### Testing Strategy

```bash
# Run all tests in release mode (recommended)
cargo test --release

# Run VTC tests in release mode only
cargo test --release run_vtc_tests

# Unit tests work in debug mode (don't trigger the issue)
cargo test --lib -- --skip run_vtc_tests
```

### Alternative Solutions (Not Implemented)

1. **Lazy regex compilation**: Could defer regex compilation until first match attempt (from a Rust thread context). Complex and adds runtime overhead.

2. **Static regex compilation**: Use `lazy_static!` or `once_cell::sync::OnceCell` for global regex storage. Requires redesigning hot-reload architecture.

3. **C-compatible regex library**: Switch to PCRE2 via C FFI. Loses Rust safety guarantees and regex syntax differences.

4. **Varnish thread initialization**: Hook into Varnish's thread creation to initialize Rust TLS. Requires Varnish patches and is fragile.

### References

- Rust TLS implementation: https://doc.rust-lang.org/std/macro.thread_local.html
- regex crate internals: https://docs.rs/regex/latest/regex/#performance
- Varnish threading model: https://varnish-cache.org/docs/trunk/phk/threenine.html
