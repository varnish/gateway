package reload

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTriggerReloadSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ReloadPath {
			t.Errorf("expected path %s, got %s", ReloadPath, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Extract host:port from server URL (strip http://)
	addr := server.URL[7:] // remove "http://"

	client := NewClient(addr)
	ctx := context.Background()

	err := client.TriggerReload(ctx)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestTriggerReloadFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ghost-error", "Invalid configuration: missing required field")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	addr := server.URL[7:]
	client := NewClient(addr)
	ctx := context.Background()

	err := client.TriggerReload(ctx)
	if err == nil {
		t.Error("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Invalid configuration") {
		t.Errorf("expected error message to contain config error, got: %v", err)
	}
}

func TestTriggerReloadFailureNoHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	addr := server.URL[7:]
	client := NewClient(addr)
	ctx := context.Background()

	err := client.TriggerReload(ctx)
	if err == nil {
		t.Error("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("expected error to mention HTTP 500, got: %v", err)
	}
}

func TestTriggerReloadConnectionError(t *testing.T) {
	// Use an address that won't be listening
	client := NewClient("localhost:59999")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := client.TriggerReload(ctx)
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestTriggerReloadContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response
		time.Sleep(1 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	addr := server.URL[7:]
	client := NewClient(addr)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := client.TriggerReload(ctx)
	if err == nil {
		t.Error("expected error due to context timeout")
	}
}

func TestTriggerReloadSimple(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	addr := server.URL[7:]
	ctx := context.Background()

	err := TriggerReloadSimple(ctx, addr)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
