package varnishadm

import (
	"fmt"
	"time"
)

// VCLEntry represents a single VCL configuration entry from vcl.list
type VCLEntry struct {
	Name        string // VCL configuration name
	Status      string // "active", "available"
	Temperature string // "auto/warm", "label/warm", etc.
	Labels      int    // number of labels in parentheses
	LabelTarget string // for label entries (e.g., "vcl-api-orig" from "label-api -> vcl-api-orig")
	Returns     int    // number of return statements for labels
}

// VCLListResult contains the parsed result of vcl.list command
type VCLListResult struct {
	Entries []VCLEntry // slice of VCL entries
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
