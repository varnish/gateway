# Upgrades

This page covers upgrading an existing Varnish Gateway installation: what changes between versions, what restarts during the upgrade, and how to roll back if something goes wrong. For a first-time install, see [Installation](../getting-started/installation.md).

## What an upgrade changes

A release ships three things that can move independently in principle but are always pinned together in a chart version:

1. **The operator binary** — runs in `varnish-gateway-system` and reconciles Gateway/HTTPRoute resources.
2. **The gateway image** (`ghcr.io/varnish/gateway-chaperone`) — the combined varnishd + ghost VMOD + chaperone image that runs as the data plane in each Gateway's pods. If you are running a [custom image with additional VMODs](custom-vmods.md), you must rebuild it against the new base tag and upgrade both together — the VMOD ABI is tied to the varnishd version.
3. **The Varnish-specific CRDs** — `GatewayClassParameters`, `VarnishCacheInvalidation`, `VarnishCachePolicy`.

The Gateway API CRDs (`Gateway`, `HTTPRoute`, etc.) are upstream and upgraded separately — see [Gateway API releases](https://github.com/kubernetes-sigs/gateway-api/releases).

## Version compatibility

- **Operator and gateway image must match.** The operator sets the gateway image via the `GATEWAY_IMAGE` environment variable at install time; the Helm chart wires this from `chaperone.image.repository` + `chaperone.image.tag`, with the tag defaulting to the chart's `appVersion`. Individual GatewayClasses can override this via `GatewayClassParameters.spec.image` — typically to load [custom VMODs](custom-vmods.md). In both cases, the image must be built from a matching release of the stock gateway image; ghost's reload protocol and chaperone's config schema can change between minor versions.
- **CRD changes are called out in release notes.** When the CRD schema changes, the release notes include an explicit "CRD changes" section and a `kubectl apply` snippet. CRDs are strictly additive within a minor version.
- **Gateway API version.** Each release is tested against a specific Gateway API version (currently v1.4.0). Newer Gateway API releases usually work but are not guaranteed; older releases may be missing types the operator consumes.

## Upgrade with Helm

Helm does not touch CRDs on `helm upgrade` — this is a deliberate [Helm design choice](https://helm.sh/docs/chart_best_practices/custom_resource_definitions/) to avoid destroying custom resource data. If the release notes indicate CRD changes, apply them manually first:

```bash
helm pull oci://ghcr.io/varnish/charts/varnish-gateway --version vX.Y.Z --untar
kubectl apply -f varnish-gateway/crds/
```

Applying CRDs when nothing has changed is a no-op, so if you are unsure, apply them.

Then upgrade the release:

```bash
helm upgrade varnish-gateway oci://ghcr.io/varnish/charts/varnish-gateway \
  --version vX.Y.Z \
  --namespace varnish-gateway-system
```

Always pin `--version` in production. Omitting it pulls `latest`, which makes the upgrade non-reproducible and surprises rollbacks.

## Upgrade with kubectl manifests

If you installed via `deploy/`, re-apply the manifests for the new version:

```bash
kubectl apply -f deploy/
```

The manifests under `deploy/` are the raw form of what the chart renders. CRDs are included in `00-prereqs.yaml` and are overwritten on apply — this is safe because CRD schema changes are additive.

## What restarts during an upgrade

Upgrades touch two independent restart paths:

1. **The operator Deployment.** Changing the operator image updates the `varnish-gateway-operator` Deployment pod template and Kubernetes performs a rolling restart of the operator itself. Data plane traffic is unaffected — Varnish pods keep serving while the operator reconciles nothing for a few seconds.

2. **The gateway (data plane) pods.** The operator computes an `varnish.io/infra-hash` annotation on each Gateway's Deployment from its infrastructure inputs (image, `varnishdExtraArgs`, logging config, listener set, image pull secrets, extra volumes, backend TLS). When the gateway image changes, the hash changes, and Kubernetes performs a rolling restart of the Gateway's pods. This happens once per Gateway, driven by the Deployment's rollout strategy and respecting any configured PodDisruptionBudget. See [pod-disruption-budgets.md](pod-disruption-budgets.md) for how PDB interacts with rolling restarts.

What does **not** restart:

- **Routing changes** (HTTPRoute adds/edits/removes) are hot-reloaded via ghost's HTTP endpoint.
- **User VCL changes** (referenced ConfigMap updates) are hot-reloaded via `varnishadm`.
- **GatewayClassParameters changes that do not touch infrastructure inputs** — e.g., a `resources` edit with no image or args change still triggers a rolling restart (it changes the pod template), but a parameter that only affects routing does not.

See [concepts/reload-paths.md](../concepts/reload-paths.md) for the full model.

## Downtime expectations

Varnish pods are stateless with respect to the cache — a restart drops the pod's cache and the next requests refill it from origin. For a smooth upgrade:

- Run at least two replicas with an HPA floor of `minReplicas: 2` ([HPA guide](horizontal-pod-autoscaling.md)).
- Configure a `PodDisruptionBudget` so Kubernetes never takes the whole fleet down at once ([PDB guide](pod-disruption-budgets.md)).
- Expect an origin load spike proportional to the fraction of the fleet restarted concurrently — cold pods miss everything until the cache refills.

Single-replica Gateways will see a brief connection interruption during restart (typically a few seconds for varnishd to start and pass health checks). If you cannot tolerate that, scale up before upgrading.

## Rollback

### Helm

```bash
helm rollback varnish-gateway --namespace varnish-gateway-system
```

This reverts the operator Deployment and the Helm-managed chart resources to the previous release. The `varnish.io/infra-hash` on each Gateway Deployment will recompute and — if the gateway image changed — trigger another rolling restart back to the previous image.

**CRD caveat:** Helm does not roll back CRDs. If the failed upgrade added fields to a CRD and you created resources using those fields, rolling back the operator will leave those fields present on the API server but unused. If the failed upgrade _removed_ fields (rare — we treat CRDs as additive), rollback is a manual CRD re-apply.

### kubectl

Re-apply the manifests from the previous version tag:

```bash
git checkout vX.Y.Z-previous -- deploy/
kubectl apply -f deploy/
```

## Canary upgrades

The operator does not support running two versions of itself side-by-side in the same namespace — the CRDs are cluster-scoped and the controller filters by `controllerName` rather than by installation. For a canary-style upgrade, stage the upgrade in a non-production cluster first, or use a separate GatewayClass with a distinct `controllerName` and a second operator installation in another namespace.

Most operators who want risk-limited rollouts get what they need from the rolling restart of individual Gateway pods — a failing new image surfaces on the first restarted pod and Kubernetes halts the rollout at `maxUnavailable`.

## Verifying an upgrade

After the upgrade:

```bash
# Operator is running the new version
kubectl get deploy -n varnish-gateway-system varnish-gateway-operator \
  -o jsonpath='{.spec.template.spec.containers[0].image}'

# All Gateway pods have restarted on the new image
kubectl get pods -A -l app.kubernetes.io/component=gateway \
  -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}: {.spec.containers[?(@.name=="varnish-gateway")].image}{"\n"}{end}'

# Gateways are still Programmed
kubectl get gateway -A
```

If a Gateway stops programming after upgrade, check operator logs and the Gateway's status conditions — see [troubleshooting.md](troubleshooting.md).

## See also

- [Installation](../getting-started/installation.md) — initial install, including CRD bootstrap
- [Custom VMODs](custom-vmods.md) — layering VMODs into the gateway image
- [Reload paths](../concepts/reload-paths.md) — which changes restart pods and which hot-reload
- [Pod Disruption Budgets](pod-disruption-budgets.md)
- [Horizontal Pod Autoscaling](horizontal-pod-autoscaling.md)
