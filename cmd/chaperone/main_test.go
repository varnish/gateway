package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/varnish/gateway/internal/dashboard"
)

// newTestState returns a StateTracker marked ready (not draining).
func newTestState() *dashboard.StateTracker {
	bus := dashboard.NewEventBus(16)
	st := dashboard.NewStateTracker(bus, "test")
	st.SetReady()
	return st
}

func TestDrainHandler_ValidToken(t *testing.T) {
	st := newTestState()
	h := makeDrainHandler(st, "s3cr3t")

	req := httptest.NewRequest(http.MethodGet, "/drain", nil)
	req.Header.Set("X-Drain-Token", "s3cr3t")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !st.IsDraining() {
		t.Fatal("expected draining state after valid drain request")
	}
}

func TestDrainHandler_MissingToken(t *testing.T) {
	st := newTestState()
	h := makeDrainHandler(st, "s3cr3t")

	req := httptest.NewRequest(http.MethodGet, "/drain", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if st.IsDraining() {
		t.Fatal("gateway drained despite missing token (cross-pod DoS not closed)")
	}
}

func TestDrainHandler_WrongToken(t *testing.T) {
	st := newTestState()
	h := makeDrainHandler(st, "s3cr3t")

	req := httptest.NewRequest(http.MethodGet, "/drain", nil)
	req.Header.Set("X-Drain-Token", "guessed")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if st.IsDraining() {
		t.Fatal("gateway drained despite wrong token")
	}
}

// When no token is configured the endpoint must refuse rather than drain on
// any request.
func TestDrainHandler_EmptyTokenDisabled(t *testing.T) {
	st := newTestState()
	h := makeDrainHandler(st, "")

	req := httptest.NewRequest(http.MethodGet, "/drain", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if st.IsDraining() {
		t.Fatal("gateway drained with no token configured")
	}
}
