// Primary correctness oracle for the gateway: for each request we record
// the intended route (host+path), the expected backend service, the actual
// response status, and the X-Echo-Service reflected by the echo pod. The
// analyzer joins these with the echo-side records (POSTed directly by echo)
// by trace-id to catch drops, misroutes, and duplicates.
//
// Env:
//   GATEWAY_URL      base URL, e.g. http://gateway.example:80
//   COLLECTOR_URL    ledger collector base URL, e.g. http://ledger-collector:8080
//   VUS, DURATION    standard k6 knobs
//   RPS              optional constant-arrival-rate target
//
// Example:
//   k6 run -e GATEWAY_URL=http://127.0.0.1:8080 \
//          -e COLLECTOR_URL=http://127.0.0.1:9090 \
//          -e RPS=50 -e DURATION=1m \
//          test/load/k6/run.js

import http from 'k6/http';
import { check } from 'k6';
import { Ledger, traceID } from './lib/ledger.js';
import { pickRoute } from './lib/routes.js';

const GATEWAY_URL = __ENV.GATEWAY_URL;
const COLLECTOR_URL = __ENV.COLLECTOR_URL;

if (!GATEWAY_URL) {
  throw new Error('GATEWAY_URL is required');
}

const rps = parseInt(__ENV.RPS || '0', 10);
const duration = __ENV.DURATION || '30s';
const vus = parseInt(__ENV.VUS || '10', 10);

export const options = rps > 0 ? {
  scenarios: {
    steady: {
      executor: 'constant-arrival-rate',
      rate: rps,
      timeUnit: '1s',
      duration,
      preAllocatedVUs: vus,
      maxVUs: vus * 4,
    },
  },
} : {
  vus,
  duration,
};

// Per-VU state: one Ledger per VU so flushes don't contend.
let ledger;

export function setup() {
  // Emit a start marker so the analyzer can bound the run window.
  if (COLLECTOR_URL) {
    const marker = JSON.stringify({ source: 'chaos', event: 'run_start', ts: Date.now() }) + '\n';
    http.post(`${COLLECTOR_URL}/ingest`, marker, { headers: { 'Content-Type': 'application/x-ndjson' } });
  }
  return {};
}

export default function () {
  if (!ledger) ledger = new Ledger(COLLECTOR_URL);

  const route = pickRoute();
  const tid = traceID();
  const url = `${GATEWAY_URL}${route.path}`;

  const start = Date.now();
  const res = http.get(url, {
    headers: {
      Host: route.host,
      'X-Trace-ID': tid,
    },
    tags: { exp_service: route.expService, host: route.host },
  });
  const latency = Date.now() - start;

  // X-Echo-Service is set by the echo handler so we can catch misroutes
  // even if the echo->collector POST was lost.
  const seenService = res.headers['X-Echo-Service'] || '';

  ledger.record({
    trace_id: tid,
    req_host: route.host,
    req_path: route.path,
    req_method: 'GET',
    exp_service: route.expService,
    status: res.status,
    latency_ms: latency,
    // Reuse the echo field so the analyzer can check without joining.
    service: seenService,
  });

  check(res, {
    'status 2xx': (r) => r.status >= 200 && r.status < 300,
    'hit expected service': (r) => seenService === route.expService,
  });
}

export function teardown() {
  // Ledger is per-VU; teardown runs in its own VU, so the last flushes
  // happen naturally when each VU exits. We only emit the run_end marker.
  if (COLLECTOR_URL) {
    const marker = JSON.stringify({ source: 'chaos', event: 'run_end', ts: Date.now() }) + '\n';
    http.post(`${COLLECTOR_URL}/ingest`, marker, { headers: { 'Content-Type': 'application/x-ndjson' } });
  }
}

// k6 calls this when a VU stops — flush whatever is left.
export function handleSummary(data) {
  if (ledger) ledger.flush();
  return { stdout: JSON.stringify(data, null, 2) };
}
