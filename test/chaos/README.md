# Chaos scenarios

Fault-injection suite layered on top of `test/load/`. Each scenario runs
k6 traffic and the ledger analyzer while Chaos Mesh injects a specific
failure; the runner emits `source: chaos` markers to the collector so
the analyzer can measure convergence around each event.

Requires a cluster with [Chaos Mesh](https://chaos-mesh.org/) installed
and the load suite already up (`make load-up`). See scenario notes for
minimum cluster sizing — most P0s run on rancher-desktop, but the "rapid
scaling" and "HTTPRoute churn" scenarios benefit from ≥3 nodes.

## Scenarios

| ID  | Priority | Name                        | Fault                                             | Pass criteria                                        | Status |
| --- | -------- | --------------------------- | ------------------------------------------------- | ---------------------------------------------------- | ------ |
| C01 | P0       | Chaperone pod kill          | `PodChaos` kill 1 chaperone pod                   | 0 misroutes post-recovery, convergence < 10s, < 1% drops during event | scaffolded |
| C02 | P0       | Rapid backend scaling       | Scale echo 0→30→0→30 over 2 min                   | 0 stale backends in `ghost.json`, 500s only for empty groups | scaffolded |
| C03 | P0       | HTTPRoute churn             | Apply/delete 50 HTTPRoutes in a 30s burst         | No ConfigMap conflicts, routing converges < 15s      | scaffolded |
| C04 | P0       | Operator pod kill           | `kubectl delete pod` mid-reconcile                | Clean recovery, no partial state, next reconcile succeeds | scaffolded |
| C05 | P1       | API-server network partition | `NetworkChaos` chaperone ↔ apiserver for 60s     | Resync on reconnect, no restart loops                | scaffolded |
| C06 | P1       | Node drain                  | `kubectl drain` a node running gateway pods       | Traffic continues, pods rescheduled, 0 misroutes     | scaffolded (needs ≥2 nodes) |
| C07 | P1       | TLS cert rotation under load | Rotate cert-manager cert mid-run                  | Hot-reload, 0 TLS handshake failures after rotation  | scaffolded (blocked on TLS fixture) |
| C08 | P1       | Simultaneous Gateway + Route changes | Concurrent Gateway + HTTPRoute edits      | Both converge, no lost updates                       | scaffolded |

"Convergence" = first correct echo response for a given route after the
recovery marker, measured by the analyzer.

## Running

```bash
# Prereqs: cluster with Chaos Mesh + load suite up
make load-up

# Port-forwards for k6 and chaos runner (both need collector)
kubectl -n varnish-load port-forward svc/ledger-collector 9090:8080 &
kubectl -n varnish-gateway-system port-forward svc/<gw-svc> 8080:80 &

# Run a single scenario end-to-end (k6 + fault + analyze)
make chaos-run SCENARIO=C01

# Or run manually
./test/chaos/run.sh C01
```

The runner exits non-zero if the analyzer reports violations above the
scenario's thresholds.

## Layout

- `scenarios/C0X-*.yaml` — Chaos Mesh CR(s) for the scenario
- `scenarios/C0X.env`     — scenario parameters (duration, thresholds) sourced by `run.sh`
- `run.sh`                — generic driver: start k6 → mark → apply CR → wait → delete CR → mark → analyze
- `lib/mark.sh`           — POSTs a ledger record with `source=chaos` to the collector

## Adding a scenario

Scenarios come in two flavors — pick whichever fits the fault:

- **Chaos Mesh CR**: drop the CR in `scenarios/<ID>-<name>.yaml`. The
  runner applies it, waits `WAIT_S`, then deletes it.
- **Action script**: drop an executable script at
  `scenarios/<ID>-<name>.sh`. The runner invokes it synchronously; the
  script owns the full fault lifecycle (apply, wait, revert). Use this
  when the fault isn't a Chaos Mesh primitive — e.g., `kubectl scale`,
  route churn via `kubectl apply/delete`, or multi-step sequences.

Both flavors share:

1. A `scenarios/<ID>.env` with `DURATION`, `PRE_FAULT_S`, `POST_FAULT_S`,
   and thresholds (`MAX_DROP_RATIO`, `MAX_MISROUTES`, `MAX_CONVERGE_MS`).
2. A row in the scenarios table above with pass criteria.
3. A clean-cluster run of `./test/chaos/run.sh <ID>` that passes.

## Known gaps

- All scenarios are scaffolded but none have been validated against a
  real cluster — syntax-checked only. First end-to-end run will surface
  the real issues (selector mismatches, timing windows, etc.).
- C06 requires ≥2 schedulable nodes; it prints `SKIP` on single-node
  clusters (rancher-desktop).
- C07 requires a cert-manager Certificate on the load Gateway; the
  current fixture is HTTP-only, so the scenario prints `SKIP` until
  `test/load/fixtures/routes.yaml` grows a TLS listener.
- C02 and C03 are sized for rancher-desktop defaults; the issue calls
  for "hundreds of HTTPRoutes on 10–15 nodes" — that needs a CI cluster
  budget decision before those scenarios can run at full scale.
- "Mid-reconcile" in C04 and "during partition" in C05 are timing-
  dependent — the apply happens a few seconds into the fault window,
  which is best-effort rather than deterministic. Scenarios catch the
  common cases; a dedicated fault-injection hook in the operator would
  be needed for reliable mid-operation kills.
- Per-scenario thresholds are enforced by `run.sh` post-processing the
  analyzer's `-json` output, not by the analyzer itself.
