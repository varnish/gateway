package k8sutil

import (
	"context"
	"net/url"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	clientmetrics "k8s.io/client-go/tools/metrics"
)

// Canonical bucket layouts copied from kube-controller-manager so the
// metrics line up with community client-go dashboards.
var (
	latencyBuckets     = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 15, 30, 60}
	rateLimiterBuckets = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60}
	sizeBuckets        = prometheus.ExponentialBuckets(64, 4, 8)
)

// clientmetrics.Register is sync.Once-gated, and prometheus.MustRegister
// panics on duplicates — guard the whole setup so a second call is a no-op
// rather than a partial/broken registration.
var registerClientGoOnce sync.Once

// RegisterClientGoMetrics installs the full set of client-go REST metrics
// (requests_total, request_duration_seconds, rate_limiter_duration_seconds,
// request/response_size_bytes, request_retries_total) onto reg. Use this
// from binaries that don't pull in controller-runtime.
func RegisterClientGoMetrics(reg prometheus.Registerer) {
	registerClientGoOnce.Do(func() {
		requestResult, requestLatency, rateLimiter, requestSize, responseSize, requestRetry := newClientGoCollectors()
		reg.MustRegister(requestResult, requestLatency, rateLimiter, requestSize, responseSize, requestRetry)
		clientmetrics.Register(clientmetrics.RegisterOpts{
			RequestResult:      &resultAdapter{metric: requestResult},
			RequestLatency:     &latencyAdapter{metric: requestLatency},
			RateLimiterLatency: &latencyAdapter{metric: rateLimiter},
			RequestSize:        &sizeAdapter{metric: requestSize},
			ResponseSize:       &sizeAdapter{metric: responseSize},
			RequestRetry:       &retryAdapter{metric: requestRetry},
		})
	})
}

// RegisterClientGoDurationMetric installs rest_client_request_duration_seconds
// onto reg, bypassing client-go's sync.Once-gated Register (which
// controller-runtime's init() has already consumed to wire only requests_total).
// Direct assignment to the exported global is the same workaround used by
// kube-controller-manager.
func RegisterClientGoDurationMetric(reg prometheus.Registerer) {
	h := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rest_client_request_duration_seconds",
			Help:    "Request latency in seconds. Broken down by verb and host.",
			Buckets: latencyBuckets,
		},
		[]string{"verb", "host"},
	)
	reg.MustRegister(h)
	clientmetrics.RequestLatency = &latencyAdapter{metric: h}
}

func newClientGoCollectors() (
	requestResult *prometheus.CounterVec,
	requestLatency *prometheus.HistogramVec,
	rateLimiterLatency *prometheus.HistogramVec,
	requestSize *prometheus.HistogramVec,
	responseSize *prometheus.HistogramVec,
	requestRetry *prometheus.CounterVec,
) {
	requestResult = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rest_client_requests_total",
			Help: "Number of HTTP requests, partitioned by status code, method, and host.",
		},
		[]string{"code", "method", "host"},
	)
	requestLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rest_client_request_duration_seconds",
			Help:    "Request latency in seconds. Broken down by verb and host.",
			Buckets: latencyBuckets,
		},
		[]string{"verb", "host"},
	)
	rateLimiterLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rest_client_rate_limiter_duration_seconds",
			Help:    "Client-side rate limiter latency in seconds. Broken down by verb and host.",
			Buckets: rateLimiterBuckets,
		},
		[]string{"verb", "host"},
	)
	requestSize = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rest_client_request_size_bytes",
			Help:    "Request size in bytes. Broken down by verb and host.",
			Buckets: sizeBuckets,
		},
		[]string{"verb", "host"},
	)
	responseSize = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rest_client_response_size_bytes",
			Help:    "Response size in bytes. Broken down by verb and host.",
			Buckets: sizeBuckets,
		},
		[]string{"verb", "host"},
	)
	requestRetry = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rest_client_request_retries_total",
			Help: "Number of request retries, partitioned by status code, verb, and host.",
		},
		[]string{"code", "verb", "host"},
	)
	return
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
