package varnishadm

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ParseVCLListJSON parses the payload of `vcl.list -j`. The CLI JSON envelope
// is [version, [command, args...], timestamp, entry, entry, ...]; entries
// start at index 3. Varnish 9 entries look like:
//
//	{"status": "available", "state": "auto", "temperature": "warm",
//	 "busy": 0, "name": "ph", "labels": 2}
//	{"status": "available", "state": "label", "temperature": "warm",
//	 "busy": 0, "name": "gwvcl-0", "label": {"name": "ph", "refs": 1}}
//
// "busy" is absent while the child is not running, "labels" is absent for a
// VCL nothing labels, and "label.refs" (how many VCLs return(vcl) to the
// label) is absent when zero.
func ParseVCLListJSON(payload string) ([]VCLEntry, error) {
	var envelope []json.RawMessage
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return nil, fmt.Errorf("json.Unmarshal(vcl.list -j): %w", err)
	}
	if len(envelope) < 3 {
		return nil, fmt.Errorf("vcl.list -j: short envelope (%d elements)", len(envelope))
	}
	entries := make([]VCLEntry, 0, len(envelope)-3)
	for i, raw := range envelope[3:] {
		var e struct {
			Name        string `json:"name"`
			Status      string `json:"status"`
			State       string `json:"state"`
			Temperature string `json:"temperature"`
			Busy        int    `json:"busy"`
			Labels      int    `json:"labels"`
			Label       *struct {
				Name string `json:"name"`
				Refs int    `json:"refs"`
			} `json:"label"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("json.Unmarshal(vcl.list -j entry %d): %w", i, err)
		}
		if e.Name == "" {
			return nil, fmt.Errorf("vcl.list -j entry %d: missing name", i)
		}
		entry := VCLEntry{
			Name:        e.Name,
			Status:      e.Status,
			State:       e.State,
			Temperature: e.Temperature,
			Busy:        e.Busy,
			Labels:      e.Labels,
		}
		if e.Label != nil {
			entry.LabelTarget = e.Label.Name
			entry.LabelRefs = e.Label.Refs
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// parseTLSCertList parses the output from tls.cert.list command
// Expected format: Frontend State   Hostname         Certificate ID  Expiration date           OCSP stapling
func parseTLSCertList(payload string) (*TLSCertListResult, error) {
	result := &TLSCertListResult{}

	lines := strings.Split(strings.TrimSpace(payload), "\n")

	// Skip header line if present (contains "Frontend", "State", etc.)
	startIndex := 0
	if len(lines) > 0 && strings.Contains(lines[0], "Frontend") {
		startIndex = 1
	}

	for i := startIndex; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}

		entry, err := parseTLSCertLine(line)
		if err != nil {
			return nil, fmt.Errorf("error parsing TLS cert line %q: %w", line, err)
		}

		result.Entries = append(result.Entries, entry)
	}

	return result, nil
}

// parseTLSCertLine parses a single line from tls.cert.list output
// Expected format: "frontend state hostname cert-id Feb 29 13:38:00 2042 GMT true"
func parseTLSCertLine(line string) (TLSCertEntry, error) {
	entry := TLSCertEntry{}

	// Split by multiple spaces to handle column alignment
	parts := strings.Fields(line)
	// Need at least: frontend, state, hostname, cert-id, date(5 parts), ocsp = 10 parts
	if len(parts) < 10 {
		return entry, fmt.Errorf("insufficient parts in TLS cert line, expected at least 10, got %d", len(parts))
	}

	entry.Frontend = parts[0]
	entry.State = parts[1]
	entry.Hostname = parts[2]
	entry.CertificateID = parts[3]

	// Parse expiration date - format: "Feb 29 13:38:00 2042 GMT"
	// This is parts[4] through parts[8]
	dateStr := strings.Join(parts[4:9], " ")
	if expiration, err := time.Parse("Jan 02 15:04:05 2006 MST", dateStr); err == nil {
		entry.Expiration = expiration
	} else {
		// If parsing fails, log but don't error - expiration is optional for functionality
		// Could try alternative formats here if needed
	}

	// OCSP stapling is parts[9]
	ocspStr := parts[9]
	entry.OCSPStapling = strings.ToLower(ocspStr) == "enabled" || strings.ToLower(ocspStr) == "true"

	return entry, nil
}
