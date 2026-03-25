// Package k8sutil provides shared Kubernetes client utilities.
package k8sutil

import (
	"log/slog"
	"net/http"
	"time"

	"k8s.io/client-go/rest"
)

const defaultWarnThreshold = 5 * time.Second

// WrapTransportForSlowRequests adds a round-tripper to the rest config that
// logs a warning when any API server request exceeds the threshold. This
// provides early warning of API server latency issues.
func WrapTransportForSlowRequests(cfg *rest.Config, logger *slog.Logger) {
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return &slowRequestLogger{next: rt, logger: logger, warnThreshold: defaultWarnThreshold}
	})
}

type slowRequestLogger struct {
	next          http.RoundTripper
	logger        *slog.Logger
	warnThreshold time.Duration
}

func (s *slowRequestLogger) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := s.next.RoundTrip(req)
	elapsed := time.Since(start)
	if elapsed >= s.warnThreshold {
		s.logger.Warn("slow API server request",
			"method", req.Method,
			"url", req.URL.String(),
			"duration", elapsed.Round(time.Millisecond),
		)
	}
	return resp, err
}
