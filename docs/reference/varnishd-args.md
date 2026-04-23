# varnishd Arguments

The `varnishdExtraArgs` field on `GatewayClassParameters` lets you pass
additional command-line arguments to varnishd. This page documents what
the operator sets by default, which flags are protected, and where to
find the upstream reference for tuning.

## Default arguments

The operator and chaperone set these arguments on every varnishd
process. They cannot be changed via `varnishdExtraArgs`:

| Flag      | Value                            | Purpose                                    |
| --------- | -------------------------------- | ------------------------------------------ |
| `-S`      | `<workdir>/secret`               | Admin authentication secret                |
| `-M`      | `localhost:<admin-port>`         | Reverse-mode admin socket for varnishadm   |
| `-F`      |                                  | Run in foreground                          |
| `-f`      | `""`                             | Empty — VCL is loaded later via varnishadm |
| `-a`      | per listener                     | One `-a` per Gateway listener plus the internal ghost-reload socket |
| `-s`      | `malloc,256m`                    | Cache storage, configurable via `VARNISH_STORAGE` |
| `-t`      | `0`                              | Default TTL of 0 — disables caching unless the origin sends Cache-Control headers |

The `-t 0` default exists to prevent stale content in blue/green
deployments and similar patterns where caching without explicit policy
would be surprising. It can be overridden by passing `-t` in
`varnishdExtraArgs` — varnishd uses the last value when a flag appears
more than once.

## Protected flags

The following flags are rejected if they appear in `varnishdExtraArgs`:

- `-M` — admin port, managed by chaperone
- `-S` — secret file, managed by chaperone
- `-F` — foreground mode, required for container operation
- `-f` — VCL file, managed by the reload pipeline
- `-n` — working directory, managed by chaperone

Attempting to pass a protected flag causes the varnishd process to fail
at startup with an error in the chaperone logs.

## Passing extra arguments

Set `spec.varnishdExtraArgs` on the `GatewayClassParameters` referenced
by your GatewayClass. Each array element is a separate argument:

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-params
spec:
  varnishdExtraArgs:
    - "-p"
    - "thread_pools=4"
    - "-p"
    - "thread_pool_stack=160k"
```

Arguments that use semicolons as separators are passed through correctly —
the operator joins elements with semicolons internally, but each array
element maps to one shell argument on the varnishd command line.

Changing `varnishdExtraArgs` updates the infrastructure hash and
triggers a rolling restart of the gateway pods.

## Common tuning parameters

These are starting points, not recommendations. Measure your workload
before changing defaults.

| Parameter                  | Default   | Notes                                                        |
| -------------------------- | --------- | ------------------------------------------------------------ |
| `thread_pools`             | 2         | One per CPU core is a common starting point                  |
| `thread_pool_min`          | 100       | Minimum threads per pool                                     |
| `thread_pool_max`          | 5000      | Maximum threads per pool                                     |
| `thread_pool_stack`        | 80k       | Stack per thread. 160k recommended when using regex matching |
| `workspace_client`         | 64k       | Per-request workspace for client side                        |
| `workspace_backend`        | 64k       | Per-request workspace for backend side                       |

Thread pool memory adds up: `thread_pool_max` x `thread_pools` x
`thread_pool_stack` under peak load. With the values above and a 160k
stack, that is 5000 x 2 x 160k = ~1.5 GiB of thread stacks alone.
Include this in your memory sizing — see
[resources-and-scaling.md](../operations/resources-and-scaling.md).

## Storage

The default storage backend is `malloc,256m` — in-memory storage backed
by malloc. Override it via `varnishdExtraArgs` or the `VARNISH_STORAGE`
environment variable on the chaperone container.

Common options:

- `-s malloc,SIZE` — in-memory. Fast. Contents lost on restart.
- `-s file,PATH,SIZE` — file-backed via mmap. Survives OOM kills but
  not pod restarts in most Kubernetes configurations since the file
  lives on an emptyDir volume.

## Upstream reference

The authoritative reference for all varnishd flags and runtime
parameters is the Varnish documentation:

- [varnishd(1)](https://varnish-cache.org/docs/trunk/reference/varnishd.html) — command-line flags
- [varnishd parameters](https://varnish-cache.org/docs/trunk/reference/varnishd.html#run-time-parameters) — runtime parameters set via `-p`

## See also

- [GatewayClassParameters reference](gatewayclassparameters.md) — the full CRD spec
- [Resources and Scaling](../operations/resources-and-scaling.md) — memory and CPU sizing guidance
