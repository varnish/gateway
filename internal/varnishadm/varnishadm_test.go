package varnishadm

import (
	"context"
	"log/slog"
	"os"
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
