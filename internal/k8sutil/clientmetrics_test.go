package k8sutil

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	clientmetrics "k8s.io/client-go/tools/metrics"
)

func TestRegisterClientGoMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterClientGoMetrics(reg)

	// Second call must be a no-op — otherwise MustRegister would panic on
	// duplicate collector registration against the same registry.
	RegisterClientGoMetrics(reg)

	u, _ := url.Parse("https://apiserver.example:443")
	clientmetrics.RequestResult.Increment(context.Background(), "200", "GET", u.Host)
	clientmetrics.RequestLatency.Observe(context.Background(), "GET", *u, 10*time.Millisecond)
	clientmetrics.RateLimiterLatency.Observe(context.Background(), "GET", *u, time.Millisecond)
	clientmetrics.RequestSize.Observe(context.Background(), "GET", u.Host, 128)
	clientmetrics.ResponseSize.Observe(context.Background(), "GET", u.Host, 256)
	clientmetrics.RequestRetry.IncrementRetry(context.Background(), "503", "GET", u.Host)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	samples := map[string]uint64{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			switch {
			case m.Histogram != nil:
				samples[f.GetName()] += m.Histogram.GetSampleCount()
			case m.Counter != nil:
				samples[f.GetName()] += uint64(m.Counter.GetValue())
			}
		}
	}

	for _, name := range []string{
		"rest_client_requests_total",
		"rest_client_request_duration_seconds",
		"rest_client_rate_limiter_duration_seconds",
		"rest_client_request_size_bytes",
		"rest_client_response_size_bytes",
		"rest_client_request_retries_total",
	} {
		if samples[name] == 0 {
			t.Errorf("metric %q registered but recorded no samples — adapter not wired", name)
		}
	}
}
