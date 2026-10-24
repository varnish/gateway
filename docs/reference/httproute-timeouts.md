# HTTPRoute Timeouts

`HTTPRouteRule.timeouts` bounds how long the gateway waits on a backend.

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: api-route
spec:
  parentRefs:
    - name: my-gateway
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /slow-api
      backendRefs:
        - name: api-service
          port: 8080
      timeouts:
        backendRequest: 5s
```

A route whose backend exceeds the timeout returns **504 Gateway Timeout**.

## `request` and `backendRequest`

| Field | Gateway API scope | Varnish behaviour |
|---|---|---|
| `backendRequest` | Gateway sends request headers → complete response received | `connect_timeout` + `first_byte_timeout` + `between_bytes_timeout` |
| `request` | Client request received → response fully sent | Same as `backendRequest` |

`request` is applied as an alias for `backendRequest`. When both are set the
tighter value wins — normally `backendRequest`, since the spec requires it to be
no larger than `request`.

## How it maps to Varnish

Varnish backends are pooled by `address:port`, so they cannot carry per-route
timeouts. Ghost bridges the value on the matched route to the fetch instead:

```
routing.json → ghost.json → ghost sets X-Ghost-Timeout on the request
  → vcl_backend_fetch sets bereq.connect_timeout + bereq.first_byte_timeout
    + bereq.between_bytes_timeout
  → vcl_backend_error reports 504 instead of 503
```

All three are set to the same value, so the route's budget bounds establishing the
connection as well as waiting for bytes. Gateway API scopes `backendRequest` to
after request headers are sent, but the bound users actually want is their own
wall-clock wait. A route with a tight timeout to an off-cluster backend
(ExternalName, or one using TLS) must complete the TCP and TLS handshake inside
that budget.

## Caveats

**Any fetch failure on a timeout route reports 504.** Varnish exposes no failure
reason in `vcl_backend_error`, so a refused connection on a route with
`backendRequest` set also returns 504 rather than 503. Routes without a timeout
are unaffected and keep 503.

**There is no total-request cap.** Gateway API scopes `request` to the whole
client request-response cycle and `backendRequest` to the complete response, but
Varnish has neither bound — the clock necessarily starts at the backend fetch, and
time spent reading the client request body or streaming back to a slow client is
not counted.

**Streaming responses are cut mid-body.** `between_bytes_timeout` bounds every
gap between response body bytes, not the total elapsed time, so a long-lived SSE
or streaming response on a route with a short `backendRequest` is terminated —
and once headers are delivered the 504 flip no longer applies, so the client sees
a truncated 200. Do not set `backendRequest` on streaming routes.

**`0s` falls back to varnishd defaults.** Gateway API defines `0s` as "disable
the timeout". Varnish always applies its fetch timeouts, so a disabled route
inherits the global varnishd values (`connect_timeout` 3.5s, `first_byte_timeout`
and `between_bytes_timeout` 60s each) rather than running unbounded. Setting only
one of the two fields to `0s` leaves the other in force.

**No retries.** A timed-out fetch fails immediately; Varnish only retries when
VCL calls `return (retry)`.

## Interaction with user VCL

The 504 flip lives in the gateway postamble, which is concatenated *after* user
VCL. A user-supplied `vcl_backend_error` that ends with `return (deliver)`
terminates VCL execution before the postamble runs, and the response keeps
Varnish's 503. Branch on `bereq.http.X-Ghost-Timeout` if you need custom
handling for timed-out routes:

```vcl
sub vcl_backend_error {
    if (bereq.http.X-Ghost-Timeout) {
        set beresp.status = 504;
        set beresp.http.Content-Type = "application/json";
        set beresp.body = {"{"error": "upstream timeout"}"};
        return (deliver);
    }
}
```

`X-Ghost-Timeout` is stripped from client requests in `vcl_recv` before routing,
so it cannot be spoofed. Like the cache policy headers, it stays on `bereq`
through the fetch and is therefore visible to the backend.
