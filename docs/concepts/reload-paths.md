# Reload Paths

A gateway's configuration is not monolithic. Routes, backends, user VCL,
listeners, and the container image all change on different clocks, and
propagating every change through a full pod restart would be wasteful —
and, for a cache, destructive. Varnish Gateway therefore has three
distinct propagation paths, each matched to the smallest unit of change
that actually needs to move.

| Change                               | Path                  | Mechanism                                         | Cache impact                |
| ------------------------------------ | --------------------- | ------------------------------------------------- | --------------------------- |
| HTTPRoute, Service endpoints         | Ghost HTTP reload     | Chaperone POSTs to `/.varnish-ghost/reload`       | None — cache preserved      |
| User VCL, `VarnishCachePolicy`       | varnishadm VCL reload | Chaperone issues `vcl.load` + `vcl.use`           | None — cache preserved      |
| Listener ports, image, varnishd args | Rolling pod restart   | Infrastructure hash annotation change on pod spec | Cache lost on each pod roll |

The first two are in-process and essentially free. The third is a
Kubernetes rolling update and costs cache content. Getting a change onto
the cheapest path that can carry it is the whole point of this design.

## The infrastructure hash is the decision boundary

Every reconcile of the Gateway controller computes an `InfrastructureConfig`
(see `internal/controller/infra_hash.go`) covering exactly the inputs
that varnishd reads at startup and cannot reconfigure on the fly:
image, `varnishdExtraArgs`, logging configuration, image pull secrets,
the sorted list of listener sockets, extra volumes/mounts/init
containers, and whether backend TLS is in use. Its SHA-256 is written
to the Deployment's pod template as `varnish.io/infra-hash`.

If the hash doesn't change, the pod template doesn't change, and
Kubernetes leaves pods alone. If it does change, Kubernetes performs a
rolling update. This is the single signal that decides "config change"
versus "pod-level change", there is no other trigger for pod churn.

Everything outside the hash, the contents of `main.vcl`, the contents
of `routing.json`, the EndpointSlices referenced by routes, flows
through the two hot-reload paths instead.

## Ghost HTTP reload

Routing data (HTTPRoutes and the Services they target) changes
frequently: a deployment rollout rewrites an EndpointSlice every time a
pod comes or goes. These changes must not restart `varnishd`.

### What triggers it

Chaperone watches two sources:

1. The gateway's ConfigMap, specifically the `routing.json` key.
2. EndpointSlices for every Service referenced by the current
   `routing.json`.

Either side changing causes chaperone to regenerate `ghost.json` (the
merged tree of routes and resolved pod addresses) and write it to the
varnish work directory. Writes use content-based deduplication: if the
regenerated file is byte-identical to the previous one, no reload is
issued.

### How the reload is delivered

Chaperone sends an HTTP `GET /.varnish-ghost/reload` over the loopback
`ghost-reload` socket (`127.0.0.1:1969`). The request is handled by
the ghost VMOD inside varnishd, which reparses `ghost.json` and swaps
its internal router atomically. In-flight requests already past the
routing decision finish against the old router; new requests pick up
the new one. The client (`internal/reload/client.go`) reads any error
back from the `x-ghost-error` response header.

The reload endpoint lives on a loopback-only socket by design: it must
be reachable from chaperone but never from outside the pod, and it must
work even in HTTPS-only gateways without dragging TLS and certificates
into an internal control-plane message.

## varnishadm VCL reload

User VCL, the generated preamble/postamble, and the VCL produced by
`VarnishCachePolicy` attachments all live in the `main.vcl` key of the
gateway's ConfigMap.

### What triggers it

Chaperone watches the ConfigMap via a Kubernetes informer and
specifically tracks `main.vcl`. The same content-based deduplication
applies: identical bytes, no reload.

### How the reload is delivered

The VCL reloader (`internal/vcl/reloader.go`) speaks the varnishadm
protocol directly (`internal/varnishadm/`). The sequence is:

1. `vcl.load <name> <path>` — compile and load the new VCL under a
   generated name (see `generateVCLName`).
2. `vcl.use <name>` — atomically switch the active VCL to the new
   version. Existing transactions continue on the previous VCL until
   they complete.
3. Garbage-collect older, now-unreferenced VCL versions so loaded
   configurations don't accumulate.

Compilation happens before `vcl.use` runs, so a broken VCL surfaces as
a load error and never takes effect — the previous VCL stays active
and traffic is unaffected.

## Rolling restart

Listener changes, image upgrades, `varnishdExtraArgs` changes, and
other fields inside `InfrastructureConfig` require a new varnishd
process. The operator writes a new infrastructure hash onto the
Deployment's pod template, the Deployment's rolling update strategy
takes over, and Kubernetes replaces pods one at a time.

This is the only path that loses cache content. Each replacement pod
starts with an empty cache and warms it from origin traffic. For
listener changes specifically, the motivation is mechanical: the `-a`
flags to varnishd are set at process start and cannot be reconfigured,
so a listener change must be a new process.

## Who watches what

```
Gateway / GatewayClassParameters / user-VCL ConfigMap
        │
        ▼ (Gateway controller)
  ┌───────────────────────┬──────────────────────────────────┐
  │ ConfigMap: main.vcl   │ Deployment: varnish.io/infra-hash│
  └───────────┬───────────┴────────────────┬─────────────────┘
              │                            │
              │ (chaperone ConfigMap       │ (kube-controller-manager
              │  informer, main.vcl diff)  │  rolling update)
              ▼                            ▼
         varnishadm                  new pod lifecycle
         vcl.load / vcl.use

HTTPRoute                                   EndpointSlices
        │                                         │
        ▼ (HTTPRoute controller)                  │
  ┌────────────────────────┐                      │
  │ ConfigMap: routing.json│                      │
  └───────────┬────────────┘                      │
              │                                   │
              └──────────┬────────────────────────┘
                         ▼ (chaperone)
                  ghost.json regenerated
                         │
                         ▼
                GET /.varnish-ghost/reload
                (loopback ghost-reload socket)
```

The two reconcilers write into disjoint keys of the same ConfigMap, and
the two chaperone watchers read disjoint keys. A VCL change is invisible
to the ghost reload path, a routing change is invisible to the VCL
reload path, and neither can trigger a pod restart without also
flipping the infrastructure hash.

## Failure handling

Both hot-reload paths are best-effort: if a reload fails, the previous
configuration remains active and chaperone surfaces the error through
logs and the dashboard event bus. The next successful ConfigMap or
EndpointSlice event retries the reload from scratch, so transient
failures heal on the next update; persistent failures (e.g., a VCL
syntax error introduced by user VCL) keep the gateway serving the last
good configuration until the user corrects the input.

A rolling restart that produces an unschedulable or crashing pod is
handled by the Deployment's rolling update strategy in the usual way
— the previous ReplicaSet stays up while the new one fails to make
progress, and `kubectl rollout status` surfaces the failure.

## See also

- [Architecture overview](architecture.md) — where the reload paths sit
  in the overall data flow
- [VCL merging](vcl-merging.md) — what goes into `main.vcl` before it
  reaches the reloader
- [Multi-listener model](multi-listener.md) — why listener changes are
  on the restart path and the role of the `ghost-reload` socket
- [Caching model](caching-model.md) — cache lifetime across the
  different reload paths
