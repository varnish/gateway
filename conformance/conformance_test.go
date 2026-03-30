//go:build conformance

package conformance_test

import (
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/gateway-api/conformance"
	confv1 "sigs.k8s.io/gateway-api/conformance/apis/v1"
	"sigs.k8s.io/gateway-api/pkg/features"
)

func TestConformance(t *testing.T) {
	opts := conformance.DefaultOptions(t)

	opts.GatewayClassName = "varnish"
	opts.CleanupBaseResources = true
	opts.AllowCRDsMismatch = true

	opts.SupportedFeatures = sets.New[features.FeatureName](
		// Core
		features.SupportGateway,
		features.SupportHTTPRoute,
		// Extended
		features.SupportHTTPRouteQueryParamMatching,
		features.SupportHTTPRouteMethodMatching,
		features.SupportHTTPRouteResponseHeaderModification,
		features.SupportHTTPRoutePortRedirect,
		features.SupportHTTPRouteSchemeRedirect,
		features.SupportHTTPRoutePathRedirect,
		features.SupportHTTPRouteHostRewrite,
		features.SupportHTTPRoutePathRewrite,
		features.SupportGatewayHTTPListenerIsolation,
		// BackendTLSPolicy: Varnish 9.0 does not expose a backend API field for
		// specifying the CA certificate used to verify backend TLS certs. Backend
		// TLS works on a best-effort basis (system CA store / SSL_CERT_FILE) but
		// cannot pass conformance until the Varnish C API adds CA cert support.
		// features.SupportBackendTLSPolicy,
	)

	version := os.Getenv("GATEWAY_VERSION")
	if version == "" {
		version = "dev"
	}

	opts.Implementation = confv1.Implementation{
		Organization: "varnish",
		Project:      "gateway",
		URL:          "https://github.com/varnish/gateway",
		Version:      version,
		Contact:      []string{"@perbu"},
	}

	if reportPath := os.Getenv("CONFORMANCE_REPORT_PATH"); reportPath != "" {
		opts.ReportOutputPath = reportPath
	}

	conformance.RunConformanceWithOptions(t, opts)
}
