package reload

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTriggerReloadSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ReloadPath {
			t.Errorf("expected path %s, got %s", ReloadPath, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Response{
			Status:  "ok",
			Message: "Configuration reloaded successfully",
		})
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
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Response{
			Status:  "error",
			Message: "Invalid configuration: missing required field",
		})
	}))
	defer server.Close()

	addr := server.URL[7:]
	client := NewClient(addr)
	ctx := context.Background()

	err := client.TriggerReload(ctx)
	if err == nil {
		t.Error("expected error, got nil")
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

func TestTriggerReloadInvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not valid json"))
	}))
	defer server.Close()

	addr := server.URL[7:]
	client := NewClient(addr)
	ctx := context.Background()

	err := client.TriggerReload(ctx)
	if err == nil {
		t.Error("expected error, got nil")
	}
}

func TestTriggerReloadContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response
		time.Sleep(1 * time.Second)
		json.NewEncoder(w).Encode(Response{Status: "ok"})
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
		json.NewEncoder(w).Encode(Response{Status: "ok"})
	}))
	defer server.Close()

	addr := server.URL[7:]
	ctx := context.Background()

	err := TriggerReloadSimple(ctx, addr)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
