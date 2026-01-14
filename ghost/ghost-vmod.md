# Ghost VMOD - Development Plan

A purpose-built Varnish vmod for Gateway API implementation, written in Rust using [varnish-rs](https://github.com/varnish-rs/varnish-rs).

## Status

| Phase | Description | Status |
|-------|-------------|--------|
| 1 | Virtual Host Routing | **COMPLETE** |
| 2 | Path Matching | Not started |
| 3 | Advanced Request Matching | Not started |
| 4 | Traffic Management | Not started |
| 5 | Request/Response Modification | Not started |
| 6 | Backend Health and TLS | Not started |

### Phase 1 Completion Notes

**Implemented:**
- [x] JSON config loading and validation
- [x] Exact hostname matching
- [x] Wildcard hostname matching (single label per Gateway API spec)
- [x] Weighted random backend selection
- [x] Default backend fallback
- [x] Hot reload via `/.varnish-ghost/reload`
- [x] HTTP forwarding via async reqwest with connection pooling
- [x] Hop-by-hop header filtering
- [x] Error responses: 404 (no vhost), 503 (no backends) with JSON bodies
- [x] Docker build with Varnish 7.6
- [x] Unit tests for config and routing
- [x] Background tokio runtime for async HTTP (vmod-reqwest pattern)
- [x] Connection pooling that survives config reloads
- [x] Streaming response bodies via channels

**Not yet implemented:**
- [ ] VTC integration tests (framework ready, tests need real Varnish)
- [ ] Localhost-only restriction on reload endpoint

**Architecture decision:** Uses synthetic backend pattern (like vmod-reqwest) rather than Varnish directors. The `GhostBackend` sends HTTP requests to a background tokio runtime via channels. The async reqwest client handles connection pooling, and responses are streamed back via channels.

**Build requirement:** Docker with Varnish 7.6. varnish-rs 0.5.5 doesn't support Varnish trunk (VDP API changes).

---

## Overview

Ghost replaces the nodes/udo/activedns vmod trio with a single vmod designed specifically for Kubernetes Gateway API routing. It handles backend management, request routing, and configuration hot-reloading internally.

**Design principles:**
- Configuration-driven: JSON file defines all routing and backend behavior
- Hot-reloadable: Configuration changes without VCL reload
- Minimal VCL footprint: Two function calls injected into user VCL
- Progressive enhancement: Start simple, add Gateway API features incrementally

## Target Platform

- **Varnish Cache 7.6** (varnish-rs 0.5.5 compatibility)
- **Rust** via varnish-rs
- **Docker** images built with ghost included

## Integration Points

### VCL Integration

Ghost injects minimal VCL into the user's configuration:

```vcl
import ghost;

sub vcl_init {
    ghost.init("/var/run/varnish/ghost.json");
    new router = ghost.ghost_backend();
}

sub vcl_recv {
    if (ghost.recv()) {
        return (synth(200, "Reload"));
    }
}

sub vcl_backend_fetch {
    set bereq.backend = router.backend();
}

# --- User VCL below ---
```

The operator generates this preamble and concatenates user VCL after it. VCL's subroutine concatenation means user `vcl_recv` and `vcl_backend_fetch` code runs after ghost's routing calls.

### Configuration Reload

Ghost supports hot-reload via a magic HTTP request:

```
GET /.varnish-ghost/reload HTTP/1.1
Host: localhost
```

When `ghost.recv()` sees this request:
1. Re-reads the JSON configuration file
2. Validates the new configuration
3. Atomically swaps to the new config (Arc swap)
4. Returns JSON status: `{"status": "ok"}` or `{"status": "error", "message": "..."}`

The chaperone triggers reload by sending this request to Varnish on localhost.

### Exposed Functions

| Function | Subroutine | Description |
|----------|------------|-------------|
| `ghost.init(path)` | `vcl_init` | Initialize ghost with config file path |
| `ghost.recv()` | `vcl_recv` | Handle reload requests; returns JSON string if reload request |
| `ghost.ghost_backend()` | `vcl_init` | Create backend object |
| `router.backend()` | `vcl_backend_fetch` | Return VCL_BACKEND for routing |

---

## Phase 1: Virtual Host Routing - COMPLETE

**Goal:** Route requests to backend groups based on hostname only.

### Configuration Schema (v1)

```json
{
  "version": 1,
  "vhosts": {
    "api.example.com": {
      "backends": [
        {"address": "10.0.0.1", "port": 8080, "weight": 100},
        {"address": "10.0.0.2", "port": 8080, "weight": 100}
      ]
    },
    "web.example.com": {
      "backends": [
        {"address": "10.0.1.1", "port": 80, "weight": 100}
      ]
    },
    "*.staging.example.com": {
      "backends": [
        {"address": "10.0.2.1", "port": 8080, "weight": 100}
      ]
    }
  },
  "default": {
    "backends": [
      {"address": "10.0.99.1", "port": 80, "weight": 100}
    ]
  }
}
```

### Features

- **Exact hostname matching**: `api.example.com`
- **Wildcard hostname matching**: `*.staging.example.com` (single label wildcard per Gateway API)
- **Weighted random selection**: Distribute traffic based on backend weights
- **Default backend group**: Catch-all for unmatched requests
- **Empty backends**: Return 503 when no backends available
- **No vhost match**: Return 404

### Implementation

**Files:**
- `src/lib.rs` - VMOD entry points, GhostBackend implementation
- `src/config.rs` - JSON parsing and validation
- `src/routing.rs` - Host matching and backend selection
- `src/response.rs` - VclResponse implementation
- `Dockerfile` - Build with Varnish 7.6

**Data structures:**
- HashMap for exact hostname lookups
- Linear scan for wildcard matching (evaluated in order)
- Per-vhost backend list with weights

**Backend selection:**
- Weighted random selection via `rand::thread_rng().gen_range()`

**Reload safety:**
- Parse new config fully before swapping
- Use `Arc<GhostState>` with `parking_lot::RwLock`
- Old config dropped when last reference released

### Build

```bash
docker build -t ghost-build .
docker run --rm ghost-build cat /usr/lib/varnish/vmods/libvmod_ghost.so > libvmod_ghost.so
```

---

## Phase 2: Path Matching

**Goal:** Add path-based routing within vhosts.

### Configuration Schema (v2)

```json
{
  "version": 2,
  "vhosts": {
    "api.example.com": {
      "routes": [
        {
          "path": {"type": "prefix", "value": "/v1/"},
          "backends": [
            {"address": "10.0.0.1", "port": 8080, "weight": 100}
          ]
        },
        {
          "path": {"type": "prefix", "value": "/v2/"},
          "backends": [
            {"address": "10.0.0.2", "port": 8080, "weight": 100}
          ]
        },
        {
          "path": {"type": "exact", "value": "/health"},
          "backends": [
            {"address": "10.0.0.3", "port": 8080, "weight": 100}
          ]
        }
      ],
      "default": {
        "backends": [
          {"address": "10.0.0.99", "port": 8080, "weight": 100}
        ]
      }
    }
  }
}
```

### Features

- **Path prefix matching**: `/api/` matches `/api/users`, `/api/posts`
- **Exact path matching**: `/health` matches only `/health`
- **Regex path matching**: `^/users/[0-9]+$` (extended support)
- **Route ordering**: Routes evaluated in definition order, first match wins
- **Per-vhost default**: Fallback when no routes match within a vhost

### Implementation Notes

**Path matching order:**
1. Exact matches first (O(1) lookup via HashMap)
2. Prefix matches (sorted longest-first)
3. Regex matches (in definition order)
4. Vhost default

**Regex handling:**
- Pre-compile regexes at config load time
- Use `regex` crate for PCRE-like patterns
- Fail config load if regex is invalid

---

## Phase 3: Advanced Request Matching

**Goal:** Support full HTTPRoute matching capabilities.

### Configuration Schema (v3)

```json
{
  "version": 3,
  "vhosts": {
    "api.example.com": {
      "routes": [
        {
          "matches": [
            {
              "path": {"type": "prefix", "value": "/api/"},
              "headers": [
                {"name": "X-API-Version", "type": "exact", "value": "v2"}
              ],
              "method": "POST"
            }
          ],
          "backends": [...]
        }
      ]
    }
  }
}
```

### Features

- **Header matching**: Exact, prefix, regex on any request header
- **Method matching**: GET, POST, PUT, DELETE, etc.
- **Query parameter matching**: Exact, regex on query string params
- **Multiple match conditions**: All conditions in a match must be true (AND)
- **Multiple matches per route**: Any match can trigger the route (OR)

### Implementation Notes

**Match evaluation:**
- Short-circuit on first failed condition within a match
- Cache parsed query strings per request

**Header access:**
- Use Varnish's header access APIs via varnish-rs
- Handle missing headers gracefully (no match, not error)

---

## Phase 4: Traffic Management

**Goal:** Support traffic splitting and mirroring.

### Configuration Schema (v4)

```json
{
  "version": 4,
  "vhosts": {
    "api.example.com": {
      "routes": [
        {
          "matches": [...],
          "backends": [
            {"group": "v1", "weight": 90},
            {"group": "v2", "weight": 10}
          ]
        }
      ]
    }
  },
  "backend_groups": {
    "v1": {
      "backends": [
        {"address": "10.0.0.1", "port": 8080}
      ]
    },
    "v2": {
      "backends": [
        {"address": "10.0.1.1", "port": 8080}
      ]
    }
  }
}
```

### Features

- **Traffic splitting**: Weighted distribution across backend groups (canary deploys)
- **Request mirroring**: Send copy of request to secondary backend (shadow traffic)
- **Backend groups**: Named groups reusable across routes
- **Sticky sessions**: Optional session affinity via cookie/header (extended)

### Implementation Notes

**Traffic splitting:**
- Random selection weighted by group weights
- Deterministic option via hash of client IP or header

**Request mirroring:**
- Mark request for mirroring in `vcl_recv`
- Clone in `vcl_backend_fetch` (or document VCL pattern)
- Mirror is fire-and-forget, response discarded

---

## Phase 5: Request/Response Modification

**Goal:** Support Gateway API filters.

### Configuration Schema (v5)

```json
{
  "version": 5,
  "vhosts": {
    "api.example.com": {
      "routes": [
        {
          "matches": [...],
          "filters": [
            {
              "type": "request_header_modifier",
              "add": [{"name": "X-Forwarded-Host", "value": "${host}"}],
              "set": [{"name": "X-Real-IP", "value": "${client.ip}"}],
              "remove": ["X-Internal-Header"]
            },
            {
              "type": "url_rewrite",
              "path": {"type": "replace_prefix", "from": "/old/", "to": "/new/"}
            }
          ],
          "backends": [...]
        }
      ]
    }
  }
}
```

### Features

- **Request header modification**: Add, set, remove headers
- **Response header modification**: Add, set, remove on responses
- **URL rewriting**: Path prefix replacement, full path replacement
- **Redirects**: 301/302 redirects without hitting backend
- **Variable substitution**: `${host}`, `${client.ip}`, `${path}`, etc.

### Implementation Notes

**Filter ordering:**
1. Redirects (short-circuit, no backend)
2. URL rewrites
3. Request header modifications
4. Backend selection
5. Response header modifications (in `vcl_deliver`)

**New integration point:**
- Add `ghost.deliver()` for response header modification

---

## Phase 6: Backend Health and TLS

**Goal:** Production-ready backend management.

### Configuration Schema (v6)

```json
{
  "version": 6,
  "backend_groups": {
    "api": {
      "backends": [...],
      "health_check": {
        "path": "/health",
        "interval": "5s",
        "timeout": "2s",
        "healthy_threshold": 2,
        "unhealthy_threshold": 3
      },
      "tls": {
        "enabled": true,
        "verify": true,
        "ca_cert": "/etc/varnish/ca.pem"
      }
    }
  }
}
```

### Features

- **Active health checks**: Periodic probes to backends
- **Passive health checks**: Track backend errors, circuit breaker
- **Backend TLS**: HTTPS to backends with certificate verification
- **Connection pooling**: Configurable connection reuse
- **Timeouts**: Connect, first byte, between bytes timeouts

### Implementation Notes

**Health checks:**
- Background thread for active probes
- Atomic health state updates
- Integrate with Varnish's backend health system if possible

**TLS:**
- Use Varnish's backend TLS support
- Configure via ghost or document VCL pattern

---

## Testing Strategy

### Unit Tests (Rust)

- Config parsing and validation
- Hostname matching (exact, wildcard)
- Path matching (prefix, exact, regex)
- Backend selection with weights
- Config reload logic

### VTC Tests (Varnish Test Cases)

- End-to-end routing tests
- Reload behavior verification
- Error handling (invalid config, empty backends)
- Performance benchmarks

### Integration Tests

- Docker-based tests with real Varnish
- Reload under load
- Backend failure scenarios

---

## Build and Distribution

### Docker Image Structure

```dockerfile
FROM rust:1.83-bookworm AS builder
# Install Varnish 7.6 dev headers, clang
# Build ghost.so

FROM varnish:7.6
COPY --from=builder /build/target/release/libvmod_ghost.so /usr/lib/varnish/vmods/
```

### Versioning

Ghost version is coupled to operator version:
- `ghost v0.1.x` works with `operator v0.1.x`
- Config schema version indicates required ghost version
- Operator writes config version, ghost validates compatibility

---

## Resolved Questions

1. **VCL variable passing**: ~~How does `ghost.recv()` communicate routing decisions to `ghost.backend_fetch()`?~~

   **Answer:** Not needed. Routing happens entirely in `get_response()` when the backend is invoked. The `GhostBackend` reads the Host header from `bereq` directly.

2. **Regex crate size**: ~~The `regex` crate adds binary size. Defer regex support to Phase 3+?~~

   **Answer:** Yes, deferred. Phase 1 uses no regex. Binary is 4MB with reqwest included.

3. **Health check integration**: ~~Does Varnish 8.0 expose backend health APIs to vmods?~~

   **Answer:** TBD in Phase 6. For now, using synthetic backend means health checks would need to be implemented in ghost itself.

4. **Logging**: ~~How should ghost report errors and debug info?~~

   **Answer:** Currently uses VclError for error propagation. VSL logging available via `ctx.log()`. Not yet heavily instrumented.

## Open Questions

1. ~~**Async vs blocking**~~: **RESOLVED** - Now uses async reqwest with background tokio runtime (like vmod-reqwest pattern). Workers still block waiting for responses, but HTTP I/O is handled asynchronously.

2. ~~**Connection pooling**~~: **RESOLVED** - Async reqwest client with proper connection pooling. Pool survives config reloads (only routing config changes, HTTP client persists).

3. ~~**Body streaming**~~: **RESOLVED** - Response bodies are streamed via tokio channels, not buffered in memory.
