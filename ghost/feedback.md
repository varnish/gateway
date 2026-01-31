# Ghost VMOD - Outstanding Issues

## Readability Issues

### Complex Filter Application Functions

**Location**: `src/vhost_director.rs:650-746`

**Issue**: `apply_url_rewrite_filter()` has deep nesting, especially the `ReplacePrefixMatch` logic (lines 671-735).

**Recommendation**: Extract `ReplacePrefixMatch` logic to separate function:

```rust
fn apply_replace_prefix_match(
    path: &str,
    new_prefix: &str,
    matched_path: Option<&PathMatchCompiled>,
) -> (String, Option<(LogTag, String)>) {
    // Current lines 683-720
    // ...
}
```

---

### Heuristic Function Documentation

**Location**: `src/vhost_director.rs:749-766`

**Issue**: `replace_first_segment_heuristic()` name doesn't clearly convey when/why it's used.

**Current Comment**:
```rust
/// Heuristic: replace first path segment (fallback when matched prefix unknown)
```

**Recommended Documentation**:
```rust
/// Replace first path segment with new prefix (fallback for regex/no-match cases).
///
/// This is a best-effort heuristic used when:
/// - Route uses RegularExpression path matching (can't extract matched portion)
/// - Route has no path match specified
/// - We need to apply ReplacePrefixMatch but don't know what was matched
///
/// Examples:
/// - `/v1/users` with prefix `/v2` -> `/v2/users`
/// - `/api/v1/status` with prefix `/api/v2` -> `/api/v2/v1/status`
///
/// This is NOT semantically correct per Gateway API spec, but provides
/// reasonable behavior when exact matching isn't possible.
```

---

### Thread Safety Documentation

**Location**:
- `src/director.rs:23-30` - `SendSyncBackend`
- `src/backend_pool.rs:22-26` - `BackendPool`

**Issue**: Comments say "single-threaded per worker" but code uses `Arc` and atomic operations.

**Current Comment** (`SendSyncBackend`):
```rust
/// This is safe because Varnish runs single-threaded per worker,
/// and the backend pointer is valid for the lifetime of the director.
```

**Recommended Comment**:
```rust
/// SAFETY: VCL_BACKEND is an opaque Varnish handle designed for multi-threaded use.
/// While Varnish workers are single-threaded, we use Arc and atomic operations
/// because multiple workers may access the same director concurrently (via shared
/// VCL state). The raw pointer is managed by Varnish's backend infrastructure
/// which provides its own synchronization guarantees.
```

---

## Performance Notes

### Thread-local RNG Overhead

**Location**: `src/vhost_director.rs:354`

**Issue**: `rand::thread_rng()` called on every backend selection.

**Mitigation**: Already optimized for single-backend case (lines 341-343).

**Recommendation**: Monitor if this becomes a bottleneck. Could cache RNG, but current approach is clean and likely fast enough.
