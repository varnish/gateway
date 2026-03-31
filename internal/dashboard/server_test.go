package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, *EventBus, *StateTracker) {
	t.Helper()
	bus := NewEventBus(256)
	state := NewStateTracker(bus, "v1.0.0-test")
	srv := NewServer(":0", state, bus, nil)
	return srv, bus, state
}

func TestHandleDashboard(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	srv.handleDashboard(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("expected text/html content type, got %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Varnish Gateway") {
		t.Error("expected dashboard HTML to contain 'Varnish Gateway'")
	}
}

func TestHandleState_EmptyState(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	w := httptest.NewRecorder()

	srv.handleState(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected application/json, got %q", ct)
	}

	var snap Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("failed to decode snapshot: %v", err)
	}
	if snap.Version != "v1.0.0-test" {
		t.Errorf("expected version 'v1.0.0-test', got %q", snap.Version)
	}
	if snap.Ready {
		t.Error("expected ready=false")
	}
}

func TestHandleState_WithData(t *testing.T) {
	srv, _, state := newTestServer(t)

	state.SetReady()
	state.UpdateServices(map[string]ServiceState{
		"default/api": {Name: "api", Namespace: "default", Backends: []BackendState{
			{Address: "10.0.0.1", Port: 8080},
		}},
	})
	state.UpdateVHosts(map[string]VHostState{
		"api.example.com": {Hostname: "api.example.com", Routes: 2, Services: []string{"default/api"}},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	w := httptest.NewRecorder()
	srv.handleState(w, req)

	var snap Snapshot
	if err := json.NewDecoder(w.Result().Body).Decode(&snap); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if !snap.Ready {
		t.Error("expected ready=true")
	}
	if len(snap.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(snap.Services))
	}
	if len(snap.VHosts) != 1 {
		t.Fatalf("expected 1 vhost, got %d", len(snap.VHosts))
	}
}

func TestHandleState_NoCacheHeader(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	w := httptest.NewRecorder()
	srv.handleState(w, req)

	if cc := w.Result().Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("expected Cache-Control: no-cache, got %q", cc)
	}
}

func TestHandleSSE_Headers(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	// Run SSE handler in goroutine since it blocks
	done := make(chan struct{})
	go func() {
		srv.handleSSE(w, req)
		close(done)
	}()

	// Give handler time to set headers and start
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	resp := w.Result()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("expected Cache-Control: no-cache, got %q", cc)
	}
}

func TestHandleSSE_ReceivesEvents(t *testing.T) {
	srv, bus, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.handleSSE(w, req)
		close(done)
	}()

	// Wait for handler to subscribe
	time.Sleep(50 * time.Millisecond)

	// Publish an event
	bus.Publish(Event{Type: EventVCLReload, Message: "test reload"})

	// Wait for event to be written
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := w.Body.String()
	if !strings.Contains(body, "event: vcl_reload") {
		t.Errorf("expected SSE event line, got:\n%s", body)
	}
	if !strings.Contains(body, "test reload") {
		t.Errorf("expected event data with message, got:\n%s", body)
	}
}

func TestHandleSSE_Heartbeat(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.handleSSE(w, req)
		close(done)
	}()

	// Wait for at least one heartbeat (1 second interval)
	time.Sleep(1200 * time.Millisecond)
	cancel()
	<-done

	body := w.Body.String()
	if !strings.Contains(body, "event: heartbeat") {
		t.Errorf("expected heartbeat event, got:\n%s", body)
	}
}

func TestServer_Run_Shutdown(t *testing.T) {
	bus := NewEventBus(10)
	state := NewStateTracker(bus, "v1.0.0")
	srv := NewServer("127.0.0.1:0", state, bus, nil)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Run(ctx)
	}()

	// Give server time to start
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("expected nil error on clean shutdown, got: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not shut down in time")
	}
}
