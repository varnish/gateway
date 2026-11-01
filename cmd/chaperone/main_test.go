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

func TestLoadConfig_RouteVCLThrottle(t *testing.T) {
	t.Setenv("ROUTE_VCL_COMPILE_RATE", "")
	t.Setenv("ROUTE_VCL_COMPILE_BURST", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.RouteVCLCompileRate != 4 || cfg.RouteVCLCompileBurst != 8 {
		t.Errorf("defaults = %v/%v, want 4/8", cfg.RouteVCLCompileRate, cfg.RouteVCLCompileBurst)
	}

	t.Setenv("ROUTE_VCL_COMPILE_RATE", "0.5")
	t.Setenv("ROUTE_VCL_COMPILE_BURST", "2")
	if cfg, err = loadConfig(); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.RouteVCLCompileRate != 0.5 || cfg.RouteVCLCompileBurst != 2 {
		t.Errorf("got %v/%v, want 0.5/2", cfg.RouteVCLCompileRate, cfg.RouteVCLCompileBurst)
	}

	// "0" is a valid value (disables throttling), not "unset".
	t.Setenv("ROUTE_VCL_COMPILE_RATE", "0")
	if cfg, err = loadConfig(); err != nil || cfg.RouteVCLCompileRate != 0 {
		t.Errorf("rate 0: cfg=%v err=%v", cfg, err)
	}

	for _, bad := range []struct{ k, v string }{
		{"ROUTE_VCL_COMPILE_RATE", "fast"}, {"ROUTE_VCL_COMPILE_RATE", "-1"}, {"ROUTE_VCL_COMPILE_RATE", "NaN"},
		{"ROUTE_VCL_COMPILE_BURST", "x"}, {"ROUTE_VCL_COMPILE_BURST", "-2"},
	} {
		t.Setenv(bad.k, bad.v)
		if _, err := loadConfig(); err == nil {
			t.Errorf("%s=%s: want an error", bad.k, bad.v)
		}
		t.Setenv(bad.k, "")
	}
}
