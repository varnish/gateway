# HTTPRoute Filters Reference

## Overview

[HTTPRoute filters](https://gateway-api.sigs.k8s.io/api-types/httproute/#filters-optional)
modify requests and responses as they pass through a route rule. In Varnish
Gateway, filters are handled in-band by the ghost VMOD — there is no VCL
generation involved, and filter changes apply on the same millisecond-scale
hot-reload path as routing changes.

A filter is attached to a rule via `spec.rules[].filters`:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: my-route
spec:
  rules:
    - filters:
        - type: RequestHeaderModifier
          requestHeaderModifier:
            set:
              - name: X-Env
                value: prod
      backendRefs:
        - name: my-service
          port: 8080
```

## Supported filters

| Filter | Gateway API tier | Notes |
|--------|------------------|-------|
| `RequestHeaderModifier` | Core | Set/add/remove request headers before backend selection. |
| `RequestRedirect` | Core | Generate a 3xx redirect (scheme, hostname, path, port, status code). |
| `ResponseHeaderModifier` | Extended | Set/add/remove response headers on the way back to the client. |
| `URLRewrite` | Extended | Rewrite hostname and/or path (`ReplaceFullPath`, `ReplacePrefixMatch`). |

Both **Core** filters are implemented, so Varnish Gateway meets the Gateway API
Core conformance bar for HTTPRoute filtering. The two Extended filters above are
also implemented.

## Not supported

| Filter | Gateway API tier | Behaviour |
|--------|------------------|-----------|
| `RequestMirror` | Extended | Accepted by the CRD, **silently ignored** — no request is mirrored. |
| `ExtensionRef` | Implementation-specific | Accepted by the CRD, **silently ignored**. |

These filter types are part of the standard Gateway API CRD schema that ships
with Varnish Gateway, so the API server will accept an HTTPRoute that uses them
and the route will still be marked `Accepted`. **Varnish Gateway does not act on
them and does not set a status condition to flag that they were dropped.** If
you add one of these filters and the expected behaviour doesn't happen, that is
why.

Both are Extended/implementation-specific in the Gateway API and are not
required for Core conformance, which is why they are not implemented.

### Need behaviour a filter doesn't cover?

Reach for [custom VCL](../guides/custom-vcl.md). Request and response
manipulation that goes beyond the supported filters — including approximating a
mirror with a side request — can be expressed in VCL, with the usual caveat that
VCL applies globally rather than per-route (branch on the `X-Gateway-Route`
header when you need route-specific logic).
