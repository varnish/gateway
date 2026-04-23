// Package ledger defines the on-the-wire record shared by echo, k6 (via JSON),
// collector, and analyzer.
package ledger

// Source identifies which component emitted a record.
type Source string

const (
	SourceK6    Source = "k6"
	SourceEcho  Source = "echo"
	SourceChaos Source = "chaos"
)

// Record is a single ledger entry. JSON field names are stable — k6 writes
// them too.
type Record struct {
	Source  Source `json:"source"`
	TraceID string `json:"trace_id,omitempty"`
	// Unix milliseconds.
	TS int64 `json:"ts"`

	// k6 fields
	ReqHost    string `json:"req_host,omitempty"`
	ReqPath    string `json:"req_path,omitempty"`
	ReqMethod  string `json:"req_method,omitempty"`
	ExpService string `json:"exp_service,omitempty"`
	Status     int    `json:"status,omitempty"`
	LatencyMs  int64  `json:"latency_ms,omitempty"`

	// echo fields
	Pod       string `json:"pod,omitempty"`
	Service   string `json:"service,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	SeenHost  string `json:"seen_host,omitempty"`
	SeenPath  string `json:"seen_path,omitempty"`

	// chaos fields — a free-form event stream for convergence analysis
	Event  string `json:"event,omitempty"`
	Target string `json:"target,omitempty"`
}
