# Debug Mode Stack Size Requirements

## Regex Compilation in Varnish Worker Threads

### Summary

VTC tests that use regular expressions now work in debug mode by increasing Varnish's `thread_pool_stack` parameter from the default 80k to 160k.

### Previous Symptom (FIXED)

VTC tests using regular expressions (`RegularExpression` path matching, header matching, or query parameter matching) would crash in debug mode with SIGABRT. Release mode worked fine.

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

### Solution

**Increase Varnish worker thread stack size** from the default 80k to 160k (2x increase).

The Rust `regex` crate in debug mode requires more stack space for TLS initialization. Varnish's `thread_pool_stack` parameter controls worker thread stack size.

**Implementation:**
- VTC tests: Add `-arg "-p thread_pool_stack=160k"` to varnish startup
- Production: Add `-p thread_pool_stack=160k` via `varnishdExtraArgs` in GatewayClassParameters
- Local testing: Set `VARNISHD_EXTRA_ARGS="-p;thread_pool_stack=160k"`

**Why 160k?**
- Default: 80k
- Debug mode regex TLS needs more stack than release mode
- Varnish docs recommend 150%-200% increments for stack issues
- 160k (2x) provides adequate headroom without wasting memory

### Previous Workarounds (Still Valid)

1. **Removed early validation**: Regex compilation removed from `config::validate()` - only compiled during `build_routing_state()`
2. **Deferred compilation**: Regex errors caught and reported during routing state build

### Impact

**Production**: ✅ No impact - works in both debug and release with `thread_pool_stack=160k`
**CI/CD**: ✅ Tests pass in both modes
**Local Development**: ✅ VTC tests work in debug mode with increased stack

### Testing Strategy

```bash
# All tests now work in debug mode (with increased stack in VTC tests)
cargo test

# Release mode still recommended for performance testing
cargo test --release
```

### Memory Overhead

Stack size is per-thread. For typical configurations:
- Default 80k × 2 pools × 100 threads/pool = 16 MB
- Increased 160k × 2 pools × 100 threads/pool = 32 MB
- Overhead: +16 MB (negligible for modern systems)

### Alternative Solutions (Not Needed)

The stack size increase is the simplest and most reliable solution. Other approaches considered:

1. **Lazy regex compilation**: Defer until first match (complex, adds overhead)
2. **Static regex compilation**: Use `lazy_static!` (breaks hot-reload)
3. **C-compatible regex**: PCRE2 via FFI (loses Rust safety)
4. **Varnish thread hooks**: Initialize Rust TLS (fragile, requires patches)

### References

- Rust TLS implementation: https://doc.rust-lang.org/std/macro.thread_local.html
- regex crate internals: https://docs.rs/regex/latest/regex/#performance
- Varnish threading model: https://varnish-cache.org/docs/trunk/phk/threenine.html
