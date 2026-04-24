# Chaos test run-book

Operational guide for running `test/chaos/` scenarios. Written to be
usable both as a copy-paste run-book and as debugging context when
things break.

**Status at time of writing**: C01, C04 and C08 have been dry-run
against a single-node kind cluster and pass with healthy numbers (see
"Reference numbers" below). C02 (scale storm, oversized for single-node),
C05 (apiserver partition), C06 (needs multi-node) and C07 (needs TLS
fixture) are unverified end-to-end and await a real multi-node cluster.
Several harness bugs surfaced during the kind run have been fixed —
see "Harness bugs fixed during kind validation" below.

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
| `envsubst` | k6 Job manifest templating (gettext)   |
| `jq`     | Threshold checks in `run.sh` |
| `go`     | Analyzer + builds           |

Chaos Mesh is installed *into* the cluster by the setup script — you
don't need it locally. k6 likewise runs in-cluster as a Kubernetes
Job (image `grafana/k6:0.49.0`, pre-loaded into kind during setup);
you do not need k6 on `PATH`.

## Quick start (kind)

```bash
# 1. Stand up the cluster end-to-end. ~3-5 minutes.
make chaos-kind-setup

# 2. Run a scenario. Traffic is generated inside the cluster; no
#    port-forwards required.
make chaos-run SCENARIO=C01

# 3. Or run the full suite unattended (see "Running the full suite" below):
make chaos-suite

# 4. When done:
make chaos-kind-teardown
```

## Running the full suite

`make chaos-suite` runs all applicable scenarios serially, gates the
cluster back to health between each one, and writes an aggregated
report. It is designed to be left unattended — submit it, walk away
for a few hours, come back to a report plus diagnostic bundles for
any failures.

```bash
# Default: all scenarios; C02/C05 are skipped on single-node kind
# (known to overwhelm it), C06 auto-skips on single-node clusters,
# C07 auto-skips unless the TLS fixture is present.
make chaos-suite

# Pick a subset, or force-include the heavy scenarios:
make chaos-suite CHAOS_ARGS="--scenarios C01,C04,C08"
make chaos-suite CHAOS_ARGS="--full"                # include C02/C05
make chaos-suite CHAOS_ARGS="--bail"                # stop on first failure

# Output goes to dist/suite-<timestamp>/:
#   suite-report.md        human-readable summary table
#   suite-report.json      machine-readable aggregate
#   <scenario>/run.log     full run.sh stdout/stderr
#   <scenario>/<id>-report.json, -ledger.ndjson, -k6.log
#   <scenario>/bundle/     diagnostic bundle on FAIL/TIMEOUT
#                          (operator + chaperone logs, ghost.json,
#                           events, managedFields, etc.)
```

Per-scenario hard cap is `SCENARIO_TIMEOUT_S` (default 900s). Total
suite cap is `SUITE_TIMEOUT_S` (default 12h). Post-scenario cooldown
is `COOLDOWN_S` (default 30s), after which the health gate must pass
or remaining scenarios are aborted.

Recommended first-pass order for dry running: **C01 → C04 → C08**.
C01 exercises the CR path, C04 exercises the action-script path,
C08 exercises parallel ops. If those three pass, C02/C03/C05 will
likely pass too. C06 needs multi-node, C07 needs TLS fixtures.

**Gateway replica count matters for C01.** With a single replica, a
pod-kill produces an outage window and the harness can only measure
it via `non_2xx` (not gated). Scale to ≥2 before running C01:

```bash
kubectl -n varnish-load scale deploy/load --replicas=2
```

C01 has been observed to run cleanly at 2 replicas: zero 5xx, zero
misroutes, ~1.8M k6 samples, 1 missing ledger record (ingest noise).

## Running at scale (C02/C03)

The default fixture has 4 HTTPRoutes — enough for correctness but too
small to exercise C02's "rapid backend scaling" or C03's "HTTPRoute
churn" at realistic cardinality. For a large fixture, use the
generator:

```bash
# 50 vhosts × 10 paths = 500 HTTPRoutes, default parameters.
make load-up-large
# Custom: 100 vhosts × 20 paths = 2000 HTTPRoutes.
make load-up-large LARGE_VHOSTS=100 LARGE_PATHS=20
```

`load-up-large` writes `test/load/fixtures/generated/{routes.yaml,
routes.json}` (gitignored), applies the HTTPRoutes, and creates a
`k6-routes` ConfigMap that the chaos k6 Job mounts at
`/k6/routes/routes.json`. k6 automatically picks this up in place of
the baked-in default route table (see `test/load/k6/lib/routes.js`).

Tear down with:
```bash
make load-down-large
```

Each generated HTTPRoute is labelled `fixture=generated` so
`load-down-large` can remove them without touching the default routes.

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
| C09 | Operator/chaperone don't leak goroutines/FDs/memory under sustained churn (soak) |

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

- **k6 Job pod never went Ready.** `kubectl -n varnish-load describe
  pod -l job-name=k6-<scenario>-<epoch>`. Usually the `grafana/k6`
  image wasn't loaded into kind (kind-setup should have handled
  this) or the `k6-script` ConfigMap wasn't created.
- **Gateway Service has no endpoints.** `kubectl -n varnish-load
  describe svc load`. Gateway pods must be Ready and selectable by
  the Service selector. If no pods match, the Gateway may not have
  reconciled yet — check operator logs.
- **`load-up` never finished.** Re-run `make load-up`.
- **k6 Job logs**: `kubectl -n varnish-load logs job/<k6-job-name>`
  (also copied to `dist/<SCENARIO>-k6.log` at end of run).

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
kubectl -n varnish-load exec "$pod" -c varnish-gateway -- cat /var/run/varnish/ghost.json | jq .
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
Before running C06, apply the drain-friendly overlay and scale the
gateway to match the node count:
```bash
kubectl apply -f test/load/fixtures/drain-friendly.yaml
kubectl -n varnish-load scale deploy/load --replicas=2
```
The overlay configures:
- `topologySpreadConstraints` (one pod per node by `kubernetes.io/hostname`)
  so a drain actually moves a pod rather than killing one of several
  co-located replicas.
- `podDisruptionBudget` with `minAvailable: 1` so `kubectl drain`
  blocks until the replacement pod is Ready. Without a PDB, drain
  takes all gateway pods down and the scenario registers as a
  complete outage; `--force` sidesteps this but weakens the test.

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

## Resource-leak and reconcile-storm checks

The harness also snapshots `/metrics` from the operator and one data-plane
pod's chaperone at `fault_start`, `fault_end`, and `scenario_end`. The
summary (written to `dist/<ID>-metrics.json`; raw snapshots at
`dist/<ID>-metrics/*.prom`) captures:

| Field                              | What it catches                     |
| ---------------------------------- | ----------------------------------- |
| `operator.goroutines.delta`        | Goroutine leak across the scenario  |
| `operator.open_fds.delta`          | FD / connection leak                |
| `operator.rss_bytes.delta`         | Memory growth                       |
| `operator.workqueue_depth_end`     | Queue failed to drain after fault   |
| `operator.reconcile_rate_hz`       | Reconcile storm during fault window |
| `operator.reconcile_avg_ms`        | Mean reconcile duration during fault (regression on blocking calls, lock contention) |
| `operator.reconcile_errors`        | Reconcile errors during fault       |
| `chaperone.goroutines.delta`       | Chaperone goroutine leak            |
| `chaperone.ghost_reload_errors`    | Failed ghost reloads                |

Thresholds are opt-in per scenario — set any of these in the scenario's
`.env` to gate on them:

```bash
MAX_OPERATOR_GOROUTINE_DELTA=20      # goroutines between fault_start and scenario_end
MAX_OPERATOR_WORKQUEUE_DEPTH_END=0   # queue depth summed across controllers at scenario_end
MAX_OPERATOR_RECONCILE_RATE_HZ=50    # reconciles/sec during the fault window
MAX_OPERATOR_RECONCILE_AVG_MS=200    # mean ms per reconcile during the fault window
MAX_OPERATOR_RECONCILE_ERRORS=0      # new errors observed during the fault window
MAX_OPERATOR_FDS_DELTA=5
MAX_CHAPERONE_GOROUTINE_DELTA=10
MAX_CHAPERONE_RELOAD_ERRORS=0
```

Unset variables skip the check — no defaults are enforced. C03 and C06
intentionally produce high reconcile rates and should set
`MAX_OPERATOR_RECONCILE_RATE_HZ` higher than the quieter scenarios, or
omit it.

Scrapes use ephemeral `kubectl port-forward`s on ports 18090/18091. If the
chaperone pod being forwarded is rescheduled mid-run (C06 node drain),
the snapshots for that pod will be empty; the summary reports zeros and
the threshold check is harmless. For C06 in particular, chaperone-side
leak checks are unreliable — lean on the operator-side metrics instead.

## Long-horizon soak (C09)

C01–C08 run for ~3 minutes each. That catches leaks visible within the
fault window but misses per-event leaks where N events need to accumulate
— typically 1 goroutine per ~1k events, which takes hours to surface.

`test/chaos/soak.sh` is a separate runner (not a `run.sh` scenario) that
drives HTTPRoute churn continuously for `SOAK_HOURS` and takes metric
snapshots every `SNAPSHOT_INTERVAL_S`. A linear fit through each metric's
series gives a slope; threshold checks fail the run if the slope is
positive beyond the scenario's tolerance.

```bash
source ./test/chaos/scenarios/C09.env   # SOAK_HOURS, intervals, thresholds
./test/chaos/soak.sh
```

Differences from `run.sh`:

- **Continuous driver, not a fault window.** One apply+delete cycle every
  `CHURN_INTERVAL_S` (default 60s) — the soak itself is the fault.
- **Many snapshots.** Each appends one line to `dist/C09-soak/metrics.ndjson`;
  raw `.prom` files kept in `dist/C09-soak/snapshots/` for pprof-style
  dives if a slope trips.
- **`kubectl proxy` instead of `kubectl port-forward`.** Port-forwards
  drop on the slightest network hiccup; proxy survives pod reschedules
  and holds connections over hours. Requires `pods/proxy` RBAC.
- **Slopes, not deltas.** Fails on sustained trend, not endpoint delta.
  A 5-goroutine spike that decays back to baseline is a no-op; a
  consistent 0.5/min climb over 3 hours is a leak.

Output: `dist/C09-soak/soak-report.json`

```json
{
  "samples": 37, "duration_min": 180.0,
  "op_goroutines": {"start": 120, "end": 124, "slope_per_min": 0.022, "r2": 0.31},
  "op_rss_bytes":  {"start": 5.24e+07, "end": 5.38e+07, "slope_per_min": 7840, "r2": 0.78},
  ...
}
```

Thresholds (all opt-in, see `C09.env`):

```bash
MAX_OP_GOROUTINE_SLOPE_PER_MIN=0.5   # +30 goroutines/h sustained → leak
MAX_OP_RSS_SLOPE_PER_MIN=200000      # 200KB/min = 12MB/h
MAX_OP_FDS_SLOPE_PER_MIN=0.02        # any positive slope on fds is suspect
MAX_CH_GOROUTINE_SLOPE_PER_MIN=0.5
```

Calibrate from a clean baseline run — real slopes are never exactly 0
(GC cycles, informer resync, legitimate growth), and thresholds need
headroom above the noise floor. 3h is the default for routine soaks;
24h catches most of what longer runs would, and 72h is rarely worth the
cluster-rental cost for leak detection alone.

## Reference numbers (single-node kind, 2 gateway replicas)

Observed during dry-run validation. Use these as a smell test — the
analyzer report should look similar on an otherwise-idle cluster. A
run with `total < MIN_TOTAL` (default 50) now fails loudly instead of
silently passing on empty data.

| Scenario | total samples | drop_ratio | non_2xx | misroutes | converge_ms |
| -------- | ------------- | ---------- | ------- | --------- | ----------- |
| C01      | ~1.8M         | ~5e-7      | 0       | 0         | 0           |
| C04      | ~10M          | ~1e-7      | 0       | 0         | 0           |
| C08      | ~12M          | ~8e-8      | 0       | 0         | 0           |

Drop ratios at ~1e-7 are ledger ingest noise (one missing echo POST
per million k6 requests), not real data loss. `MAX_DROP_RATIO=1e-5`
(10 ppm) in the C04/C08 envs is a noise floor, not a tolerance for
actual drops — if the ratio climbs into 1e-4 or higher, something
real is wrong.

## Harness bugs fixed during kind validation

The first kind run found several harness issues that produced
misleading results. They are fixed in-tree; noted here because they
are the kind of thing that can regress silently.

- **`run.sh` used to swallow k6 failures.** k6 was forked with stderr
  redirected to a log file inside a temp dir. If k6 was missing or
  exited immediately, the scenario still "PASSED" because the
  analyzer found zero drops of zero requests. Fixed: preflight check
  for `k6` on `PATH`, post-launch liveness check, and a `MIN_TOTAL`
  gate (default 50) that fails the scenario when k6 didn't actually
  drive traffic. The summary line also now emits `total` and `non_2xx`.
- **C04's post-canary timeout raced controller-runtime cache sync.**
  `kubectl rollout status` returns when the pod is Ready per its
  liveness probe, but controller-runtime needs an additional ~20–25s
  for leader election + informer cache sync before HTTPRoute workers
  start processing. The previous 30s wait occasionally beat this;
  bumped to 60s. Notably the data path stayed up through this whole
  window — the operator blind-spot did not produce any non_2xx.
- **`MAX_DROP_RATIO=0.0` in C04/C08 was brittle** at multi-million
  sample runs. A single unlogged echo POST out of 10M → failure.
  Replaced with `1e-5` as a noise floor. C01 already used `0.01`.
- **`mktemp --suffix=.yaml`** in C05 is GNU-only; replaced with a
  portable form for macOS.
- **`kind-setup.sh` loaded only `:$VERSION`-tagged images into kind.**
  The load-suite manifests (`test/load/deploy/*.yaml`) hardcode
  `:latest`, so pods fell through to a ghcr.io pull (403). Fixed: the
  script now loads both `:$VERSION` and `:latest` tags for the echo
  and collector images.

## Known gaps

- **C07 has never run.** Every other scenario has a real-cluster pass;
  C07 SKIPs because the load fixture is HTTP-only. A TLS listener and
  cert-manager `Certificate load-tls` need to land before it can run.
- **Load fixture is still 4 HTTPRoutes.** The dry-run scale
  ("hundreds of HTTPRoutes") requires the generator in
  `test/load/fixtures/gen` plus a matching k6 routes file — the
  generator is in-tree, wiring it into scenarios is not.
- **Convergence is measured only after `fault_end`, not `fault_start`.**
  For scenarios where the interesting transition is at
  fault-start (C05 partition applied), the convergence metric
  doesn't capture that.
- **Soak ledger doesn't rotate.** C09 at 20 RPS × 3h produces ~200MB of
  k6 ledger, which the analyzer reads into memory fine. At 72h × 50 RPS
  (~6GB) it would OOM — the collector rotates at 1GB but the analyzer
  loads whole. Not a problem at the 3h default; becomes one for longer
  soaks.
- **No pprof capture on fail.** A threshold trip (either scenario or
  soak) tells you *something* leaked; diagnosing it still requires a
  manual `kubectl exec` + `/debug/pprof/goroutine`. The `.prom`
  snapshots C09 keeps are a step toward this, but aren't full profiles.
- **Soak driver is single-shape.** C09 only churns HTTPRoutes. A per-Gateway
  annotation thrash or a mix with occasional pod kills would exercise
  different code paths, but isn't there yet.
- **No leader-election / status-subresource checks.** A controller that
  stops writing `Programmed=True` but keeps the data path working passes.
- **No CI wiring.** Scenarios are manual. In-cluster k6 now removes
  the PR-side port-forward SPOF, but CI still needs a kind cluster per
  run with images pre-built; the run time per scenario (~3 min) is
  tolerable, the cluster spin-up cost per PR is not.

## Extending

Add a scenario:

1. Pick the next ID (e.g., C10) and decide flavor:
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
| Analyzer JSON from ledger  | `go run ./test/load/analyze -f X.ndjson -scenario C01 -json` |
| Metrics summary from snapshots | `test/chaos/lib/metrics-summary.sh dist/C01-metrics/` |
| Run soak (default 3h)      | `./test/chaos/soak.sh`                    |
| Soak slope report          | `test/chaos/lib/soak-fit.sh dist/C09-soak/metrics.ndjson` |
| Download ledger            | `curl http://127.0.0.1:9090/download`     |
