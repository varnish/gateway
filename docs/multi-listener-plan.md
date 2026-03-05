# Multi-Listener Support - Implementation Plan

## Overview

Support multiple HTTP/HTTPS listeners on a Gateway, each on a different port. Varnish
listeners map 1:1 with unique ports. Multiple Gateway listeners sharing a port collapse
into a single Varnish socket, with hostname-based isolation handled by ghost.

## Design Decisions

- **Container ports = listener ports**: No translation layer. Listener port 3000 means
  Varnish binds to `:3000` inside the container. Listener changes require a new pod,
  so no conflict concerns.
- **Varnish socket naming**: `{proto}-{port}` (e.g., `http-3000`, `https-4000`).
  Encodes both protocol and port. This allows ghost to derive the scheme from the
  listener name for redirect filters (e.g., `listener.starts_with("https")`).
- **Same-port listeners**: Collapsed into one Varnish socket. Ghost provides hostname
  isolation (already implemented). This enables `GatewayHTTPListenerIsolation` conformance.
- **Routes without sectionName**: `listener: null` in ghost.json means "match on any socket".
  No route duplication.
- **Request headers**: Ghost sets `X-Gateway-Listener` and `X-Gateway-Route` on `req`
  before user VCL runs. These propagate to the backend. Users can use them in VCL for
  per-listener logic. Not stripped — documented as intentional.

## VCL Caveat

User VCL applies globally across all listeners. There is no per-listener or per-route
VCL. Users who need listener-specific behavior should branch on `X-Gateway-Listener`
in their VCL.

## Changes by Component

### 1. Operator — routing.json

Add `listener` field to each route entry. Value is the Varnish socket name (`{proto}-{port}`).

```json
{
  "version": 3,
  "vhosts": {
    "example.com": {
      "routes": [
        {
          "hostname": "example.com",
          "listener": "http-3000",
          "path_match": {"type": "PathPrefix", "value": "/"},
          "service": "web",
          "namespace": "default",
          "port": 8080,
          "weight": 100,
          "priority": 10300,
          "rule_index": 0
        }
      ]
    }
  }
}
```

Routes with no `sectionName` (attaching to all listeners) get `"listener": null`.

### 2. Operator — resources.go

**Container spec**:
- `VARNISH_LISTEN` becomes a list of named listeners:
  `http-3000=:3000,http;http-4000=:4000,http`
  (semicolon-separated, parsed by chaperone)
- Container ports generated from unique listener ports
- Service ports map each Gateway listener port to the same container port

**hasTLS detection**: Unchanged — if any listener is HTTPS, add TLS env vars and the
HTTPS container port.

### 3. Operator — httproute_controller.go

`filterRouteHostnames` and `effectiveHostnames` already track which listener a route
targets via `sectionName`. The `listener` field (Varnish socket name) needs to be
derived from the matched listener's port and included in the routing config output.

Mapping: for each parentRef with a sectionName, find the listener, compute `{proto}-{port}`.
For parentRefs without sectionName, set `listener: null`.

### 4. Chaperone

Pass through the `listener` field when merging routing.json with EndpointSlice data
into ghost.json. No new logic needed — just preserve the field.

### 5. Ghost — ghost.json

Add `listener` field to route entries:

```json
{
  "version": 3,
  "vhosts": {
    "example.com": {
      "routes": [
        {
          "listener": "http-3000",
          "path_match": {"type": "PathPrefix", "value": "/"},
          "backend_groups": [...],
          "priority": 10300,
          "rule_index": 0
        }
      ]
    }
  }
}
```

### 6. Ghost — route matching

During request routing:
1. Read `local.socket` to get the Varnish socket name
2. Filter candidate routes: include routes where `listener` matches `local.socket`,
   or `listener` is null
3. Proceed with existing hostname + path + header matching

### 7. Ghost — request headers

After route selection, set on `req`:
- `X-Gateway-Listener: {gateway-listener-name}` (the original Gateway listener name,
  not the Varnish socket name)
- `X-Gateway-Route: {httproute-name}`

Both headers use the Varnish socket name (`http-3000`), not the original Gateway
listener name. This is simpler to implement, reason about, and document.

### 8. Conformance

Add `features.SupportGatewayHTTPListenerIsolation` to `conformance/conformance_test.go`.

Potentially also `features.SupportHTTPRouteParentRefPort` (HTTPRouteListenerPortMatching)
if we can match routes by the original listener port.

## Migration

Bump routing.json and ghost.json to version 3. Ghost should handle version 2 (no
listener field) by treating all routes as `listener: null` for backwards compatibility
during rolling updates.

## Open Questions

- Should we validate that listener names are unique across the Gateway? (The spec
  requires this, but we should verify we enforce it.)
- What status condition to set if two listeners specify the same port with conflicting
  protocols (HTTP vs HTTPS)?
- `X-Gateway-Listener` uses the Varnish socket name (`http-3000`), not the Gateway
  listener name. Simpler and unambiguous.
