package vcl

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func ptr[T any](v T) *T {
	return &v
}

func TestSanitizeServiceName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"my-service", "my_service"},
		{"my.service", "my_service"},
		{"my-service.default", "my_service_default"},
		{"simple", "simple"},
		{"a-b.c-d", "a_b_c_d"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := SanitizeServiceName(tc.input)
			if result != tc.expected {
				t.Errorf("SanitizeServiceName(%q) = %q, expected %q", tc.input, result, tc.expected)
			}
		})
	}
}

func TestGenerate_GhostPreamble(t *testing.T) {
	result := Generate(nil, GeneratorConfig{})

	// Check VCL version header
	if !strings.Contains(result, "vcl 4.1;") {
		t.Error("expected VCL version header")
	}

	// Check ghost import
	if !strings.Contains(result, "import ghost;") {
		t.Error("expected ghost import")
	}

	// Should NOT contain old nodes/udo imports
	if strings.Contains(result, "import nodes;") {
		t.Error("should not contain nodes import")
	}
	if strings.Contains(result, "import udo;") {
		t.Error("should not contain udo import")
	}

	// Check vcl_init with ghost initialization
	if !strings.Contains(result, "sub vcl_init {") {
		t.Error("expected vcl_init subroutine")
	}
	if !strings.Contains(result, "ghost.init(") {
		t.Error("expected ghost.init call")
	}
	if !strings.Contains(result, "new router = ghost.ghost_backend()") {
		t.Error("expected ghost_backend initialization")
	}

	// Check vcl_recv for reload interception
	if !strings.Contains(result, "sub vcl_recv {") {
		t.Error("expected vcl_recv subroutine")
	}
	if !strings.Contains(result, `req.url == "/.varnish-ghost/reload"`) {
		t.Error("expected reload URL check in vcl_recv")
	}
	if !strings.Contains(result, "router.reload()") {
		t.Error("expected router.reload() call for reload requests")
	}

	// Check vcl_backend_fetch
	if !strings.Contains(result, "sub vcl_backend_fetch {") {
		t.Error("expected vcl_backend_fetch subroutine")
	}
	if !strings.Contains(result, "router.backend()") {
		t.Error("expected router.backend() call")
	}

	// Check user VCL marker
	if !strings.Contains(result, "# --- User VCL concatenated below ---") {
		t.Error("expected user VCL marker")
	}
}

func TestGenerate_GhostReloadHandler(t *testing.T) {
	result := Generate(nil, GeneratorConfig{})

	// Check vcl_recv handles both IPv4 and IPv6 localhost
	if !strings.Contains(result, `client.ip == "127.0.0.1" || client.ip == "::1"`) {
		t.Error("expected vcl_recv to check both IPv4 and IPv6 localhost")
	}

	// Check that reload is called on the router
	if !strings.Contains(result, "router.reload()") {
		t.Error("expected vcl_recv to call router.reload()")
	}

	// Should return synth(200) on success
	if !strings.Contains(result, `return (synth(200, "OK"))`) {
		t.Error("expected vcl_recv to return synth(200) on successful reload")
	}

	// Should return synth(500) on failure
	if !strings.Contains(result, `return (synth(500, "Reload failed"))`) {
		t.Error("expected vcl_recv to return synth(500) on failed reload")
	}

	// Should NOT have vcl_backend_error (reload handled in vcl_recv)
	if strings.Contains(result, "sub vcl_backend_error {") {
		t.Error("should not have vcl_backend_error (reload handled in vcl_recv)")
	}
}

func TestGenerate_CustomGhostConfigPath(t *testing.T) {
	config := GeneratorConfig{GhostConfigPath: "/custom/path/ghost.json"}
	result := Generate(nil, config)

	if !strings.Contains(result, `ghost.init("/custom/path/ghost.json")`) {
		t.Errorf("expected custom ghost config path in output, got:\n%s", result)
	}
}

func TestGenerate_DefaultGhostConfigPath(t *testing.T) {
	result := Generate(nil, GeneratorConfig{})

	if !strings.Contains(result, DefaultGhostConfigPath) {
		t.Errorf("expected default ghost config path %q in output", DefaultGhostConfigPath)
	}
}

func TestGenerate_DeterministicOutput(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route-a", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"a.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "svc-a",
										Port: ptr(gatewayv1.PortNumber(8080)),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	first := Generate(routes, GeneratorConfig{})

	for i := 0; i < 10; i++ {
		result := Generate(routes, GeneratorConfig{})
		if result != first {
			t.Errorf("output not deterministic on iteration %d", i)
		}
	}
}

func TestCollectHTTPRouteBackends(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route-1", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"api.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "api-service",
										Port: ptr(gatewayv1.PortNumber(8080)),
									},
								},
							},
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route-2", Namespace: "production"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"web.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "web-service",
										Port: ptr(gatewayv1.PortNumber(80)),
									},
									Weight: ptr(int32(50)),
								},
							},
						},
					},
				},
			},
		},
	}

	backends := CollectHTTPRouteBackends(routes, "default")

	if len(backends) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(backends))
	}

	// Should be sorted by hostname
	if backends[0].Hostname != "api.example.com" {
		t.Errorf("expected first backend hostname api.example.com, got %s", backends[0].Hostname)
	}
	if backends[0].Service != "api-service" {
		t.Errorf("expected first backend service api-service, got %s", backends[0].Service)
	}
	if backends[0].Namespace != "default" {
		t.Errorf("expected first backend namespace default, got %s", backends[0].Namespace)
	}
	if backends[0].Port != 8080 {
		t.Errorf("expected first backend port 8080, got %d", backends[0].Port)
	}

	if backends[1].Hostname != "web.example.com" {
		t.Errorf("expected second backend hostname web.example.com, got %s", backends[1].Hostname)
	}
	if backends[1].Weight != 50 {
		t.Errorf("expected second backend weight 50, got %d", backends[1].Weight)
	}
}

func TestCollectHTTPRouteBackends_DefaultPort(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route-1", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"api.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "api-service",
										// Port not specified
									},
								},
							},
						},
					},
				},
			},
		},
	}

	backends := CollectHTTPRouteBackends(routes, "default")

	if len(backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(backends))
	}
	if backends[0].Port != 80 {
		t.Errorf("expected default port 80, got %d", backends[0].Port)
	}
}

func TestCollectHTTPRouteBackends_DefaultWeight(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route-1", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"api.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "api-service",
										Port: ptr(gatewayv1.PortNumber(8080)),
										// Weight not specified
									},
								},
							},
						},
					},
				},
			},
		},
	}

	backends := CollectHTTPRouteBackends(routes, "default")

	if len(backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(backends))
	}
	if backends[0].Weight != 100 {
		t.Errorf("expected default weight 100, got %d", backends[0].Weight)
	}
}

func TestCollectHTTPRouteBackends_CrossNamespace(t *testing.T) {
	otherNS := gatewayv1.Namespace("other-namespace")
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route-1", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"api.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name:      "api-service",
										Namespace: &otherNS,
										Port:      ptr(gatewayv1.PortNumber(8080)),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	backends := CollectHTTPRouteBackends(routes, "default")

	if len(backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(backends))
	}
	if backends[0].Namespace != "other-namespace" {
		t.Errorf("expected namespace other-namespace, got %s", backends[0].Namespace)
	}
}

func TestCalculateRoutePriority(t *testing.T) {
	tests := []struct {
		name     string
		pathMatch *ghost.PathMatch
		expected int
	}{
		{
			name:      "nil path match (default route)",
			pathMatch: nil,
			expected:  0,
		},
		{
			name: "exact match",
			pathMatch: &ghost.PathMatch{
				Type:  ghost.PathMatchExact,
				Value: "/api/v2/users",
			},
			expected: 10000,
		},
		{
			name: "path prefix short",
			pathMatch: &ghost.PathMatch{
				Type:  ghost.PathMatchPathPrefix,
				Value: "/api",
			},
			expected: 1000 + 40, // 1000 + len("/api")*10
		},
		{
			name: "path prefix long",
			pathMatch: &ghost.PathMatch{
				Type:  ghost.PathMatchPathPrefix,
				Value: "/api/v2/users",
			},
			expected: 1000 + 140, // 1000 + len("/api/v2/users")*10
		},
		{
			name: "regex match",
			pathMatch: &ghost.PathMatch{
				Type:  ghost.PathMatchRegularExpression,
				Value: "/files/.*",
			},
			expected: 100,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := CalculateRoutePriority(tc.pathMatch)
			if result != tc.expected {
				t.Errorf("CalculateRoutePriority() = %d, expected %d", result, tc.expected)
			}
		})
	}
}

func TestCollectHTTPRouteBackendsV2_WithPathMatches(t *testing.T) {
	exactType := gatewayv1.PathMatchExact
	prefixType := gatewayv1.PathMatchPathPrefix

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route-1", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"api.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &exactType,
									Value: ptr("/api/v2/users"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "users-v2",
										Port: ptr(gatewayv1.PortNumber(8080)),
									},
								},
							},
						},
					},
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &prefixType,
									Value: ptr("/api"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "api-v1",
										Port: ptr(gatewayv1.PortNumber(8080)),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	collectedRoutes := CollectHTTPRouteBackendsV2(routes, "default")

	if len(collectedRoutes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(collectedRoutes))
	}

	// Routes should be sorted by priority (descending - higher priority first)
	// Exact match (10000) should come before prefix match (1040)
	if collectedRoutes[0].Service != "users-v2" {
		t.Errorf("expected first route to be users-v2 (exact match), got %s", collectedRoutes[0].Service)
	}
	if collectedRoutes[0].Priority != 10000 {
		t.Errorf("expected first route priority 10000, got %d", collectedRoutes[0].Priority)
	}

	if collectedRoutes[1].Service != "api-v1" {
		t.Errorf("expected second route to be api-v1 (prefix match), got %s", collectedRoutes[1].Service)
	}
	if collectedRoutes[1].Priority != 1040 {
		t.Errorf("expected second route priority 1040, got %d", collectedRoutes[1].Priority)
	}
}

func TestCollectHTTPRouteBackendsV2_NoMatches(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route-1", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"api.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						// No matches - should create default route with PathPrefix "/"
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "api-service",
										Port: ptr(gatewayv1.PortNumber(8080)),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	collectedRoutes := CollectHTTPRouteBackendsV2(routes, "default")

	if len(collectedRoutes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(collectedRoutes))
	}

	if collectedRoutes[0].PathMatch == nil {
		t.Fatal("expected path match to be set")
	}
	if collectedRoutes[0].PathMatch.Type != ghost.PathMatchPathPrefix {
		t.Errorf("expected path match type PathPrefix, got %v", collectedRoutes[0].PathMatch.Type)
	}
	if collectedRoutes[0].PathMatch.Value != "/" {
		t.Errorf("expected path match value /, got %s", collectedRoutes[0].PathMatch.Value)
	}
}
