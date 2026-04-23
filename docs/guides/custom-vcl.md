# Custom VCL

Attach your own VCL to a gateway. It is appended to the generated VCL and
runs alongside ghost's routing. For the merge mechanics and constraints,
see [concepts/vcl-merging.md](../concepts/vcl-merging.md).

## Attach it

1. Put your VCL in a ConfigMap:

   ```yaml
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: my-vcl
     namespace: default
   data:
     user.vcl: |
       sub vcl_recv {
           set req.http.X-Hello = "world";
       }
   ```

2. Reference it from `GatewayClassParameters`:

   ```yaml
   apiVersion: gateway.varnish-software.com/v1alpha1
   kind: GatewayClassParameters
   metadata:
     name: varnish-params
   spec:
     userVCLConfigMapRef:
       name: my-vcl
       namespace: default
       key: user.vcl   # optional, defaults to "user.vcl"
   ```

Changes to the ConfigMap hot-reload via `varnishadm` — no pod restart.
Syntax errors leave the previous VCL active; check the operator and
chaperone logs.

## Examples

### Normalize request URLs

```vcl
sub vcl_recv {
    set req.url = regsub(req.url, "\?.*$", "");   // strip query string
    set req.url = regsub(req.url, "/+$", "/");    // collapse trailing slashes
}
```

### Add or strip headers

```vcl
sub vcl_recv {
    unset req.http.Cookie;
}

sub vcl_deliver {
    set resp.http.X-Served-By = "varnish-gateway";
}
```

### Redirect HTTP to HTTPS on a specific listener

`X-Gateway-Listener` is set by ghost to the Varnish socket name
(`{proto}-{port}`). Branch on it for per-listener behavior:

```vcl
sub vcl_recv {
    if (req.http.X-Gateway-Listener == "http-80") {
        return (synth(301, "https://" + req.http.host + req.url));
    }
}

sub vcl_synth {
    if (resp.status == 301) {
        set resp.http.Location = resp.reason;
        set resp.reason = "Moved Permanently";
        return (deliver);
    }
}
```

### Route-specific logic

`X-Gateway-Route` is the HTTPRoute's `namespace/name`:

```vcl
sub vcl_recv {
    if (req.http.X-Gateway-Route == "production/api-route") {
        set req.http.X-Internal-Auth = "...";
    }
}
```

## Caveats

- User VCL is **global**. It runs on every listener and every route.
  Branch on `X-Gateway-Listener` / `X-Gateway-Route` for scoped behavior.
- `req.backend_hint` is already set when your `vcl_recv` runs — ghost
  has routed. You can override the backend but not re-enter the router.
- Do not `return(pass)` unconditionally; ghost defers pass decisions to
  a postamble that runs after your VCL. An early `return` from your
  `vcl_recv` short-circuits it.
- `vcl_synth` and `vcl_backend_error` are yours to define freely.

## See also

- [concepts/vcl-merging.md](../concepts/vcl-merging.md) — how the merge works.
- [reference/gatewayclassparameters.md](../reference/gatewayclassparameters.md) — full field reference.
