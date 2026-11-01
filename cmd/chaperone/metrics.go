package main

import (
	"context"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/varnish/gateway/internal/dashboard"
)

type chaperoneMetrics struct {
	ghostReloads      prometheus.Counter
	ghostReloadErrors prometheus.Counter
	vclReloads        prometheus.Counter
	vclReloadErrors   prometheus.Counter
	tlsReloads        prometheus.Counter
	tlsReloadErrors   prometheus.Counter
	endpointChanges   prometheus.Counter
	ready             prometheus.Gauge
	draining          prometheus.Gauge

	routeVCLCompiles       *prometheus.CounterVec
	routeVCLCompileSeconds prometheus.Histogram
	routeVCLSlots          *prometheus.GaugeVec
	routeVCLMasterReloads  *prometheus.CounterVec
}

func newChaperoneMetrics(reg prometheus.Registerer) *chaperoneMetrics {
	m := &chaperoneMetrics{
		ghostReloads: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "chaperone_ghost_reloads_total",
			Help: "Total number of ghost.json reloads.",
		}),
		ghostReloadErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "chaperone_ghost_reload_errors_total",
			Help: "Total number of failed ghost.json reloads.",
		}),
		vclReloads: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "chaperone_vcl_reloads_total",
			Help: "Total number of VCL hot-reloads.",
		}),
		vclReloadErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "chaperone_vcl_reload_errors_total",
			Help: "Total number of failed VCL hot-reloads.",
		}),
		tlsReloads: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "chaperone_tls_reloads_total",
			Help: "Total number of TLS certificate reloads.",
		}),
		tlsReloadErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "chaperone_tls_reload_errors_total",
			Help: "Total number of failed TLS certificate reloads.",
		}),
		endpointChanges: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "chaperone_endpoint_changes_total",
			Help: "Total number of endpoint change events observed.",
		}),
		ready: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "chaperone_ready",
			Help: "Whether the chaperone is ready (1) or not (0).",
		}),
		draining: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "chaperone_draining",
			Help: "Whether the chaperone is draining (1) or not (0).",
		}),
		routeVCLCompiles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chaperone_route_vcl_compiles_total",
			Help: "Route VCL loads by result: ok (labelled), compile_error (VCC rejected it), error (any other refusal or a failed vcl.label).",
		}, []string{"result"}),
		routeVCLCompileSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "chaperone_route_vcl_compile_duration_seconds",
			Help:    "Duration of route VCL vcl.inline calls (compile time, includes failed compiles).",
			Buckets: []float64{0.05, 0.1, 0.2, 0.3, 0.5, 0.75, 1, 2, 5},
		}),
		routeVCLSlots: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "chaperone_route_vcl_slots",
			Help: "Route VCL pool slots by state: programmed, failed (compile failed or waiting; previous VCL or placeholder serves) and placeholder (empty).",
		}, []string{"state"}),
		routeVCLMasterReloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "chaperone_route_vcl_master_reloads_total",
			Help: "Master VCL loads (the VCL dispatching to route VCLs) by result. Also counted in chaperone_vcl_reloads_total.",
		}, []string{"result"}),
	}
	// Series exist from the start so rate() and absent() behave.
	for _, r := range []string{dashboard.RouteVCLOK, dashboard.RouteVCLCompileError, dashboard.RouteVCLOtherError} {
		m.routeVCLCompiles.WithLabelValues(r)
	}
	for _, r := range []string{dashboard.RouteVCLOK, dashboard.RouteVCLOtherError} {
		m.routeVCLMasterReloads.WithLabelValues(r)
	}
	for _, st := range []string{"programmed", "failed", "placeholder"} {
		m.routeVCLSlots.WithLabelValues(st)
	}

	reg.MustRegister(
		m.ghostReloads,
		m.ghostReloadErrors,
		m.vclReloads,
		m.vclReloadErrors,
		m.tlsReloads,
		m.tlsReloadErrors,
		m.endpointChanges,
		m.ready,
		m.draining,
		m.routeVCLCompiles,
		m.routeVCLCompileSeconds,
		m.routeVCLSlots,
		m.routeVCLMasterReloads,
	)

	return m
}

// runMetricsUpdater subscribes to dashboard events and increments Prometheus counters.
func runMetricsUpdater(ctx context.Context, bus *dashboard.EventBus, m *chaperoneMetrics) error {
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	// The slot gauges only move when a pass changes something; pick up a
	// snapshot published before we subscribed.
	for _, ev := range bus.Recent() {
		if ev.Type == dashboard.EventRouteVCLSlots {
			if st, ok := ev.RouteVCLState(); ok {
				m.setRouteVCLSlots(st)
			}
		}
	}

	for {
		select {
		case ev := <-ch:
			switch ev.Type {
			case dashboard.EventGhostReload:
				m.ghostReloads.Inc()
			case dashboard.EventGhostReloadFail:
				m.ghostReloadErrors.Inc()
			// Master VCL. Route VCL loads have their own events and do not
			// count here.
			case dashboard.EventVCLReload:
				m.vclReloads.Inc()
				m.routeVCLMasterReloads.WithLabelValues(dashboard.RouteVCLOK).Inc()
			case dashboard.EventVCLReloadFail:
				m.vclReloadErrors.Inc()
				m.routeVCLMasterReloads.WithLabelValues(dashboard.RouteVCLOtherError).Inc()
			case dashboard.EventRouteVCLReload, dashboard.EventRouteVCLReloadFail:
				m.observeRouteVCLLoad(ev)
			case dashboard.EventRouteVCLSlots:
				if st, ok := ev.RouteVCLState(); ok {
					m.setRouteVCLSlots(st)
				}
			case dashboard.EventTLSReload:
				m.tlsReloads.Inc()
			case dashboard.EventTLSReloadFail:
				m.tlsReloadErrors.Inc()
			case dashboard.EventEndpointsChanged:
				m.endpointChanges.Inc()
			case dashboard.EventReady:
				m.ready.Set(1)
			case dashboard.EventDraining:
				m.draining.Set(1)
				m.ready.Set(0)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *chaperoneMetrics) setRouteVCLSlots(st dashboard.RouteVCLState) {
	programmed, failed, placeholder := st.Counts()
	m.routeVCLSlots.WithLabelValues("programmed").Set(float64(programmed))
	m.routeVCLSlots.WithLabelValues("failed").Set(float64(failed))
	m.routeVCLSlots.WithLabelValues("placeholder").Set(float64(placeholder))
}

// observeRouteVCLLoad counts one route VCL load and, when a compile ran,
// observes its duration.
func (m *chaperoneMetrics) observeRouteVCLLoad(ev dashboard.Event) {
	result := ev.Data["result"]
	if result == "" {
		result = dashboard.RouteVCLOtherError
	}
	m.routeVCLCompiles.WithLabelValues(result).Inc()
	if v, err := strconv.ParseFloat(ev.Data["compile_seconds"], 64); err == nil {
		m.routeVCLCompileSeconds.Observe(v)
	}
}
