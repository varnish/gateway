package varnishadm

import (
	"testing"
	"time"
)

func TestParseVCLList(t *testing.T) {
	tests := []struct {
		name     string
		payload  string
		expected *VCLListResult
		wantErr  bool
	}{
		{
			name: "Complete VCL list output",
			payload: `active      auto/warm          - vcl-api-orig (1 label)
available   auto/warm          - vcl-catz-orig (1 label)
available  label/warm          - label-api -> vcl-api-orig (1 return(vcl))
available  label/warm          - label-catz -> vcl-catz-orig (1 return(vcl))
available   auto/warm          - vcl-root-orig`,
			expected: &VCLListResult{
				Entries: []VCLEntry{
					{
						Name:        "vcl-api-orig",
						Status:      "active",
						Temperature: "auto/warm",
						Labels:      1,
						Returns:     0,
					},
					{
						Name:        "vcl-catz-orig",
						Status:      "available",
						Temperature: "auto/warm",
						Labels:      1,
						Returns:     0,
					},
					{
						Name:        "label-api",
						Status:      "available",
						Temperature: "label/warm",
						Labels:      0,
						Returns:     1,
						LabelTarget: "vcl-api-orig",
					},
					{
						Name:        "label-catz",
						Status:      "available",
						Temperature: "label/warm",
						Labels:      0,
						Returns:     1,
						LabelTarget: "vcl-catz-orig",
					},
					{
						Name:        "vcl-root-orig",
						Status:      "available",
						Temperature: "auto/warm",
						Labels:      0,
						Returns:     0,
					},
				},
			},
			wantErr: false,
		},
		{
			name:    "Empty payload",
			payload: "",
			expected: &VCLListResult{
				Entries: []VCLEntry{},
			},
			wantErr: false,
		},
		{
			name:    "Single active VCL",
			payload: `active      auto/warm          - boot`,
			expected: &VCLListResult{
				Entries: []VCLEntry{
					{
						Name:        "boot",
						Status:      "active",
						Temperature: "auto/warm",
						Labels:      0,
						Returns:     0,
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseVCLList(tt.payload)

			if (err != nil) != tt.wantErr {
				t.Errorf("parseVCLList() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil {
				return
			}

			if len(result.Entries) != len(tt.expected.Entries) {
				t.Errorf("parseVCLList() got %d entries, want %d", len(result.Entries), len(tt.expected.Entries))
				return
			}

			for i, entry := range result.Entries {
				expected := tt.expected.Entries[i]
				if entry.Name != expected.Name {
					t.Errorf("Entry[%d].Name = %q, want %q", i, entry.Name, expected.Name)
				}
				if entry.Status != expected.Status {
					t.Errorf("Entry[%d].Status = %q, want %q", i, entry.Status, expected.Status)
				}
				if entry.Temperature != expected.Temperature {
					t.Errorf("Entry[%d].Temperature = %q, want %q", i, entry.Temperature, expected.Temperature)
				}
				if entry.Labels != expected.Labels {
					t.Errorf("Entry[%d].Labels = %d, want %d", i, entry.Labels, expected.Labels)
				}
				if entry.Returns != expected.Returns {
					t.Errorf("Entry[%d].Returns = %d, want %d", i, entry.Returns, expected.Returns)
				}
				if entry.LabelTarget != expected.LabelTarget {
					t.Errorf("Entry[%d].LabelTarget = %q, want %q", i, entry.LabelTarget, expected.LabelTarget)
				}
			}
		})
	}
}

func TestParseTLSCertList(t *testing.T) {
	tests := []struct {
		name     string
		payload  string
		expected *TLSCertListResult
		wantErr  bool
	}{
		{
			name: "Complete TLS cert list output",
			payload: `Frontend State   Hostname         Certificate ID  Expiration date           OCSP stapling
main     active  example.com      cert-001        Dec 31 23:59:59 2024 GMT  true
api      active  api.example.com  cert-002        Nov 30 12:00:00 2024 GMT  false`,
			expected: &TLSCertListResult{
				Entries: []TLSCertEntry{
					{
						Frontend:      "main",
						State:         "active",
						Hostname:      "example.com",
						CertificateID: "cert-001",
						Expiration:    time.Date(2024, 12, 31, 23, 59, 59, 0, time.FixedZone("GMT", 0)),
						OCSPStapling:  true,
					},
					{
						Frontend:      "api",
						State:         "active",
						Hostname:      "api.example.com",
						CertificateID: "cert-002",
						Expiration:    time.Date(2024, 11, 30, 12, 0, 0, 0, time.FixedZone("GMT", 0)),
						OCSPStapling:  false,
					},
				},
			},
			wantErr: false,
		},
		{
			name:    "No header line",
			payload: `main     active  example.com      cert-001        Dec 31 23:59:59 2024 GMT  true`,
			expected: &TLSCertListResult{
				Entries: []TLSCertEntry{
					{
						Frontend:      "main",
						State:         "active",
						Hostname:      "example.com",
						CertificateID: "cert-001",
						Expiration:    time.Date(2024, 12, 31, 23, 59, 59, 0, time.FixedZone("GMT", 0)),
						OCSPStapling:  true,
					},
				},
			},
			wantErr: false,
		},
		{
			name:    "Empty payload",
			payload: "",
			expected: &TLSCertListResult{
				Entries: []TLSCertEntry{},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseTLSCertList(tt.payload)

			if (err != nil) != tt.wantErr {
				t.Errorf("parseTLSCertList() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil {
				return
			}

			if len(result.Entries) != len(tt.expected.Entries) {
				t.Errorf("parseTLSCertList() got %d entries, want %d", len(result.Entries), len(tt.expected.Entries))
				return
			}

			for i, entry := range result.Entries {
				expected := tt.expected.Entries[i]
				if entry.Frontend != expected.Frontend {
					t.Errorf("Entry[%d].Frontend = %q, want %q", i, entry.Frontend, expected.Frontend)
				}
				if entry.State != expected.State {
					t.Errorf("Entry[%d].State = %q, want %q", i, entry.State, expected.State)
				}
				if entry.Hostname != expected.Hostname {
					t.Errorf("Entry[%d].Hostname = %q, want %q", i, entry.Hostname, expected.Hostname)
				}
				if entry.CertificateID != expected.CertificateID {
					t.Errorf("Entry[%d].CertificateID = %q, want %q", i, entry.CertificateID, expected.CertificateID)
				}
				if !entry.Expiration.Equal(expected.Expiration) {
					t.Errorf("Entry[%d].Expiration = %v, want %v", i, entry.Expiration, expected.Expiration)
				}
				if entry.OCSPStapling != expected.OCSPStapling {
					t.Errorf("Entry[%d].OCSPStapling = %v, want %v", i, entry.OCSPStapling, expected.OCSPStapling)
				}
			}
		})
	}
}

func TestParseVCLLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		expected VCLEntry
		wantErr  bool
	}{
		{
			name: "Active VCL with labels",
			line: "active      auto/warm          - vcl-api-orig (1 label)",
			expected: VCLEntry{
				Name:        "vcl-api-orig",
				Status:      "active",
				Temperature: "auto/warm",
				Labels:      1,
				Returns:     0,
			},
			wantErr: false,
		},
		{
			name: "Label VCL with returns",
			line: "available  label/warm          - label-api -> vcl-api-orig (1 return(vcl))",
			expected: VCLEntry{
				Name:        "label-api",
				Status:      "available",
				Temperature: "label/warm",
				Labels:      0,
				Returns:     1,
				LabelTarget: "vcl-api-orig",
			},
			wantErr: false,
		},
		{
			name: "Simple VCL without parentheses",
			line: "available   auto/warm          - vcl-root-orig",
			expected: VCLEntry{
				Name:        "vcl-root-orig",
				Status:      "available",
				Temperature: "auto/warm",
				Labels:      0,
				Returns:     0,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseVCLLine(tt.line)

			if (err != nil) != tt.wantErr {
				t.Errorf("parseVCLLine() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err != nil {
				return
			}

			if result != tt.expected {
				t.Errorf("parseVCLLine() = %+v, want %+v", result, tt.expected)
			}
		})
	}
}

func TestParseParenthesesContent(t *testing.T) {
	tests := []struct {
		name            string
		content         string
		expectedLabels  int
		expectedReturns int
	}{
		{
			name:            "Labels only",
			content:         "(1 label)",
			expectedLabels:  1,
			expectedReturns: 0,
		},
		{
			name:            "Returns only",
			content:         "(1 return(vcl))",
			expectedLabels:  0,
			expectedReturns: 1,
		},
		{
			name:            "Multiple labels",
			content:         "(3 label)",
			expectedLabels:  3,
			expectedReturns: 0,
		},
		{
			name:            "No match",
			content:         "(something else)",
			expectedLabels:  0,
			expectedReturns: 0,
		},
		{
			name:            "No parentheses at all",
			content:         "no parens here",
			expectedLabels:  0,
			expectedReturns: 0,
		},
		{
			name:            "Empty string",
			content:         "",
			expectedLabels:  0,
			expectedReturns: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels, returns := parseParenthesesContent(tt.content)

			if labels != tt.expectedLabels {
				t.Errorf("parseParenthesesContent() labels = %d, want %d", labels, tt.expectedLabels)
			}
			if returns != tt.expectedReturns {
				t.Errorf("parseParenthesesContent() returns = %d, want %d", returns, tt.expectedReturns)
			}
		})
	}
}

func TestParseVCLLine_TooFewParts(t *testing.T) {
	_, err := parseVCLLine("active auto/warm")
	if err == nil {
		t.Error("expected error for line with insufficient parts")
	}
}

func TestParseVCLList_MalformedLine(t *testing.T) {
	// A well-formed line followed by a malformed one
	payload := "active      auto/warm          - boot\ntoo short"
	_, err := parseVCLList(payload)
	if err == nil {
		t.Error("expected error for malformed line in vcl.list output")
	}
}

func TestParseTLSCertLine_TooFewParts(t *testing.T) {
	_, err := parseTLSCertLine("main active example.com")
	if err == nil {
		t.Error("expected error for TLS cert line with insufficient parts")
	}
}

func TestParseTLSCertLine_OCSPEnabled(t *testing.T) {
	// "enabled" should be treated as OCSP=true
	line := "main     active  example.com      cert-001        Dec 31 23:59:59 2024 GMT  enabled"
	entry, err := parseTLSCertLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if !entry.OCSPStapling {
		t.Error("expected OCSPStapling=true for 'enabled'")
	}
}

func TestParseTLSCertLine_OCSPDisabledVariant(t *testing.T) {
	// "disabled" should be treated as OCSP=false
	line := "main     active  example.com      cert-001        Dec 31 23:59:59 2024 GMT  disabled"
	entry, err := parseTLSCertLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if entry.OCSPStapling {
		t.Error("expected OCSPStapling=false for 'disabled'")
	}
}

func TestParseTLSCertLine_InvalidDate(t *testing.T) {
	// Invalid date format — should still parse without error (date field is zero)
	line := "main     active  example.com      cert-001        not a real date and GMT  false"
	entry, err := parseTLSCertLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if entry.CertificateID != "cert-001" {
		t.Errorf("CertificateID = %q, want cert-001", entry.CertificateID)
	}
	if !entry.Expiration.IsZero() {
		t.Errorf("Expiration should be zero for invalid date, got %v", entry.Expiration)
	}
}

func TestParseTLSCertList_MalformedLine(t *testing.T) {
	payload := "Frontend State   Hostname         Certificate ID  Expiration date           OCSP stapling\ntoo short"
	_, err := parseTLSCertList(payload)
	if err == nil {
		t.Error("expected error for malformed TLS cert line")
	}
}

func TestParseTLSCertList_OnlyHeader(t *testing.T) {
	payload := "Frontend State   Hostname         Certificate ID  Expiration date           OCSP stapling"
	result, err := parseTLSCertList(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(result.Entries))
	}
}

func TestParseVCLList_BlankLines(t *testing.T) {
	payload := "\n\nactive      auto/warm          - boot\n\n"
	result, err := parseVCLList(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(result.Entries))
	}
}

func TestParseTLSCertList_BlankLines(t *testing.T) {
	payload := "\nmain     active  example.com      cert-001        Dec 31 23:59:59 2024 GMT  true\n\n"
	result, err := parseTLSCertList(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(result.Entries))
	}
}
