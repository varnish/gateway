package varnishadm

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCommands_StandardCommands(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	tests := []struct {
		name           string
		commandFunc    func() (VarnishResponse, error)
		expectedStatus int
		expectedCmd    string
	}{
		{
			name:           "Ping command",
			commandFunc:    mock.Ping,
			expectedStatus: ClisOk,
			expectedCmd:    "ping",
		},
		{
			name:           "Status command",
			commandFunc:    mock.Status,
			expectedStatus: ClisOk,
			expectedCmd:    "status",
		},
		{
			name:           "Start command",
			commandFunc:    mock.Start,
			expectedStatus: ClisOk,
			expectedCmd:    "start",
		},
		{
			name:           "Stop command",
			commandFunc:    mock.Stop,
			expectedStatus: ClisOk,
			expectedCmd:    "stop",
		},
		{
			name:           "PanicShow command",
			commandFunc:    mock.PanicShow,
			expectedStatus: ClisOk,
			expectedCmd:    "panic.show",
		},
		{
			name:           "PanicClear command",
			commandFunc:    mock.PanicClear,
			expectedStatus: ClisOk,
			expectedCmd:    "panic.clear",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock.ClearCallHistory()

			// Set default response for unknown commands
			mock.SetResponse(tt.expectedCmd, VarnishResponse{
				statusCode: tt.expectedStatus,
				payload:    "OK",
			})

			resp, err := tt.commandFunc()
			if err != nil {
				t.Fatalf("Command() error = %v", err)
			}

			if resp.statusCode != tt.expectedStatus {
				t.Errorf("statusCode = %v, want %v", resp.statusCode, tt.expectedStatus)
			}

			// Check call history
			history := mock.GetCallHistory()
			if len(history) != 1 {
				t.Errorf("Expected 1 command in history, got %d", len(history))
			}
			if history[0] != tt.expectedCmd {
				t.Errorf("Expected command %q, got %q", tt.expectedCmd, history[0])
			}
		})
	}
}

func TestCommands_VCLCommands(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	t.Run("VCLList", func(t *testing.T) {
		mock.ClearCallHistory()

		resp, err := mock.VCLList()
		if err != nil {
			t.Fatalf("VCLList() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		if len(history) != 1 || history[0] != "vcl.list" {
			t.Errorf("Expected vcl.list command, got %v", history)
		}
	})

	t.Run("VCLLoad", func(t *testing.T) {
		mock.ClearCallHistory()

		resp, err := mock.VCLLoad("test", "/path/to/vcl")
		if err != nil {
			t.Fatalf("VCLLoad() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		expectedCmd := "vcl.load test /path/to/vcl"
		if len(history) != 1 || history[0] != expectedCmd {
			t.Errorf("Expected command %q, got %v", expectedCmd, history)
		}
	})

	t.Run("VCLUse", func(t *testing.T) {
		mock.ClearCallHistory()

		resp, err := mock.VCLUse("test")
		if err != nil {
			t.Fatalf("VCLUse() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		expectedCmd := "vcl.use test"
		if len(history) != 1 || history[0] != expectedCmd {
			t.Errorf("Expected command %q, got %v", expectedCmd, history)
		}
	})

	t.Run("VCLDiscard", func(t *testing.T) {
		mock.ClearCallHistory()
		mock.SetResponse("vcl.discard test", VarnishResponse{
			statusCode: ClisOk,
			payload:    "VCL 'test' discarded",
		})

		resp, err := mock.VCLDiscard("test")
		if err != nil {
			t.Fatalf("VCLDiscard() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		expectedCmd := "vcl.discard test"
		if len(history) != 1 || history[0] != expectedCmd {
			t.Errorf("Expected command %q, got %v", expectedCmd, history)
		}
	})
}

func TestCommands_ParamCommands(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	t.Run("ParamShow with name", func(t *testing.T) {
		mock.ClearCallHistory()
		mock.SetResponse("param.show thread_pool_min", VarnishResponse{
			statusCode: ClisOk,
			payload:    "thread_pool_min = 5",
		})

		resp, err := mock.ParamShow("thread_pool_min")
		if err != nil {
			t.Fatalf("ParamShow() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		expectedCmd := "param.show thread_pool_min"
		if len(history) != 1 || history[0] != expectedCmd {
			t.Errorf("Expected command %q, got %v", expectedCmd, history)
		}
	})

	t.Run("ParamShow without name", func(t *testing.T) {
		mock.ClearCallHistory()
		mock.SetResponse("param.show", VarnishResponse{
			statusCode: ClisOk,
			payload:    "All parameters...",
		})

		resp, err := mock.ParamShow("")
		if err != nil {
			t.Fatalf("ParamShow() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		expectedCmd := "param.show"
		if len(history) != 1 || history[0] != expectedCmd {
			t.Errorf("Expected command %q, got %v", expectedCmd, history)
		}
	})

	t.Run("ParamSet", func(t *testing.T) {
		mock.ClearCallHistory()
		mock.SetResponse("param.set thread_pool_min 10", VarnishResponse{
			statusCode: ClisOk,
			payload:    "thread_pool_min = 10",
		})

		resp, err := mock.ParamSet("thread_pool_min", "10")
		if err != nil {
			t.Fatalf("ParamSet() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		expectedCmd := "param.set thread_pool_min 10"
		if len(history) != 1 || history[0] != expectedCmd {
			t.Errorf("Expected command %q, got %v", expectedCmd, history)
		}
	})

	t.Run("ParamSetTyped_Int", func(t *testing.T) {
		mock.ClearCallHistory()
		mock.SetResponse("param.set thread_pool_min 200", VarnishResponse{
			statusCode: ClisOk,
			payload:    "thread_pool_min = 200",
		})

		resp, err := ParamSetTyped(mock, "thread_pool_min", 200)
		if err != nil {
			t.Fatalf("ParamSetTyped() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		expectedCmd := "param.set thread_pool_min 200"
		if len(history) != 1 || history[0] != expectedCmd {
			t.Errorf("Expected command %q, got %v", expectedCmd, history)
		}
	})

	t.Run("ParamSetTyped_Bool_True", func(t *testing.T) {
		mock.ClearCallHistory()
		mock.SetResponse("param.set debug on", VarnishResponse{
			statusCode: ClisOk,
			payload:    "debug = on",
		})

		resp, err := ParamSetTyped(mock, "debug", true)
		if err != nil {
			t.Fatalf("ParamSetTyped() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		expectedCmd := "param.set debug on"
		if len(history) != 1 || history[0] != expectedCmd {
			t.Errorf("Expected command %q, got %v", expectedCmd, history)
		}
	})

	t.Run("ParamSetTyped_Bool_False", func(t *testing.T) {
		mock.ClearCallHistory()
		mock.SetResponse("param.set debug off", VarnishResponse{
			statusCode: ClisOk,
			payload:    "debug = off",
		})

		resp, err := ParamSetTyped(mock, "debug", false)
		if err != nil {
			t.Fatalf("ParamSetTyped() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		expectedCmd := "param.set debug off"
		if len(history) != 1 || history[0] != expectedCmd {
			t.Errorf("Expected command %q, got %v", expectedCmd, history)
		}
	})

	t.Run("ParamSetTyped_Float", func(t *testing.T) {
		mock.ClearCallHistory()
		mock.SetResponse("param.set some_ratio 1.5", VarnishResponse{
			statusCode: ClisOk,
			payload:    "some_ratio = 1.5",
		})

		resp, err := ParamSetTyped(mock, "some_ratio", 1.5)
		if err != nil {
			t.Fatalf("ParamSetTyped() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		expectedCmd := "param.set some_ratio 1.5"
		if len(history) != 1 || history[0] != expectedCmd {
			t.Errorf("Expected command %q, got %v", expectedCmd, history)
		}
	})

	t.Run("ParamSetTyped_String", func(t *testing.T) {
		mock.ClearCallHistory()
		mock.SetResponse("param.set vcc_feature +allow_inline_c", VarnishResponse{
			statusCode: ClisOk,
			payload:    "vcc_feature = +allow_inline_c",
		})

		resp, err := ParamSetTyped(mock, "vcc_feature", "+allow_inline_c")
		if err != nil {
			t.Fatalf("ParamSetTyped() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		expectedCmd := "param.set vcc_feature +allow_inline_c"
		if len(history) != 1 || history[0] != expectedCmd {
			t.Errorf("Expected command %q, got %v", expectedCmd, history)
		}
	})

	t.Run("ParamSetTyped_Duration", func(t *testing.T) {
		mock.ClearCallHistory()
		mock.SetResponse("param.set timeout_idle 300s", VarnishResponse{
			statusCode: ClisOk,
			payload:    "timeout_idle = 300s",
		})

		resp, err := ParamSetTyped(mock, "timeout_idle", 5*time.Minute)
		if err != nil {
			t.Fatalf("ParamSetTyped() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		expectedCmd := "param.set timeout_idle 300s"
		if len(history) != 1 || history[0] != expectedCmd {
			t.Errorf("Expected command %q, got %v", expectedCmd, history)
		}
	})

	t.Run("ParamSetTyped_Size", func(t *testing.T) {
		mock.ClearCallHistory()
		mock.SetResponse("param.set workspace_backend 256M", VarnishResponse{
			statusCode: ClisOk,
			payload:    "workspace_backend = 256M",
		})

		resp, err := ParamSetTyped(mock, "workspace_backend", Size{Value: 256, Unit: "M"})
		if err != nil {
			t.Fatalf("ParamSetTyped() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		expectedCmd := "param.set workspace_backend 256M"
		if len(history) != 1 || history[0] != expectedCmd {
			t.Errorf("Expected command %q, got %v", expectedCmd, history)
		}
	})

	t.Run("ParamSetTyped_Error", func(t *testing.T) {
		mock.ClearCallHistory()
		mock.SetResponse("param.set invalid_param bad_value", VarnishResponse{
			statusCode: ClisParam,
			payload:    "Unknown parameter 'invalid_param'",
		})

		_, err := ParamSetTyped(mock, "invalid_param", "bad_value")
		if err == nil {
			t.Fatal("Expected error for invalid parameter")
		}

		if !strings.Contains(err.Error(), "failed with status") {
			t.Errorf("Expected error to contain 'failed with status', got: %v", err)
		}
	})
}

func TestCommands_TLSCommands(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	tests := []struct {
		name        string
		commandFunc func() (VarnishResponse, error)
		expectedCmd string
	}{
		{
			name:        "TLSCertList",
			commandFunc: mock.TLSCertList,
			expectedCmd: "tls.cert.list",
		},
		{
			name:        "TLSCertCommit",
			commandFunc: mock.TLSCertCommit,
			expectedCmd: "tls.cert.commit",
		},
		{
			name:        "TLSCertRollback",
			commandFunc: mock.TLSCertRollback,
			expectedCmd: "tls.cert.rollback",
		},
		{
			name:        "TLSCertReload",
			commandFunc: mock.TLSCertReload,
			expectedCmd: "tls.cert.reload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock.ClearCallHistory()
			mock.SetResponse(tt.expectedCmd, VarnishResponse{
				statusCode: ClisOk,
				payload:    "OK",
			})

			resp, err := tt.commandFunc()
			if err != nil {
				t.Fatalf("Command() error = %v", err)
			}

			if resp.statusCode != ClisOk {
				t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
			}

			history := mock.GetCallHistory()
			if len(history) != 1 || history[0] != tt.expectedCmd {
				t.Errorf("Expected command %q, got %v", tt.expectedCmd, history)
			}
		})
	}

	t.Run("TLSCertLoad", func(t *testing.T) {
		mock.ClearCallHistory()
		expectedCmd := "tls.cert.load example /path/to/combined.pem"
		mock.SetResponse(expectedCmd, VarnishResponse{
			statusCode: ClisOk,
			payload:    "Certificate loaded",
		})

		resp, err := mock.TLSCertLoad("example", "/path/to/combined.pem", "")
		if err != nil {
			t.Fatalf("TLSCertLoad() error = %v", err)
		}

		if resp.statusCode != ClisOk {
			t.Errorf("statusCode = %v, want %v", resp.statusCode, ClisOk)
		}

		history := mock.GetCallHistory()
		if len(history) != 1 || history[0] != expectedCmd {
			t.Errorf("Expected command %q, got %v", expectedCmd, history)
		}
	})
}

func TestCommands_StructuredMethods(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	mock := NewMock(2000, "secret", logger)

	t.Run("VCLListStructured", func(t *testing.T) {
		result, err := mock.VCLListStructured()
		if err != nil {
			t.Fatalf("VCLListStructured() error = %v", err)
		}

		if len(result.Entries) == 0 {
			t.Error("VCLListStructured() returned no entries")
		}

		// Check first entry
		if len(result.Entries) > 0 {
			entry := result.Entries[0]
			if entry.Name == "" {
				t.Error("First VCL entry has empty name")
			}
			if entry.Status == "" {
				t.Error("First VCL entry has empty status")
			}
		}
	})

	t.Run("TLSCertListStructured", func(t *testing.T) {
		// Set up initial TLS state
		mock.SetTLSState([]TLSCertEntry{
			{
				Frontend:      "main",
				State:         "active",
				Hostname:      "example.com",
				CertificateID: "cert-001",
				Expiration:    time.Now().Add(90 * 24 * time.Hour),
				OCSPStapling:  true,
			},
		})

		result, err := mock.TLSCertListStructured()
		if err != nil {
			t.Fatalf("TLSCertListStructured() error = %v", err)
		}

		if len(result.Entries) == 0 {
			t.Error("TLSCertListStructured() returned no entries")
		}

		// Check first entry
		if len(result.Entries) > 0 {
			entry := result.Entries[0]
			if entry.Frontend == "" {
				t.Error("First TLS cert entry has empty frontend")
			}
			if entry.Hostname == "" {
				t.Error("First TLS cert entry has empty hostname")
			}
			if entry.CertificateID == "" {
				t.Error("First TLS cert entry has empty certificate ID")
			}
		}
	})

	t.Run("VCLListStructured error handling", func(t *testing.T) {
		mock.SetResponse("vcl.list", VarnishResponse{
			statusCode: ClisUnknown,
			payload:    "Command failed",
		})

		_, err := mock.VCLListStructured()
		if err == nil {
			t.Error("VCLListStructured() should return error when vcl.list fails")
		}
	})

	t.Run("TLSCertListStructured error handling", func(t *testing.T) {
		mock.SetResponse("tls.cert.list", VarnishResponse{
			statusCode: ClisCant,
			payload:    "TLS not available",
		})

		_, err := mock.TLSCertListStructured()
		if err == nil {
			t.Error("TLSCertListStructured() should return error when tls.cert.list fails")
		}
	})
}
