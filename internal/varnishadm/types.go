package varnishadm

import (
	"fmt"
	"time"
)

// VCLEntry is one entry of `vcl.list -j`.
type VCLEntry struct {
	Name        string // VCL or label name
	Status      string // "active" or "available"
	State       string // "auto", "cold", "warm" or "label"
	Temperature string // "warm", "cold", "cooling", ...
	Busy        int    // in-flight references in the child (0 when no child)
	Labels      int    // number of labels pointing at this VCL
	LabelTarget string // for State == "label": the VCL the label points at
	LabelRefs   int    // for State == "label": VCLs that return(vcl) to this label
}

// IsLabel reports whether the entry is a label rather than a loaded VCL.
func (e VCLEntry) IsLabel() bool {
	return e.State == "label"
}

// TLSCertEntry represents a single TLS certificate entry from tls.cert.list
type TLSCertEntry struct {
	Frontend      string    // Frontend name/identifier
	State         string    // Certificate state
	Hostname      string    // Hostname the certificate is for
	CertificateID string    // Certificate ID
	Expiration    time.Time // Certificate expiration date
	OCSPStapling  bool      // Whether OCSP stapling is enabled
}

// TLSCertListResult contains the parsed result of tls.cert.list command
type TLSCertListResult struct {
	Entries []TLSCertEntry // slice of TLS certificate entries
}

// Size represents a size value with unit (K, M, G, T) for Varnish parameters
type Size struct {
	Value uint64
	Unit  string // "K", "M", "G", "T"
}

// String converts Size to Varnish format (e.g., "256M")
func (s Size) String() string {
	return fmt.Sprintf("%d%s", s.Value, s.Unit)
}
