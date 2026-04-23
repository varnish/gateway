# Chaos test run-book

Operational guide for running `test/chaos/` scenarios. Written to be
usable both as a copy-paste run-book and as debugging context when
things break.

**Status at time of writing**: all 8 scenarios are scaffolded. None
have been validated end-to-end against a real cluster. The first
real run will find the real bugs — this document is partly a map of
where those bugs are likely to be.

For scenario-level reference (the table of P0/P1 scenarios and their
pass criteria) see [`test/chaos/README.md`](test/chaos/README.md).
This document is the operational side: how to run, what to check
when it fails, and how to debug.

## When to use chaos tests

- **Pre-release sanity**: the P0 scenarios (C01–C04) should pass
  before cutting a beta.
- **Regression hunting**: after changing chaperone reload logic,
  ConfigMap handling in either controller, or ghost.json generation.
- **Not for every PR**: these require a live cluster. The Gateway
  API conformance suite (`make test-conformance`) is the right gate
  for routine changes.

## Prerequisites

| Tool     | Why                         |
| -------- | --------------------------- |
| `kind`   | Local cluster (Go tool, already vendored) |
| `kubectl`| Cluster access              |
| `helm`   | Chaos Mesh install          |
| `docker` | Image builds                |
| `k6`     | Load generator              |
| `jq`     | Threshold checks in `run.sh` |
| `go`     | Analyzer + builds           |

Chaos Mesh is installed *into* the cluster by the setup script — you
don't need it locally.

## Quick start (kind)

```bash
# 1. Stand up the cluster end-to-end. ~3-5 minutes.
make chaos-kind-setup

# 2. In two separate terminals, open port-forwards:
kubectl -n varnish-load port-forward svc/ledger-collector 9090:8080
kubectl -n varnish-load port-forward svc/load 8080:80

# 3. Run a scenario.
export GATEWAY_URL=http://127.0.0.1:8080
export COLLECTOR_URL=http://127.0.0.1:9090
make chaos-run SCENARIO=C01

# 4. When done:
make chaos-kind-teardown
```

Recommended first-pass order for dry running: **C01 → C04 → C08**.
C01 exercises the CR path, C04 exercises the action-script path,
C08 exercises parallel ops. If those three pass, C02/C03/C05 will
likely pass too. C06 needs multi-node, C07 needs TLS fixtures.

## What each scenario tests — one line each

| ID  | Real target                                                  |
| --- | ------------------------------------------------------------ |
| C01 | Data plane survives pod termination (one chaperone pod killed) |
| C02 | Chaperone re-generates `ghost.json` correctly under endpoint storms |
| C03 | Both controllers respect ConfigMap co-ownership under HTTPRoute churn |
| C04 | Operator survives interruption without leaving partial state |
| C05 | Chaperone informers resync after apiserver partition        |
| C06 | Gateway pods reschedule during node drain                    |
| C07 | Cert-manager hot-reload works under active traffic           |
| C08 | Controller writes don't race on the shared ConfigMap         |

## Architecture of the harness

```
  ┌──────┐    X-Trace-ID     ┌─────────┐  backend selection  ┌──────┐
  │  k6  │──────────────────▶│ Varnish │────────────────────▶│ echo │
  └──┬───┘                   │ +ghost  │                     └───┬──┘
     │ ledger POST           └─────────┘                          │ ledger POST
     │                                                            │
     ▼                                                            ▼
  ┌────────────────────────────────────────────────────────────────┐
  │ collector (PVC-backed NDJSON)                                  │
  └────────────────────────────┬───────────────────────────────────┘
                               │
                               │  /download
                               ▼
  ┌────────────────────────────────────────────────────────────────┐
  │ analyzer                                                       │
  │ - joins k6 and echo records by trace-id                        │
  │ - reports drops, misroutes, duplicates, convergence            │
  │ - -json flag produces machine-readable thresholds for run.sh   │
  └────────────────────────────────────────────────────────────────┘

  Chaos runner (run.sh):
    ┌─────────────────────────────────────────────┐
    │ 1. start k6 background                      │
    │ 2. mark <scenario>_fault_start              │
    │ 3. apply Chaos Mesh CR OR run action script │
    │ 4. mark <scenario>_fault_end                │
    │ 5. wait POST_FAULT_S                        │
    │ 6. stop k6, download ledger, analyze -json  │
    │ 7. fail if any threshold exceeded           │
    └─────────────────────────────────────────────┘
```

Marker records flow into the same NDJSON stream as k6/echo records
(`test/load/ledger/record.go`), tagged with `source: chaos`. The
analyzer's convergence metric is: "for each marker, time from its
timestamp to the first subsequent k6 sample that was correct."

## Debugging checklist when a scenario fails

The scripts fail in a few characteristic ways. Check these in order.

### 1. Setup-time failures

**Symptom**: `make chaos-kind-setup` aborts.

- **Chaos Mesh install hangs or times out.** The helm install waits
  on the `chaos-controller-manager` pod. Look at
  `kubectl -n chaos-mesh get pods` — if the chaos-daemon daemonset
  is crashlooping, it's almost certainly the containerd socket
  path. The script uses `/run/containerd/containerd.sock` which is
  correct for current kind; if kind's version has drifted,
  check the actual socket path inside a kind node:
  ```bash
  docker exec <kind-node> ls /run/containerd/
  ```
- **`ImagePullBackOff` on operator or chaperone.** Usually a tag
  mismatch — the setup script builds with tag `kind` and sets
  `VERSION=kind` through `make kind-deploy`. If `kubectl describe
  pod` shows it trying to pull `:latest` or `:0.20.0`, the deploy
  manifests weren't templated with the right tag. Check
  `deploy/01-operator.yaml` after `deploy-update` ran.

### 2. k6 failures (no traffic at all)

**Symptom**: ledger has no `source: k6` records, analyzer says
`total requests: 0`.

- **Port-forwards not running.** Check with `curl
  http://127.0.0.1:8080/` and `curl http://127.0.0.1:9090/healthz`.
- **Gateway Service has no endpoints.** `kubectl -n varnish-load
  describe svc load`. Gateway pods must be Ready and selectable by
  the Service selector. If no pods match, the Gateway may not have
  reconciled yet — check operator logs.
- **`load-up` never finished.** Re-run `make load-up`.

### 3. Analyzer reports drops or misroutes

**Symptom**: `run.sh` exits non-zero at the threshold check.

The report is written to `dist/<SCENARIO>-report.json`. Key fields:

| Field         | Meaning                                      |
| ------------- | -------------------------------------------- |
| `total`       | k6 requests that got a response (or timed out) |
| `drops`       | k6 saw a 2xx but there's no matching echo record |
| `misroutes`   | echo service != expected service, OR the `X-Echo-Service` header disagreed |
| `duplicates`  | >1 echo record for a single trace-id         |
| `convergence` | per-marker ms until first correct response after the marker |

Drops with near-zero `non_2xx`: collector ingest is lossy, or echo
isn't reliably POSTing. Check the collector's `/metrics` endpoint
(`http_requests_total`, `ingest_bytes_total`).

Drops alongside many `non_2xx`: real 5xx responses, not an
observability problem. Look at Varnish/chaperone logs.

Misroutes: serious. The header check (`X-Echo-Service`) catches
most cases even if echo→collector POSTs are lost. A non-zero
misroute count means ghost actually sent traffic to the wrong
backend. Check ghost.json:

```bash
pod=$(kubectl -n varnish-load get pod -l gateway.networking.k8s.io/gateway-name=load -o name | head -1)
kubectl -n varnish-load exec "$pod" -c chaperone -- cat /var/run/varnish/ghost.json | jq .
```

Convergence > threshold: chaperone took too long to reload after a
fault. Check chaperone logs for `reload` entries; look for
`"reload_failed"` or long gaps between fault-end and the next
successful reload.

### 4. Scenario-specific notes

**C01 — chaperone pod kill**: The Chaos Mesh CR targets
`gateway.networking.k8s.io/gateway-name=load` in `varnish-load`.
If your Gateway has a different name or namespace, edit
`test/chaos/scenarios/C01-chaperone-pod-kill.yaml`. Same for the
matching `C01.env`.

**C02 — rapid scaling**: Asserts that after final scale-to-0, the
target deployment's endpointslices are drained. If that assertion
fires but traffic was unaffected, the test is over-strict — the
endpoint propagation may just be slow. Increase
`SCALE_CONVERGE_S` in `C02.env`.

**C03 — HTTPRoute churn**: Asserts fixture vhosts (a.load.local,
b.load.local, mixed.load.local) are present in `load-vcl`'s
`routing.json` after churn. If fixture vhosts are missing, that's
a real bug in the HTTPRoute controller's ConfigMap update logic.
Worth investigating immediately — it's exactly the failure mode
the issue called out.

**C04 — operator pod kill**: The "mid-reconcile" property is
best-effort. The script applies a canary right before
`kubectl delete pod --force`. If the reconcile finishes before
the kill, the scenario only tests shutdown cleanliness, not
crash-recovery. This is acceptable for now.

**C05 — apiserver partition**: The NetworkChaos CR uses
`externalTargets: [kubernetes.default.svc.cluster.local]`. If
Chaos Mesh can't resolve that in your cluster, swap for an
explicit IP range matching your apiserver. The restart-count
check is crude — it treats `>2` as "crashloop" which is a
reasonable heuristic but not a precise one.

**C06 — node drain**: Exits with `SKIP` on single-node clusters.
kind by default is single-node; to test C06, recreate with:
```bash
kind delete cluster --name varnish-gw
cat <<EOF | kind create cluster --name varnish-gw --config -
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
EOF
# Then re-run make chaos-kind-setup (it skips cluster creation).
```
Gateway pods must have a PodDisruptionBudget or drain will block
forever — `--force` sidesteps this but weakens the test.

**C07 — TLS rotation**: Exits with `SKIP` until
`test/load/fixtures/routes.yaml` grows a TLS listener and a
cert-manager Certificate named `load-tls`. When you add those,
set `CERT_NAME` in `C07.env` if you pick a different name.

**C08 — simultaneous changes**: Tests the ConfigMap co-ownership
invariant described in `CLAUDE.md` under "ConfigMap Shared
Ownership Pattern". Failures here indicate a regression in
either controller's Server-Side Apply field management.
Inspect the ConfigMap's `metadata.managedFields`:
```bash
kubectl -n varnish-load get configmap load-vcl -o yaml | yq '.metadata.managedFields'
```
Each top-level `data` field should have a single owner (Gateway
controller for `main.vcl`, HTTPRoute controller for `routing.json`).
Mixed ownership of a single field means one controller is
overwriting the other.

## When the harness itself is suspect

Sometimes a scenario fails in a way that suggests the test
infrastructure is wrong, not the code under test. Sanity checks:

```bash
# Does the analyzer -json output match what run.sh jq-queries?
go run ./test/load/analyze -f dist/ledger.ndjson -json | jq .

# Does the collector actually have records?
curl -s http://127.0.0.1:9090/download | head -20

# Are chaos markers in the ledger?
curl -s http://127.0.0.1:9090/download | grep '"source":"chaos"'

# Does echo actually POST back?
curl -s http://127.0.0.1:9090/download | grep '"source":"echo"' | head -5
```

The ledger format is defined in `test/load/ledger/record.go`. Field
names are stable — if `run.sh` references a field that doesn't
appear in the NDJSON, someone renamed something.

## Known gaps (as of this writing)

- **No scenario has been run against a real cluster.** This is the
  single most important caveat. Syntax-checked only.
- **Convergence is measured only after `fault_end`, not `fault_start`.**
  For scenarios where the interesting transition is at
  fault-start (C05 partition applied), the convergence metric
  doesn't capture that.
- **The load fixture has 4 HTTPRoutes.** The issue specifies
  "hundreds of HTTPRoutes" for meaningful C02/C03 at scale. Real
  validation needs a larger fixture and a cluster budget.
- **No CI wiring.** Scenarios are manual. A CI job would need: a
  kind cluster per run (expensive), images pre-built, port-forwards
  replaced by in-cluster k6. The run time per scenario (~3 min) is
  tolerable; the cluster spin-up cost is not if run per PR.

## Extending

Add a scenario:

1. Pick the next ID (e.g., C09) and decide flavor:
   - Chaos Mesh CR → `test/chaos/scenarios/C09-<name>.yaml`
   - Action script → `test/chaos/scenarios/C09-<name>.sh`
2. Add `test/chaos/scenarios/C09.env` with `DURATION`,
   `PRE_FAULT_S`, `POST_FAULT_S`, and thresholds.
3. Add a row to the table in `test/chaos/README.md`.
4. Verify `make chaos-run SCENARIO=C09` on a clean cluster.

Mark a chaos event for analyzer convergence measurement anywhere
inside an action script:
```bash
"$(dirname "$0")/../lib/mark.sh" "my_event" "target"
```

## Quick-reference commands

| Task                       | Command                                   |
| -------------------------- | ----------------------------------------- |
| Stand up kind              | `make chaos-kind-setup`                   |
| Tear down kind             | `make chaos-kind-teardown`                |
| Run a scenario             | `make chaos-run SCENARIO=C01`             |
| Inspect ghost.json         | `kubectl exec ... cat /var/run/varnish/ghost.json` |
| Analyzer JSON from ledger  | `go run ./test/load/analyze -f X.ndjson -json` |
| Download ledger            | `curl http://127.0.0.1:9090/download`     |
