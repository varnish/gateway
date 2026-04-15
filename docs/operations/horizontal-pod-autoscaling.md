# Horizontal Pod Autoscaling

The operator does not set `spec.replicas` on Gateway Deployments. This leaves replica
count ownership to whatever external controller you choose. A
[HorizontalPodAutoscaler](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/)
(HPA), KEDA, or a manual `kubectl scale`. The operator will not fight your autoscaler.

## How it works

When the operator builds the desired Deployment it deliberately leaves `Spec.Replicas`
unset. Kubernetes defaults a nil `Replicas` field to `1` on create, so a fresh Gateway
starts with a single pod. On subsequent reconciles the operator only writes
`Spec.Template` and `Spec.Strategy` — never `Spec.Replicas` — so whatever replica count
the HPA has settled on is preserved across reconciles, user VCL changes, image bumps,
and any other controller activity.

This also means you can `kubectl scale deployment/<gateway-name>` for ad-hoc scaling
without the operator reverting it.

## Sample HPA manifest

The Deployment created by the operator has the same name and namespace as its Gateway.
The following HPA targets a Gateway named `my-gateway` in the `default` namespace:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: my-gateway
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: my-gateway
  minReplicas: 2
  maxReplicas: 10
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 70
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 300
    scaleUp:
      stabilizationWindowSeconds: 60
```

`minReplicas: 2` is a deliberate floor — it lets you combine the HPA with a
[PodDisruptionBudget](pod-disruption-budgets.md) without blocking node drains. A
Gateway at `minReplicas: 1` combined with `minAvailable: 1` will stall every voluntary
eviction on that node.

## Choosing a metric

**CPU utilization (Resource metric).** The simplest option and the only one that works
out of the box. Varnish is largely CPU-bound once the working set fits in cache —
decompression, TLS, VCL, and ghost's route matching all consume CPU — so CPU tracks
load reasonably well for most workloads. It requires `metrics-server` to be installed
in the cluster and resource _requests_ to be set on the pod (the operator sets a
default CPU request of `100m`, see [resources-and-scaling.md](resources-and-scaling.md)).

CPU utilization is a ratio of `usage / request`, so if you raise the CPU request via
`GatewayClassParameters.spec.resources`, adjust `averageUtilization` accordingly — a
70% target means something very different against a `100m` request than against a `2`
request.

**Memory utilization.** Generally a poor fit. Varnish's cache fills to its configured
storage size and stays there; memory usage is close to constant regardless of request
rate. Autoscaling on memory will produce a fleet sized to the cache, not to the load.

**Custom metrics.** Requests per second (RPS), p95 latency, or backend connection
count track real load better than CPU. These require a custom metrics adapter
(Prometheus Adapter, KEDA, etc.) and are out of scope for this document — point the
adapter at Varnish's stats (`varnishstat`) or at whatever ingress metric your
monitoring stack exposes. See [observability.md](observability.md) for available
signals.

## Interactions

**PodDisruptionBudget.** See [pod-disruption-budgets.md](pod-disruption-budgets.md).
A percentage-based PDB (`maxUnavailable: 25%`) tracks HPA-driven scale automatically;
an absolute `minAvailable` does not.

**Rolling restarts.** Infrastructure changes (listener edits, `varnishdExtraArgs`
changes, image bumps) trigger a rolling restart via the `varnish.io/infra-hash`
annotation. The restart replaces pods one-by-one against the current HPA-chosen
replica count — the HPA is not consulted mid-restart, and replicas are not reset to
a default.

### Cache warmth on scale-up

Newly added pods start with an empty cache and will
generate a burst of backend traffic as they fill. Size your `scaleUp`
`stabilizationWindowSeconds` and your backend capacity accordingly. The Gateway
Service load-balances uniformly across ready pods, so a cold pod is just as likely
to receive any given request as a warm one.

## See also

- [Resources and Scaling](resources-and-scaling.md)
- [Pod Disruption Budgets](pod-disruption-budgets.md)
- [GatewayClassParameters reference](../reference/gatewayclassparameters.md)
