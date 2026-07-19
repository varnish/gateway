//go:build conformance

package conformance_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/gateway-api/conformance"
	"sigs.k8s.io/gateway-api/conformance/utils/suite"
	"sigs.k8s.io/gateway-api/pkg/features"
)

func TestConformance(t *testing.T) {
	opts := conformance.DefaultOptions(t)

	opts.CleanupBaseResources = true

	opts.ConformanceProfiles = sets.New[suite.ConformanceProfileName](
		suite.GatewayHTTPConformanceProfileName,
	)

	opts.SupportedFeatures = sets.New[features.FeatureName](
		// Core
		features.SupportGateway,
		features.SupportReferenceGrant,
		features.SupportHTTPRoute,
		// Extended
		features.SupportHTTPRouteQueryParamMatching,
		features.SupportHTTPRouteMethodMatching,
		features.SupportHTTPRouteResponseHeaderModification,
		features.SupportHTTPRoutePortRedirect,
		features.SupportHTTPRouteSchemeRedirect,
		features.SupportHTTPRoutePathRedirect,
		features.SupportHTTPRoute303RedirectStatusCode,
		features.SupportHTTPRoute307RedirectStatusCode,
		features.SupportHTTPRoute308RedirectStatusCode,
		features.SupportHTTPRouteHostRewrite,
		features.SupportHTTPRoutePathRewrite,
		features.SupportGatewayHTTPListenerIsolation,
		features.SupportHTTPRouteParentRefPort,
		// BackendTLSPolicy: Varnish 9.0 does not expose a backend API field for
		// specifying the CA certificate used to verify backend TLS certs. Backend
		// TLS works on a best-effort basis (system CA store / SSL_CERT_FILE) but
		// cannot pass conformance until the Varnish C API adds CA cert support.
		// features.SupportBackendTLSPolicy,
	)

	conformance.RunConformanceWithOptions(t, opts)
}
