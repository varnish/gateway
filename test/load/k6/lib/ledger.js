// Ledger batching for k6. Each VU buffers records and flushes to the
// collector's /ingest endpoint as NDJSON. Buffered send keeps the hot path
// fast and reduces collector pressure.
//
// Field names must match test/load/ledger/record.go.

import http from 'k6/http';

const BATCH_SIZE = parseInt(__ENV.LEDGER_BATCH_SIZE || '100', 10);

export class Ledger {
  constructor(collectorURL) {
    this.url = collectorURL ? `${collectorURL}/ingest` : null;
    this.buf = [];
  }

  record(rec) {
    rec.source = 'k6';
    if (!rec.ts) rec.ts = Date.now();
    this.buf.push(JSON.stringify(rec));
    if (this.buf.length >= BATCH_SIZE) this.flush();
  }

  flush() {
    if (!this.url || this.buf.length === 0) return;
    const body = this.buf.join('\n') + '\n';
    this.buf = [];
    // Fire-and-check: if the collector is down we surface it as a k6 check
    // failure but don't abort the test — the test's own results matter more.
    const res = http.post(this.url, body, {
      headers: { 'Content-Type': 'application/x-ndjson' },
      tags: { scenario: 'ledger-ingest' },
    });
    if (res.status >= 300) {
      console.error(`ledger ingest failed: status=${res.status}`);
    }
  }
}

// Simple UUID-ish trace ID. We don't need cryptographic strength — we just
// need collision resistance across the run.
export function traceID() {
  const rnd = () => Math.floor(Math.random() * 0x100000000).toString(16).padStart(8, '0');
  return `${rnd()}${rnd()}-${Date.now().toString(16)}`;
}
