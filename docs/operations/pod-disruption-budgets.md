# Pod Disruption Budgets

A [PodDisruptionBudget](https://kubernetes.io/docs/concepts/workloads/pods/disruptions/#pod-disruption-budgets) (PDB) limits how many Gateway pods can be voluntarily evicted at the same time — by `kubectl drain`, node autoscaler scale-down, or rolling updates. The operator supports creating a PDB per Gateway, but it is _opt-in_.

## Why opt-in?

The default replica count for a Gateway Deployment is `1`. A PDB with `minAvailable: 1` on a single-replica Deployment blocks _all_ voluntary evictions indefinitely — node drains hang, cluster autoscaler cannot reclaim the node, and rolling upgrades stall.

Only enable a PDB if you have also arranged for more than one replica, either via an HPA (see [horizontal-pod-autoscaling.md](horizontal-pod-autoscaling.md)) or by manually scaling the Deployment.

## Enabling a PDB

Set `spec.podDisruptionBudget` on the `GatewayClassParameters` referenced by your GatewayClass. Exactly one of `minAvailable` or `maxUnavailable` must be set — the semantics mirror Kubernetes' [`PodDisruptionBudgetSpec`](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#poddisruptionbudgetspec-v1-policy).

```yaml
apiVersion: gateway.varnish-software.com/v1alpha1
kind: GatewayClassParameters
metadata:
  name: varnish-params
  namespace: varnish-gateway-system
spec:
  podDisruptionBudget:
    minAvailable: 1
```

The operator creates one PDB per Gateway in the Gateway's namespace, named after the Gateway and selecting the Gateway's pods. It is owned by the Gateway and garbage-collected on deletion.

## Recommended values

There is no single correct value — it depends on your replica count and your tolerance for reduced capacity during disruptions. Some guidance:

- **`minAvailable: N-1`** (absolute) — keep all but one pod available. Safest for small replica counts (2–4). Works well when pods are roughly equal in cache fill.
- **`maxUnavailable: "25%"`** (percentage) — scales naturally with HPA. Good default once replicas are in the double digits.
- **`minAvailable: "50%"`** — conservative; keeps at least half the fleet serving. Use when each pod carries a meaningful share of the working set and cache loss on drain matters.

Avoid `minAvailable: <replicas>` or `maxUnavailable: 0` — both disable voluntary disruption entirely, which is almost never what you want.

## Interaction with HPA

PDB is evaluated against _currently ready_ pods, not the HPA target. During a scale-down, the HPA removes pods one at a time and the PDB is not consulted. HPA-triggered removals are not voluntary disruptions from the PDB's perspective. PDB does, however, gate node drains and rolling restarts even while the HPA is active.

If your PDB uses a percentage, it is computed against the current replica count at the time of eviction, so it tracks HPA scaling automatically. Absolute `minAvailable` values do not — a `minAvailable: 3` with a replica count of 3 blocks drains just like the single-replica case.

## Rolling restarts

Rolling restarts — triggered by infrastructure changes such as listener edits, image bumps, or `varnishdExtraArgs` changes (see the `varnish.io/infra-hash` annotation in the [architecture overview](../../CLAUDE.md)) — respect the PDB. With `minAvailable: 1` and two replicas, a rolling restart proceeds one pod at a time. Keep this in mind when sizing: a tight PDB on a small replica count extends upgrade duration.

## Disabling a PDB

Remove `spec.podDisruptionBudget` from the `GatewayClassParameters` (or set it to `null`). The operator deletes the PDB it owns on the next reconcile. This is important: a stale `minAvailable: 1` left behind after scaling down to a single replica would silently block future drains.

## See also

- [GatewayClassParameters reference](../reference/gatewayclassparameters.md)
- [Horizontal Pod Autoscaling](horizontal-pod-autoscaling.md)
- [Resources and Scaling](resources-and-scaling.md)
