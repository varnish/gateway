package main

import (
	"context"

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
	)

	return m
}

// runMetricsUpdater subscribes to dashboard events and increments Prometheus counters.
func runMetricsUpdater(ctx context.Context, bus *dashboard.EventBus, m *chaperoneMetrics) error {
	ch := bus.Subscribe()
	defer bus.Unsubscribe(ch)

	for {
		select {
		case ev := <-ch:
			switch ev.Type {
			case dashboard.EventGhostReload:
				m.ghostReloads.Inc()
			case dashboard.EventGhostReloadFail:
				m.ghostReloadErrors.Inc()
			case dashboard.EventVCLReload:
				m.vclReloads.Inc()
			case dashboard.EventVCLReloadFail:
				m.vclReloadErrors.Inc()
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
