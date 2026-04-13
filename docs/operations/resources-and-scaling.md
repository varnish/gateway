# Resources and Scaling

## Default resource requests

The main `varnish-gateway` container is created with the following default resource requests and **no limits**:

| Resource | Request | Limit     |
| -------- | ------- | --------- |
| CPU      | `100m`  | _(unset)_ |
| Memory   | `256Mi` | _(unset)_ |

The optional logging sidecar (enabled via `spec.logging`) has its own fixed resource profile and is not affected by overrides on the main container.

## Why no limits?

The absence of limits is deliberate. Varnish's working set is dominated by its configured cache storage (`-s malloc,SIZE` or `-s file,...`), and memory usage can drift above the configured storage size because of transient workspace allocations, session state, and thread pool stacks. A memory limit set too close to the cache size leads to the kernel OOM-killing varnishd under load, which is far more disruptive than a pod running at sustained high memory usage:

- The entire cache is lost when the pod restarts.
- In-flight requests are dropped.
- On cold start the pod hammers origins until the cache refills (see [horizontal-pod-autoscaling.md](horizontal-pod-autoscaling.md#cache-warmth-on-scale-up)).

CPU limits are likewise omitted because CPU throttling under Linux CFS produces latency spikes that are hard to diagnose and rarely what operators actually want — a noisy-neighbour concern is better solved with requests and node sizing.

If you need a ceiling (multi-tenant clusters, strict bin-packing, chargeback) set both `requests` and `limits` via `GatewayClassParameters.spec.resources`, and size the memory limit **above** your configured Varnish storage size plus headroom for transient allocations. A rule of thumb: memory limit ≥ storage size × 1.3, and never below storage size + 256Mi.

## Overriding defaults

Set `spec.resources` on the `GatewayClassParameters` referenced by your `GatewayClass`. The field is a standard Kubernetes [`ResourceRequirements`](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#resourcerequirements-v1-core) and is applied to the main `varnish-gateway` container verbatim — setting it replaces the defaults entirely, so include `requests` even if you only care about `limits`.

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-params
spec:
  resources:
    requests:
      cpu: "500m"
      memory: "1Gi"
    limits:
      memory: "2Gi"
  varnishdExtraArgs:
    - "-s"
    - "malloc,1g"
```

In this example the `malloc,1g` storage fits comfortably under the `2Gi` memory limit, leaving headroom for workspaces and thread stacks.

A change to `spec.resources` updates the Deployment pod template and triggers a rolling restart.

### Sizing guidance

- **CPU request.** The default `100m` is a placeholder for small dev workloads. For production, size the request to your expected steady-state CPU usage — this is also what HPA's `averageUtilization` metric is computed against (see [horizontal-pod-autoscaling.md](horizontal-pod-autoscaling.md#choosing-a-metric)). TLS termination and HTTP/2 meaningfully increase CPU per request; measure before settling.
- **Memory request.** Set to your configured storage size plus a small buffer (e.g., `malloc,1g` → `memory: 1.2Gi` request). The scheduler uses requests, not limits, to place pods on nodes.
- **Thread pool memory.** Each worker thread consumes its stack (default 80k, 160k with the recommended `thread_pool_stack=160k`). A Varnish with `thread_pool_max=5000` across 2 pools can allocate ~1.5Gi of thread stacks under peak load — include this in your memory sizing.

## Scaling

Replica count is not managed by the operator — it deliberately leaves `spec.replicas` unset on the Deployment so that an HPA, KEDA, or a manual `kubectl scale` can own it without being overwritten. The default when nothing else sets it is a single replica.

For the full model — how the operator avoids fighting autoscalers, recommended metrics, and interactions with rolling restarts — see [horizontal-pod-autoscaling.md](horizontal-pod-autoscaling.md).

### Vertical vs horizontal

Varnish scales well in both directions, but they have different trade-offs:

- **Vertical (bigger pods, more cache).** The cache lives per-pod; doubling a pod's memory doubles its working set and hit ratio. A small number of large pods maximizes cache efficiency because each request has a high probability of landing on a pod that has seen the URL before.
- **Horizontal (more pods, same size).** Doubling replicas roughly doubles CPU and connection capacity but **not** the cache footprint per pod. The Service load-balances uniformly across ready pods, so each pod sees 1/N of traffic and its cache fills with 1/N of the working set. Cache hit ratio can degrade as N grows because unique URLs are spread thinner.

The practical implication: scale vertically first (until a single pod is large enough to hold your hot working set), then horizontally for CPU and availability. Running ten 256Mi pods is almost always worse than running two 1Gi pods for the same total memory budget.

### Interaction with PodDisruptionBudget

A PodDisruptionBudget caps how many Gateway pods can be voluntarily evicted at once (node drains, rolling restarts). PDBs are opt-in because the default replica count is `1` and a `minAvailable: 1` PDB on a single-replica Deployment blocks every drain indefinitely.

The two controls compose as follows:

- An HPA with `minReplicas: 2` plus a percentage PDB (`maxUnavailable: 25%`) is the usual production shape — the PDB scales with the replica count and never forces the fleet below the HPA floor.
- Infrastructure changes (listener edits, `varnishdExtraArgs` bumps, image changes) trigger a rolling restart through the `varnish.io/infra-hash` annotation. The restart respects the PDB, so a tight PDB on a small replica count extends upgrade duration.

See [pod-disruption-budgets.md](pod-disruption-budgets.md) for the full interaction model and recommended values.

## See also

- [GatewayClassParameters reference](../reference/gatewayclassparameters.md)
- [Horizontal Pod Autoscaling](horizontal-pod-autoscaling.md)
- [Pod Disruption Budgets](pod-disruption-budgets.md)
