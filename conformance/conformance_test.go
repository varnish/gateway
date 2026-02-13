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
