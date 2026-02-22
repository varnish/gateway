package varnishadm

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMockVarnishadm_Exec(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	tests := []struct {
		name              string
		command           string
		expectedStatus    int
		expectedInPayload string
	}{
		{
			name:              "ping command",
			command:           "ping",
			expectedStatus:    ClisOk,
			expectedInPayload: "PONG",
		},
		{
			name:              "status command",
			command:           "status",
			expectedStatus:    ClisOk,
			expectedInPayload: "running",
		},
		{
			name:              "unknown command",
			command:           "unknown_command",
			expectedStatus:    ClisUnknown,
			expectedInPayload: "Unknown request",
		},
		{
			name:              "vcl.load command",
			command:           "vcl.load test /path/to/vcl",
			expectedStatus:    ClisOk,
			expectedInPayload: "VCL compiled",
		},
		{
			name:              "vcl.use command",
			command:           "vcl.use test",
			expectedStatus:    ClisOk,
			expectedInPayload: "now active",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := mock.Exec(tt.command)
			if err != nil {
				t.Fatalf("Exec() error = %v", err)
			}

			if resp.statusCode != tt.expectedStatus {
				t.Errorf("statusCode = %v, want %v", resp.statusCode, tt.expectedStatus)
			}

			if tt.expectedInPayload != "" && resp.payload == "" {
				t.Errorf("payload is empty, expected to contain %q", tt.expectedInPayload)
			}
		})
	}
}

func TestMockVarnishadm_CallHistory(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	commands := []string{"ping", "status", "banner"}

	for _, cmd := range commands {
		_, err := mock.Exec(cmd)
		if err != nil {
			t.Fatalf("Exec(%s) error = %v", cmd, err)
		}
	}

	history := mock.GetCallHistory()
	if len(history) != len(commands) {
		t.Errorf("CallHistory length = %v, want %v", len(history), len(commands))
	}

	for i, cmd := range commands {
		if history[i] != cmd {
			t.Errorf("CallHistory[%d] = %v, want %v", i, history[i], cmd)
		}
	}

	mock.ClearCallHistory()
	history = mock.GetCallHistory()
	if len(history) != 0 {
		t.Errorf("CallHistory after clear = %v, want empty", history)
	}
}

func TestMockVarnishadm_CustomResponse(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	customResp := VarnishResponse{
		statusCode: ClisCant,
		payload:    "Custom error message",
	}

	mock.SetResponse("custom_command", customResp)

	resp, err := mock.Exec("custom_command")
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}

	if resp.statusCode != customResp.statusCode {
		t.Errorf("statusCode = %v, want %v", resp.statusCode, customResp.statusCode)
	}

	if resp.payload != customResp.payload {
		t.Errorf("payload = %v, want %v", resp.payload, customResp.payload)
	}
}

func TestMockVarnishadm_Run(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if mock.IsRunning() {
		t.Error("Mock should not be running initially")
	}

	done := make(chan error, 1)
	go func() {
		done <- mock.Run(ctx)
	}()

	// Give it a moment to start
	time.Sleep(10 * time.Millisecond)

	if !mock.IsRunning() {
		t.Error("Mock should be running after Run() is called")
	}

	// Wait for context timeout
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() error = %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("Run() did not return after context timeout")
	}

	if mock.IsRunning() {
		t.Error("Mock should not be running after context cancellation")
	}
}

func TestMockVarnishadm_Delay(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	delay := 50 * time.Millisecond
	mock.SetDelay(delay)

	start := time.Now()
	_, err := mock.Exec("ping")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}

	if elapsed < delay {
		t.Errorf("Command executed too quickly, elapsed = %v, expected at least %v", elapsed, delay)
	}
}

func TestVarnishResponse(t *testing.T) {
	resp := VarnishResponse{
		statusCode: ClisOk,
		payload:    "test payload",
	}

	if resp.statusCode != ClisOk {
		t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
	}

	if resp.payload != "test payload" {
		t.Errorf("payload = %v, want %v", resp.payload, "test payload")
	}
}

func TestConstants(t *testing.T) {
	tests := []struct {
		name     string
		constant int
		expected int
	}{
		{"ClisSyntax", ClisSyntax, 100},
		{"ClisUnknown", ClisUnknown, 101},
		{"ClisOk", ClisOk, 200},
		{"ClisTruncated", ClisTruncated, 201},
		{"ClisCant", ClisCant, 300},
		{"ClisComms", ClisComms, 400},
		{"ClisClose", ClisClose, 500},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.constant != tt.expected {
				t.Errorf("%s = %v, want %v", tt.name, tt.constant, tt.expected)
			}
		})
	}
}

func TestMockConnected(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	ch := mock.Connected()
	if ch == nil {
		t.Fatal("Connected() returned nil")
	}

	// Mock's connected channel should be immediately closed
	select {
	case <-ch:
		// expected
	default:
		t.Error("Mock Connected channel should be closed immediately")
	}
}

func TestMock_VCLInline(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)
	mock.ClearCallHistory()

	resp, err := mock.VCLInline("test-vcl", "vcl 4.0; backend default { .host = \"localhost\"; }")
	if err != nil {
		t.Fatalf("VCLInline() error = %v", err)
	}
	if resp.statusCode != ClisOk {
		t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
	}

	history := mock.GetCallHistory()
	if len(history) != 1 {
		t.Fatalf("expected 1 call, got %d", len(history))
	}
	if !strings.Contains(history[0], "vcl.inline test-vcl") {
		t.Errorf("expected vcl.inline command, got %q", history[0])
	}
}

func TestMock_VCLLabel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)
	mock.ClearCallHistory()

	resp, err := mock.VCLLabel("label-api", "vcl-api-v2")
	if err != nil {
		t.Fatalf("VCLLabel() error = %v", err)
	}
	if resp.statusCode != ClisOk {
		t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
	}

	history := mock.GetCallHistory()
	expectedCmd := "vcl.label label-api vcl-api-v2"
	if len(history) != 1 || history[0] != expectedCmd {
		t.Errorf("expected command %q, got %v", expectedCmd, history)
	}
}

func TestMock_TLSCertLoadWithPrivateKey(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)
	mock.ClearCallHistory()

	resp, err := mock.TLSCertLoad("example", "/path/to/cert.pem", "/path/to/key.pem")
	if err != nil {
		t.Fatalf("TLSCertLoad() error = %v", err)
	}
	if resp.statusCode != ClisOk {
		t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
	}

	history := mock.GetCallHistory()
	expectedCmd := "tls.cert.load example /path/to/cert.pem -k /path/to/key.pem"
	if len(history) != 1 || history[0] != expectedCmd {
		t.Errorf("expected command %q, got %v", expectedCmd, history)
	}
}

func TestMock_TLSCertDiscard(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	// Set initial state with a cert
	mock.SetTLSState([]TLSCertEntry{
		{
			CertificateID: "cert-001",
			Frontend:      "main",
			State:         "active",
			Hostname:      "example.com",
			Expiration:    time.Now().Add(90 * 24 * time.Hour),
		},
	})

	// Discard existing cert (starts a transaction)
	resp, err := mock.TLSCertDiscard("cert-001")
	if err != nil {
		t.Fatalf("TLSCertDiscard() error = %v", err)
	}
	if resp.statusCode != ClisOk {
		t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
	}

	// Commit the discard
	resp, err = mock.TLSCertCommit()
	if err != nil {
		t.Fatalf("TLSCertCommit() error = %v", err)
	}
	if resp.statusCode != ClisOk {
		t.Errorf("commit statusCode = %v, want %v", resp.statusCode, ClisOk)
	}

	// Verify cert is gone
	state := mock.GetTLSState()
	if len(state) != 0 {
		t.Errorf("expected 0 certs after discard+commit, got %d", len(state))
	}
}

func TestMock_TLSCertDiscardNonExistent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	// Discard non-existent cert
	resp, err := mock.TLSCertDiscard("does-not-exist")
	if err != nil {
		t.Fatalf("TLSCertDiscard() error = %v", err)
	}
	if resp.statusCode != 300 {
		t.Errorf("statusCode = %v, want 300 (not found)", resp.statusCode)
	}
}

func TestMock_TLSTransactionLoadCommit(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	// Load a cert
	resp, err := mock.TLSCertLoad("cert-new", "/path/to/cert.pem", "")
	if err != nil {
		t.Fatalf("TLSCertLoad() error = %v", err)
	}
	if resp.statusCode != ClisOk {
		t.Errorf("load statusCode = %v, want %v", resp.statusCode, ClisOk)
	}

	// Before commit, GetTLSState should show no certs (not committed yet)
	state := mock.GetTLSState()
	if len(state) != 0 {
		t.Errorf("expected 0 committed certs before commit, got %d", len(state))
	}

	// Commit
	resp, err = mock.TLSCertCommit()
	if err != nil {
		t.Fatalf("TLSCertCommit() error = %v", err)
	}
	if resp.statusCode != ClisOk {
		t.Errorf("commit statusCode = %v, want %v", resp.statusCode, ClisOk)
	}

	// After commit, GetTLSState should show the cert
	state = mock.GetTLSState()
	if len(state) != 1 {
		t.Fatalf("expected 1 committed cert after commit, got %d", len(state))
	}
	if state[0].CertificateID != "cert-new" {
		t.Errorf("CertificateID = %q, want cert-new", state[0].CertificateID)
	}
}

func TestMock_TLSTransactionLoadRollback(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	// Load a cert
	_, err := mock.TLSCertLoad("cert-temp", "/path/to/cert.pem", "")
	if err != nil {
		t.Fatalf("TLSCertLoad() error = %v", err)
	}

	// Rollback
	resp, err := mock.TLSCertRollback()
	if err != nil {
		t.Fatalf("TLSCertRollback() error = %v", err)
	}
	if resp.statusCode != ClisOk {
		t.Errorf("rollback statusCode = %v, want %v", resp.statusCode, ClisOk)
	}

	// After rollback, no certs should be committed
	state := mock.GetTLSState()
	if len(state) != 0 {
		t.Errorf("expected 0 committed certs after rollback, got %d", len(state))
	}
}

func TestMock_TLSTransactionDuplicateLoad(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	// Load a cert
	_, err := mock.TLSCertLoad("cert-dup", "/path/to/cert.pem", "")
	if err != nil {
		t.Fatalf("first TLSCertLoad() error = %v", err)
	}

	// Load same cert again — should fail
	resp, err := mock.TLSCertLoad("cert-dup", "/path/to/cert2.pem", "")
	if err != nil {
		t.Fatalf("second TLSCertLoad() error = %v", err)
	}
	if resp.statusCode != 300 {
		t.Errorf("duplicate load statusCode = %v, want 300", resp.statusCode)
	}
	if !strings.Contains(resp.payload, "already exists") {
		t.Errorf("expected 'already exists' in payload, got %q", resp.payload)
	}
}

func TestMock_TLSCommitNoTransaction(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	resp, err := mock.TLSCertCommit()
	if err != nil {
		t.Fatalf("TLSCertCommit() error = %v", err)
	}
	if resp.statusCode != ClisOk {
		t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
	}
	if !strings.Contains(resp.payload, "No changes") {
		t.Errorf("expected 'No changes' in payload, got %q", resp.payload)
	}
}

func TestMock_TLSRollbackNoTransaction(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	resp, err := mock.TLSCertRollback()
	if err != nil {
		t.Fatalf("TLSCertRollback() error = %v", err)
	}
	if resp.statusCode != ClisOk {
		t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
	}
	if !strings.Contains(resp.payload, "No changes") {
		t.Errorf("expected 'No changes' in payload, got %q", resp.payload)
	}
}

func TestMock_GetTLSState(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	// Initially empty
	state := mock.GetTLSState()
	if len(state) != 0 {
		t.Errorf("initial state should be empty, got %d entries", len(state))
	}

	// Set state with multiple certs
	certs := []TLSCertEntry{
		{CertificateID: "cert-a", Frontend: "main", State: "active", Hostname: "a.example.com"},
		{CertificateID: "cert-b", Frontend: "api", State: "active", Hostname: "b.example.com"},
	}
	mock.SetTLSState(certs)

	state = mock.GetTLSState()
	if len(state) != 2 {
		t.Fatalf("expected 2 certs, got %d", len(state))
	}

	// Verify certs are present (order may vary due to map iteration)
	certIDs := make(map[string]bool)
	for _, cert := range state {
		certIDs[cert.CertificateID] = true
	}
	if !certIDs["cert-a"] || !certIDs["cert-b"] {
		t.Errorf("expected cert-a and cert-b, got %v", certIDs)
	}
}

func TestMock_TLSCertLoadBadUsage(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	// Send a malformed tls.cert.load command (too few args)
	resp, err := mock.Exec("tls.cert.load")
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if resp.statusCode != ClisUnknown {
		t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisUnknown)
	}
}

func TestMock_TLSCertDiscardBadUsage(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	// Send a malformed tls.cert.discard command (too few args)
	resp, err := mock.Exec("tls.cert.discard")
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if resp.statusCode != ClisUnknown {
		t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisUnknown)
	}
}

func TestMock_TLSCertListFromState(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	// Set some TLS state and verify the list output
	mock.SetTLSState([]TLSCertEntry{
		{
			CertificateID: "cert-test",
			Frontend:      "main",
			State:         "active",
			Hostname:      "test.example.com",
			Expiration:    time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC),
			OCSPStapling:  true,
		},
	})

	resp, err := mock.TLSCertList()
	if err != nil {
		t.Fatalf("TLSCertList() error = %v", err)
	}
	if resp.statusCode != ClisOk {
		t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
	}
	if !strings.Contains(resp.payload, "cert-test") {
		t.Errorf("expected cert-test in payload, got %q", resp.payload)
	}
	if !strings.Contains(resp.payload, "test.example.com") {
		t.Errorf("expected hostname in payload, got %q", resp.payload)
	}
}

func TestMock_TLSCertListEmptyFrontend(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	// Set state with empty Frontend — should default to "default"
	mock.SetTLSState([]TLSCertEntry{
		{
			CertificateID: "cert-x",
			Frontend:      "",
			State:         "active",
			Hostname:      "x.example.com",
			Expiration:    time.Now().Add(24 * time.Hour),
		},
	})

	resp, err := mock.TLSCertList()
	if err != nil {
		t.Fatalf("TLSCertList() error = %v", err)
	}
	if !strings.Contains(resp.payload, "default") {
		t.Errorf("expected 'default' frontend in payload, got %q", resp.payload)
	}
}

func TestParseBanner(t *testing.T) {
	testCases := []struct {
		name            string
		banner          string
		expectedEnv     string
		expectedVersion string
	}{
		{
			name: "Linux Varnish Plus banner from logs",
			banner: `-----------------------------
Varnish Cache CLI 1.0
-----------------------------
Linux,6.8.0-79-generic,x86_64,-jlinux,-smse4,-hcritbit
varnish-plus-6.0.15r1 revision d0b65fce8c712013f9bd614bacca1e67a45799e8

Type 'help' for command list.
Type 'quit' to close CLI session.`,
			expectedEnv:     "Linux,6.8.0-79-generic,x86_64,-jlinux,-smse4,-hcritbit",
			expectedVersion: "varnish-plus-6.0.15r1 revision d0b65fce8c712013f9bd614bacca1e67a45799e8",
		},
		{
			name: "Darwin banner from user example",
			banner: `-----------------------------
Varnish Cache CLI 1.0
-----------------------------
Darwin,24.6.0,arm64,-jnone,-smse4,-sdefault,-hcritbit
varnish-7.7.3 revision 6884b75af9c9bdb2c9b6e2aa464a435e42cb4931

Type 'help' for command list.
Type 'quit' to close CLI session.
Type 'start' to launch worker process.`,
			expectedEnv:     "Darwin,24.6.0,arm64,-jnone,-smse4,-sdefault,-hcritbit",
			expectedVersion: "varnish-7.7.3 revision 6884b75af9c9bdb2c9b6e2aa464a435e42cb4931",
		},
		{
			name:            "Empty banner",
			banner:          "",
			expectedEnv:     "",
			expectedVersion: "",
		},
		{
			name: "Malformed banner",
			banner: `Some random text
without proper format`,
			expectedEnv:     "",
			expectedVersion: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			env, version := parseBanner(tc.banner)
			if env != tc.expectedEnv {
				t.Errorf("Expected environment '%s', got '%s'", tc.expectedEnv, env)
			}
			if version != tc.expectedVersion {
				t.Errorf("Expected version '%s', got '%s'", tc.expectedVersion, version)
			}
		})
	}
}
