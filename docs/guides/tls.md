# TLS

The Varnish Gateway terminates TLS at the listener. Certificates are sourced from
Kubernetes TLS Secrets and hot-reloaded into Varnish without a pod restart.

## Listener TLS

Declare an HTTPS listener on the Gateway with one or more `certificateRefs`:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: my-gateway
  namespace: default
spec:
  gatewayClassName: varnish
  listeners:
    - name: http
      port: 80
      protocol: HTTP
    - name: https
      port: 443
      protocol: HTTPS
      tls:
        mode: Terminate
        certificateRefs:
          - kind: Secret
            name: example-com-tls
```

The referenced Secret must be of type `kubernetes.io/tls` with `tls.crt`, PEM-encoded
certificate chain, and `tls.key`, PEM-encoded private key. This is the standard format
produced by cert-manager and by `kubectl create secret tls`.

### Supported TLS modes

Only `Terminate` is supported, the Gateway API default for HTTPS listeners. Terminate
means Varnish decrypts the TLS connection and caches/processes plaintext. `Passthrough`
is not implemented, listeners with `mode: Passthrough` will not serve traffic.

### Multiple certificates and SNI

A listener may list multiple `certificateRefs`. All referenced certificates are loaded
into Varnish, and SNI selects the matching cert at handshake time. Use this when a
single HTTPS listener fronts several hostnames with different certificates.

### Cross-namespace certificate refs

A `certificateRef` may point to a Secret in a different namespace by setting
`namespace:`. The target namespace must grant access with a `ReferenceGrant`:

```yaml
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: gateway-to-certs
  namespace: certs
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: Gateway
      namespace: default
  to:
    - group: ""
      kind: Secret
```

Without a matching ReferenceGrant the listener is rejected with a `RefNotPermitted`
condition.

## Certificate hot-reload

Certificate rotation does not require a pod restart. The flow is:

1. The operator watches the referenced TLS Secrets.
2. On change, it rebuilds the gateway's TLS Secret (`{gateway-name}-tls`), which is
   mounted into the pod at `/etc/varnish/tls/` as PEM files.
3. The chaperone's TLS reloader watches that directory with fsnotify, debounces for
   200ms, then reloads all certs atomically via `varnishadm`
   (`tls.cert.discard` + `tls.cert.load` + `tls.cert.commit`).

In-flight connections are not interrupted; new handshakes use the new cert.

## Backend TLS

Varnish can speak TLS to upstream Services and verify their certificates against a
CA bundle, but the bundle is **global to the varnishd process** — there is no
per-backend CA selection. Every TLS upstream is verified against the union of all
trusted CAs.

`BackendTLSPolicy` is accepted by the operator, and any `caCertificateRefs` that
point to ConfigMaps (key `ca.crt`) are collected into a single bundle mounted at
`/etc/varnish/backend-ca/ca-bundle.crt`. `SSL_CERT_FILE` is set to that path so
OpenSSL uses it as the trust store. Adding or removing backend TLS configuration
changes the pod's infrastructure hash and triggers a rolling restart — it is not
hot-reloadable.

Caveats:

- Per-backend CA pinning is not implemented. If Service A and Service B both need
  backend TLS with different private CAs, both CAs will be trusted for both
  services. Tracked in `varnish/gateway#22` (blocked on `varnish/varnish#26`); the
  Gateway API conformance tests for BackendTLSPolicy are skipped for this reason.
- CA refs must be ConfigMaps. Secret-based CA refs are not supported.
- SNI hostname and certificate hostname verification via `validation.hostname`
  behave as you would expect for a global trust store; they do not narrow _which_
  CA is used, only what name is verified on the presented cert.

If you need strict per-backend CA isolation today, terminate TLS at a sidecar or
service mesh in front of the workload and let the gateway speak plain HTTP to it.

## Troubleshooting

- **Listener stuck in `Programmed=False`** — check the Gateway status for TLS conditions.
  Common causes: Secret missing, wrong Secret type, malformed PEM, or a cross-namespace
  ref without a matching ReferenceGrant.
- **New cert not picked up** — confirm the Secret in the gateway namespace
  (`{gateway-name}-tls`) was updated. The operator watches source Secrets and
  regenerates this; Kubernetes then remounts the pod's `/etc/varnish/tls/` within a
  minute. The reloader acts as soon as the filesystem updates.
