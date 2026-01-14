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
	if !strings.Contains(result, "return (pass)") {
		t.Error("expected return (pass) for reload requests")
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
