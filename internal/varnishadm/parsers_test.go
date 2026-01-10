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
