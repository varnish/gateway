package main

import (
	"context"
	"net/url"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	clientmetrics "k8s.io/client-go/tools/metrics"
)

// Metric names follow kube-controller-manager conventions so existing
// client-go dashboards apply unchanged.
//
// Guarded by sync.Once because client-go's metrics.Register is itself
// sync.Once — a second call would silently drop our adapters while still
// panicking on duplicate collector registration.
var registerClientGoOnce sync.Once

func registerClientGoMetrics(reg prometheus.Registerer) {
	registerClientGoOnce.Do(func() {
		requestResult := prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "rest_client_requests_total",
				Help: "Number of HTTP requests, partitioned by status code, method, and host.",
			},
			[]string{"code", "method", "host"},
		)
		requestLatency := prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "rest_client_request_duration_seconds",
				Help:    "Request latency in seconds. Broken down by verb and host.",
				Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 15, 30, 60},
			},
			[]string{"verb", "host"},
		)
		rateLimiterLatency := prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "rest_client_rate_limiter_duration_seconds",
				Help:    "Client-side rate limiter latency in seconds. Broken down by verb and host.",
				Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60},
			},
			[]string{"verb", "host"},
		)
		requestSize := prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "rest_client_request_size_bytes",
				Help:    "Request size in bytes. Broken down by verb and host.",
				Buckets: prometheus.ExponentialBuckets(64, 4, 8),
			},
			[]string{"verb", "host"},
		)
		responseSize := prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "rest_client_response_size_bytes",
				Help:    "Response size in bytes. Broken down by verb and host.",
				Buckets: prometheus.ExponentialBuckets(64, 4, 8),
			},
			[]string{"verb", "host"},
		)
		requestRetry := prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "rest_client_request_retries_total",
				Help: "Number of request retries, partitioned by status code, verb, and host.",
			},
			[]string{"code", "verb", "host"},
		)

		reg.MustRegister(
			requestResult,
			requestLatency,
			rateLimiterLatency,
			requestSize,
			responseSize,
			requestRetry,
		)

		clientmetrics.Register(clientmetrics.RegisterOpts{
			RequestResult:      &resultAdapter{metric: requestResult},
			RequestLatency:     &latencyAdapter{metric: requestLatency},
			RateLimiterLatency: &latencyAdapter{metric: rateLimiterLatency},
			RequestSize:        &sizeAdapter{metric: requestSize},
			ResponseSize:       &sizeAdapter{metric: responseSize},
			RequestRetry:       &retryAdapter{metric: requestRetry},
		})
	})
}

type resultAdapter struct{ metric *prometheus.CounterVec }

func (r *resultAdapter) Increment(_ context.Context, code, method, host string) {
	r.metric.WithLabelValues(code, method, host).Inc()
}

type latencyAdapter struct{ metric *prometheus.HistogramVec }

func (l *latencyAdapter) Observe(_ context.Context, verb string, u url.URL, latency time.Duration) {
	l.metric.WithLabelValues(verb, u.Host).Observe(latency.Seconds())
}

type sizeAdapter struct{ metric *prometheus.HistogramVec }

func (s *sizeAdapter) Observe(_ context.Context, verb, host string, size float64) {
	s.metric.WithLabelValues(verb, host).Observe(size)
}

type retryAdapter struct{ metric *prometheus.CounterVec }

func (r *retryAdapter) IncrementRetry(_ context.Context, code, verb, host string) {
	r.metric.WithLabelValues(code, verb, host).Inc()
}
