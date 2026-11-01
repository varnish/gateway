# Route VCL capacity spike (issue #96)

Measurements behind the slot pool defaults for per-HTTPRoute VCL. This is a
development record, not user documentation.

## Environment caveat

- **Not run in a pod.** Local workstation: 16 cores, Linux 7.0, Varnish
  9.0.0 (the target is 9.1.0), unprivileged uid 1000, no cgroup limits.
- Backend: a Go "ok" server on loopback, sharing cores with `wrk` and
  varnishd. Latency numbers are loopback only.
- varnishd ran with `-s malloc,64m -p thread_pool_stack=160k -f ''` and was
  driven over `-T`/`-S`. The ~0.7 ms `varnishadm` spawn overhead is included
  in CLI timings.
- Child baseline RSS before any route VCL: ~97 MB. Compare deltas.
- A "route VCL" is the repo preamble + a ~34-line user part + the postamble,
  all pointing at one tiny ghost.json (1 vhost, 1 backend). Per-route
  ghost.json sizes were not varied.
- VCL compile forks a C compiler (~200 ms CPU here). A pod with a CPU limit
  will be slower, possibly several times.

## 1. Compile time (median of 10)

| Variant | lines | median | min–max |
| --- | --- | --- | --- |
| placeholder, no VMOD | 3 | 160 ms | – |
| route 1× (`vcl.load`) | 205 | 223 ms | 221–242 |
| route 1× (`vcl.inline`) | 205 | 224 ms | 222–225 |
| route 4× user part | 295 | 269 ms | 268–273 |
| route 16× user part | 655 | 453 ms | 452–456 |

Linear: ~160 ms fixed cost plus ~0.5 ms per line. A realistic route VCL is
dominated by the fixed cost; budget ~0.2 s CPU per route VCL on this host.

## 2. Pool capacity

Fresh varnishd per level, `max_vcl=1000`, N labels each pointing at a
distinct populated route VCL. Baseline: 97.5 MB child, 9.1 MB manager, 383
stat lines.

| N | child RSS (Δ) | mgr RSS | stat lines (Δ) | `vcl.list` | bootstrap N×(load+label) | master compile+use |
| --- | --- | --- | --- | --- | --- | --- |
| 16 | 101.8 MB (+4.2) | 9.2 MB | 1043 (+660) | 0.8 ms | 3.6 s | 0.19 s |
| 64 | 107.4 MB (+9.7) | 9.4 MB | 2963 (+2580) | 1.3 ms | 14.4 s | 0.27 s |
| 256 | 129.5 MB (+31.9) | 9.8 MB | 10643 (+10260) | 1.6 ms | 57.4 s | 0.60 s |

Each populated route VCL costs ~125 KB child RSS, ~40 stat lines (ghost
counters plus one `VBE.*` set) and ~0.22 s compile. Bootstrap is serial
compile. The last label answered 200 through the jump at every level.

Placeholder-only pool (one placeholder VCL, N labels on it):

| N | child RSS | stat lines | load+label loop | master compile |
| --- | --- | --- | --- | --- |
| 16 | 100.0 MB | 403 | 0.22 s | 0.20 s |
| 64 | 100.0 MB | 403 | 0.23 s | 0.32 s |
| 256 | 100.1 MB | 403 | 0.37 s | 0.60 s |

Unused labels are effectively free (~1 ms each, no RSS or stats growth).

### `max_vcl`

- Default `max_vcl=100` with `max_vcl_handling=1` only warns; 151 VCLs
  loaded under defaults with just the "Remember to vcl.discard" warning.
- With `max_vcl_handling=2` the 101st `vcl.load` fails: "Too many (100)
  VCLs already loaded".
- **Labels do not count toward `max_vcl`.** 100 extra labels were added at
  the enforced limit; 300 labels on one VCL also worked.
- Headroom needed: populated route VCLs + master + placeholder + replaced
  generations awaiting discard. Set `max_vcl` explicitly (2 × pool + 10).

## 3. Request latency, jump vs no jump

`wrk -t4 -c64`, keep-alive, median of 3 runs after a 2 s warmup.

### 3a. Full route VCL to a backend (10 s runs)

| Case | req/s | p50 | p99 |
| --- | --- | --- | --- |
| route VCL active, no jump | 155,363 | 396 µs | 980 µs |
| master, jump, pool 1 | 154,618 | 396 µs | 1.00 ms |
| pool 64, slot 0 | 155,469 | 393 µs | 1.00 ms |
| pool 64, slot 63 | 153,458 | 399 µs | 0.99 ms |
| pool 256, slot 0 | 154,486 | 397 µs | 0.98 ms |
| pool 256, slot 255 (linear dispatch) | 145,286 | 421 µs | 1.03 ms |

The jump is inside run-to-run noise (<1%) up to pool 64. At the far end of a
256-entry linear chain it costs ~6% throughput and +25 µs p50.

### 3b. Jump isolated: synth 503 in the target, no backend (8 s runs)

| Case | req/s | p50 | p99 |
| --- | --- | --- | --- |
| synth in active VCL, no jump | 919,660 | 37 µs | 80 µs |
| jump, pool 1 | 898,340 | 55 µs | 85 µs |
| jump, pool 64, slot 0 | 884,733 | 56 µs | 113 µs |
| jump, pool 64, slot 63 | 854,960 | 61 µs | 128 µs |
| jump, pool 256, slot 0 | 900,686 | 54 µs | 84 µs |
| jump, pool 256, slot 255, linear dispatch | 705,578 | 68 µs | 408 µs |
| pool 256, slot 0, binary-tree dispatch | 887,757 | 57 µs | 89 µs |
| pool 256, slot 255, binary-tree dispatch | 886,853 | 57 µs | 90 µs |

`wrk` saturates near 900k req/s, so p50 includes queueing; treat deltas as
upper bounds. Per-request jump cost: ~2–3% at synthetic saturation (roughly
20–30 ns CPU), undetectable once a backend round trip is involved. Pool size
does not matter; dispatch shape does. A 256-entry linear `if` chain adds
~330 ns for the last entry; a balanced binary tree on
`std.integer(req.http.X-Ghost-VCL-Slot, -1)` is flat and compiles in 0.69 s.

## 4. Connection pool sharing

Master and a labelled VCL each declared a backend to the same IP:port (plain
`backend` blocks, then ghost native backends). Deltas of
`MAIN.backend_conn/reuse/recycle`, `VBE.<vcl>.<backend>.req` and `ss`
ESTABLISHED counts, `wrk -c16 -d4s` per phase:

| Phase | new conns | ESTAB to backend |
| --- | --- | --- |
| master path, cold | 16 | 16 |
| jump path after master warmed | **0** | unchanged |
| master path again | 0 | unchanged |
| fresh instance, jump path first | 16 | +16 |
| then master path | **0** | unchanged |

Ghost backends behaved identically. **Confirmed: the connection pool is
shared across VCLs by endpoint.** Per-VCL backend objects and their `VBE.*`
counters stay separate. Not tested: options that change pool identity
(host_header, probes, PROXY, TLS).

## 5. Child access to the varnishadm secret (from #95)

Locally, manager and child both run as uid 1000 with the same groups; the
0600 secret is readable by the child. The container is the same:
`docker/chaperone.Dockerfile` sets `USER varnish` (uid 1000), the operator
adds only `IPC_LOCK` and `NET_BIND_SERVICE`, there is no `runAsUser`/`fsGroup`
and `internal/vrun` passes no `-j`. Chaperone writes `<WorkDir>/secret` as
0600 and passes `-S`. So chaperone, manager and child share one uid and any
VCL running in the child can read the CLI secret. Hiding it would need real
uid separation, which unprivileged varnishd does not give.

## Recommendations adopted

- Default pool size **64** (+~10 MB RSS and ~2,600 stat lines fully
  populated, ~14 s serial bootstrap here, no per-request penalty).
- Ceiling **256** (largest measured: +32 MB RSS, ~10.6k stat lines, ~1 min
  serial bootstrap, 0.6 s master compile).
- Binary-tree dispatch regardless of pool size.
- Set `max_vcl` explicitly to `2 × pool + 10`.
- Re-measure compile cost in a pod with the real CPU limit and the C
  compiler in the image, on Varnish 9.1.0.

## Not measured

Cold start via `-f` (impossible with labels); pod CPU limits; realistic
per-route ghost.json sizes; reload-epoch catch-up cost (functionally covered
by `ghost/tests/test_multi_vcl_reload.vtc`); actual VSM bytes; pool identity
under differing backend options.
