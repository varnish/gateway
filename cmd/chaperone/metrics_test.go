package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/varnish/gateway/internal/dashboard"
)

func TestNewChaperoneMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newChaperoneMetrics(reg)

	// All counters should start at zero.
	if v := testutil.ToFloat64(m.ghostReloads); v != 0 {
		t.Errorf("ghostReloads = %v, want 0", v)
	}
	if v := testutil.ToFloat64(m.ready); v != 0 {
		t.Errorf("ready = %v, want 0", v)
	}

	// Verify all metrics are gathered without error.
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	wantNames := map[string]bool{
		"chaperone_ghost_reloads_total":       true,
		"chaperone_ghost_reload_errors_total": true,
		"chaperone_vcl_reloads_total":         true,
		"chaperone_vcl_reload_errors_total":   true,
		"chaperone_tls_reloads_total":         true,
		"chaperone_tls_reload_errors_total":   true,
		"chaperone_endpoint_changes_total":    true,
		"chaperone_ready":                     true,
		"chaperone_draining":                  true,
	}

	for _, f := range families {
		delete(wantNames, f.GetName())
	}
	for name := range wantNames {
		t.Errorf("metric %q not found in gathered families", name)
	}
}

func TestRunMetricsUpdater(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newChaperoneMetrics(reg)
	bus := dashboard.NewEventBus(64)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runMetricsUpdater(ctx, bus, m)
	}()

	// Give the goroutine time to subscribe before publishing.
	time.Sleep(20 * time.Millisecond)

	// Publish events and verify counters.
	publish := func(et dashboard.EventType) {
		bus.Publish(dashboard.Event{Type: et, Time: time.Now()})
		// Give the goroutine time to process.
		time.Sleep(10 * time.Millisecond)
	}

	publish(dashboard.EventGhostReload)
	publish(dashboard.EventGhostReload)
	publish(dashboard.EventGhostReloadFail)
	publish(dashboard.EventVCLReload)
	publish(dashboard.EventVCLReloadFail)
	publish(dashboard.EventTLSReload)
	publish(dashboard.EventTLSReloadFail)
	publish(dashboard.EventEndpointsChanged)
	publish(dashboard.EventReady)
	publish(dashboard.EventDraining)

	if v := testutil.ToFloat64(m.ghostReloads); v != 2 {
		t.Errorf("ghostReloads = %v, want 2", v)
	}
	if v := testutil.ToFloat64(m.ghostReloadErrors); v != 1 {
		t.Errorf("ghostReloadErrors = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.vclReloads); v != 1 {
		t.Errorf("vclReloads = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.vclReloadErrors); v != 1 {
		t.Errorf("vclReloadErrors = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.tlsReloads); v != 1 {
		t.Errorf("tlsReloads = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.tlsReloadErrors); v != 1 {
		t.Errorf("tlsReloadErrors = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.endpointChanges); v != 1 {
		t.Errorf("endpointChanges = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.ready); v != 0 {
		t.Errorf("ready = %v, want 0 (draining overrides)", v)
	}
	if v := testutil.ToFloat64(m.draining); v != 1 {
		t.Errorf("draining = %v, want 1", v)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("runMetricsUpdater: %v", err)
	}
}

func TestRunMetricsUpdater_RouteVCL(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newChaperoneMetrics(reg)
	bus := dashboard.NewEventBus(64)

	// A snapshot published before the updater starts is picked up.
	dashboard.PublishRouteVCLSlots(bus, dashboard.RouteVCLState{PoolSize: 8, Slots: []dashboard.RouteVCLSlot{
		{Slot: 0, State: dashboard.RouteVCLProgrammed},
		{Slot: 1, State: dashboard.RouteVCLProgrammed},
		{Slot: 2, State: dashboard.RouteVCLFailed},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runMetricsUpdater(ctx, bus, m) }()
	time.Sleep(20 * time.Millisecond)

	// Master loads stay on the old counters; route loads do not touch them.
	bus.Publish(dashboard.Event{Type: dashboard.EventVCLReload})
	bus.Publish(dashboard.Event{Type: dashboard.EventVCLReloadFail})
	dashboard.PublishRouteVCLReload(bus, 0, "route_0_abc", 250*time.Millisecond)
	dashboard.PublishRouteVCLReload(bus, 1, "route_1_abc", 0) // label retry, no compile
	dashboard.PublishRouteVCLReloadFail(bus, 2, errors.New("boom"), dashboard.RouteVCLCompileError, 80*time.Millisecond)
	dashboard.PublishRouteVCLReloadFail(bus, 3, errors.New("label"), dashboard.RouteVCLOtherError, 0)
	time.Sleep(50 * time.Millisecond)

	check := func(name string, got, want float64) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("vclReloads", testutil.ToFloat64(m.vclReloads), 1)
	check("vclReloadErrors", testutil.ToFloat64(m.vclReloadErrors), 1)
	check("master ok", testutil.ToFloat64(m.routeVCLMasterReloads.WithLabelValues("ok")), 1)
	check("master error", testutil.ToFloat64(m.routeVCLMasterReloads.WithLabelValues("error")), 1)
	check("compiles ok", testutil.ToFloat64(m.routeVCLCompiles.WithLabelValues("ok")), 2)
	check("compiles compile_error", testutil.ToFloat64(m.routeVCLCompiles.WithLabelValues("compile_error")), 1)
	check("compiles error", testutil.ToFloat64(m.routeVCLCompiles.WithLabelValues("error")), 1)
	check("slots programmed", testutil.ToFloat64(m.routeVCLSlots.WithLabelValues("programmed")), 2)
	check("slots failed", testutil.ToFloat64(m.routeVCLSlots.WithLabelValues("failed")), 1)
	check("slots placeholder", testutil.ToFloat64(m.routeVCLSlots.WithLabelValues("placeholder")), 5)

	// Two compiles ran (250 ms and 80 ms): the histogram has two samples.
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == "chaperone_route_vcl_compile_duration_seconds" {
			if n := f.GetMetric()[0].GetHistogram().GetSampleCount(); n != 2 {
				t.Errorf("compile duration samples = %d, want 2", n)
			}
			return
		}
	}
	t.Error("compile duration histogram not found")
}
