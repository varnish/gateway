# Multi-Listener Model

A Gateway can declare multiple listeners — different ports, protocols, or
hostnames. This document explains how those listeners map onto Varnish
sockets, how ghost keeps routes attached to the right listener, and what
that means for user VCL.

## Listener → socket mapping

Each Gateway listener maps to a Varnish `-a` socket named
`{proto}-{port}`:

| Listener protocol | Listener port | Varnish socket |
| ----------------- | ------------- | -------------- |
| HTTP              | 80            | `http-80`      |
| HTTPS             | 443           | `https-443`    |
| HTTP              | 3000          | `http-3000`    |

`TLS` listener share the `https-` prefix
with HTTPS. The naming rule is implemented by `listenerSocketName` in
`internal/controller/resources.go`.

The operator translates the Gateway's listeners into a `VARNISH_LISTEN`
environment variable on the gateway pod. For a Gateway with an HTTP
listener on port 80 and an HTTPS listener on port 443, the value is:

```
ghost-reload=127.0.0.1:1969,http;http-80=:80,http;https-443=:443,https
```

There is also a loopback-only listener, on `127.0.0.1:1969` used for
internal ghost reload traffic.

## Container ports equal listener ports

There is no port translation. A listener on port 3000 means Varnish binds
to `:3000` inside the container and the Kubernetes Service exposes port 3000. Hosting a listener on a non-privileged port is therefore a
configuration decision the user makes once, at the Gateway; the operator
does not remap.

The motivation is simplicity: port translation introduces a second
namespace and becomes a persistent source of confusion in logs, NetworkPolicies, and
troubleshooting. Keeping them equal means "what you declare is what binds".

## Ports collapse; hostnames don't

Multiple Gateway listeners on the same port — typically a pattern where
each listener pins a different `hostname` — collapse into a single
Varnish socket. Varnish binds once per unique `{proto,port}` pair.

Hostname isolation is not done at the socket level. It is done by
ghost's vhost routing: every route in `ghost.json` lives under a vhost
key, and ghost selects the vhost by the HTTP `Host` header with a
three-tier priority — exact match first, then wildcard
(`*.example.com`) with the longest suffix winning, then a catch-all
`*` vhost for routes with no declared hostnames. Hostnames are compared
case-insensitively. For HTTPS listeners, SNI is used at TLS handshake
time for certificate selection; it is not part of route matching.

Example: three listeners

```yaml
listeners:
  - name: a
    protocol: HTTP
    port: 80
    hostname: a.example.com
  - name: b
    protocol: HTTP
    port: 80
    hostname: b.example.com
  - name: admin
    protocol: HTTP
    port: 8080
    hostname: admin.example.com
```

produce two Varnish sockets: `http-80` and `http-8080`. Listeners `a`
and `b` share `http-80`; routing between them is by `Host` header.

## Attaching routes to specific listeners

An HTTPRoute can attach to an entire Gateway, or only to a named
listener, via the standard Gateway API `parentRefs.sectionName` field:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: api
spec:
  parentRefs:
    - name: my-gateway
      sectionName: https # listener name, not the Varnish socket name
  rules:
    - backendRefs:
        - name: api
          port: 8080
```

Without `sectionName`, the route attaches to every listener on the
Gateway that accepts it. With `sectionName`, traffic on other listeners
will not match this route. An HTTPRoute attached only to the HTTPS
listener stays invisible from the plaintext HTTP listener, even when
the `Host` header is the same.

This allows one to treat HTTP traffic different than HTTPS traffic, typically
used in HTTP->HTTPS redirects.

## Request headers for user VCL

Ghost sets two headers on every request before user VCL runs:

| Header               | Value                                            |
| -------------------- | ------------------------------------------------ |
| `X-Gateway-Listener` | Varnish socket name (e.g., `http-80`)            |
| `X-Gateway-Route`    | HTTPRoute `namespace/name` (e.g., `default/api`) |

These propagate to the backend and are available in user VCL. They are
the recommended way to branch user VCL on listener or route, since there
is no per-listener or per-route VCL (see caveat below):

```vcl
sub vcl_recv {
    // Tag requests arriving on the internal admin listener so the
    // backend can enforce stricter access rules.
    if (req.http.X-Gateway-Listener == "http-8080") {
        set req.http.X-Internal = "1";
    }
}

sub vcl_deliver {
    // Surface the route name on responses for debugging.
    set resp.http.X-Served-By-Route = req.http.X-Gateway-Route;
}
```

HTTP→HTTPS redirects and similar protocol-level concerns should be
expressed through native Gateway API features (an HTTPRoute with a
`RequestRedirect` filter), not through `X-Gateway-Listener` checks in
VCL. These headers are for behavior the Gateway API doesn't model —
listener-scoped observability, tenant tagging for backends, or
attaching debug information to responses.

The headers are informational, not internal; they reach the backend
untouched so applications can use them too.

## VCL is global across listeners

User VCL applies globally. There is no per-listener or per-route VCL
injection. Users who need listener-specific behavior must branch on
`X-Gateway-Listener` (or on `local.socket` directly) inside their
shared VCL.

This is a limitation of Varnish and might be addressed in the future.

## Changing listeners restarts pods

Listener changes — adding a port, removing one, changing protocols —
require a new `-a` argument to varnishd, which means a new pod. The
operator detects this by including the sorted list of listener socket
names in the infrastructure hash (`listenerSpecs()` in
`internal/controller/resources.go`), so a listener change flips the
`varnish.io/infra-hash` annotation on the Deployment's pod template and
Kubernetes rolls the pods. Unlike VCL or routing changes, this is not a
hot operation and the cache content will be lost.

## Internal: the ghost-reload listener

In addition to the user-facing listeners, the operator always adds a
loopback-only socket named `ghost-reload` on `127.0.0.1:1969`:

```
ghost-reload=127.0.0.1:1969,http
```

Chaperone uses this socket to drive ghost's HTTP reload endpoint
(`/.varnish-ghost/reload`). Keeping it on loopback means reload traffic
can't arrive over the user-facing listeners, and it stays plain HTTP
even in HTTPS-only gateways — avoiding TLS and certificate complications
for an internal message.

## See also

- [Architecture overview](architecture.md)
- [Reload paths](reload-paths.md) — how `ghost-reload` fits into the
  reload story
