package main

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
	registerClientGoMetrics(reg)

	// Second call must be a no-op (sync.Once) — would otherwise panic on
	// duplicate collector registration against the same registry.
	registerClientGoMetrics(reg)

	// Drive the client-go global adapters so histograms/counters emit
	// at least one sample and become visible in Gather output.
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

	want := map[string]bool{
		"rest_client_requests_total":                false,
		"rest_client_request_duration_seconds":      false,
		"rest_client_rate_limiter_duration_seconds": false,
		"rest_client_request_size_bytes":            false,
		"rest_client_response_size_bytes":           false,
		"rest_client_request_retries_total":         false,
	}
	for _, f := range families {
		if _, ok := want[f.GetName()]; ok {
			want[f.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric %q not registered", name)
		}
	}
}
