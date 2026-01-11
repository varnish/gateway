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

func TestGenerate_EmptyRoutes(t *testing.T) {
	config := GeneratorConfig{BackendsFilePath: "/var/run/varnish/backends.conf"}
	result := Generate(nil, config)

	if !strings.Contains(result, "vcl 4.1;") {
		t.Error("expected VCL version header")
	}
	if !strings.Contains(result, "import nodes;") {
		t.Error("expected nodes import")
	}
	if !strings.Contains(result, "import udo;") {
		t.Error("expected udo import")
	}
	if !strings.Contains(result, "sub vcl_init {") {
		t.Error("expected vcl_init subroutine")
	}
	if !strings.Contains(result, "sub gateway_backend_fetch {") {
		t.Error("expected gateway_backend_fetch subroutine")
	}
	if !strings.Contains(result, "sub vcl_backend_fetch {") {
		t.Error("expected vcl_backend_fetch subroutine")
	}
	if !strings.Contains(result, "# --- User VCL concatenated below ---") {
		t.Error("expected user VCL marker")
	}
}

func TestGenerate_SingleRoute(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "test-route"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"foo.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  ptr(gatewayv1.PathMatchPathPrefix),
									Value: ptr("/api"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "svc-foo",
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

	config := GeneratorConfig{BackendsFilePath: "/var/run/varnish/backends.conf"}
	result := Generate(routes, config)

	// Check service director initialization
	if !strings.Contains(result, `new svc_foo_conf = nodes.config_group("/var/run/varnish/backends.conf", "svc_foo")`) {
		t.Error("expected service config group initialization")
	}
	if !strings.Contains(result, "new svc_foo_dir = udo.director(hash)") {
		t.Error("expected service director initialization")
	}
	if !strings.Contains(result, "svc_foo_dir.subscribe(svc_foo_conf.get_tag())") {
		t.Error("expected director subscription")
	}

	// Check route matching
	if !strings.Contains(result, `bereq.http.host == "foo.example.com"`) {
		t.Error("expected hostname match")
	}
	if !strings.Contains(result, `bereq.url ~ "^/api(/|$)"`) {
		t.Error("expected path prefix match")
	}
	if !strings.Contains(result, "set bereq.backend = svc_foo_dir.backend()") {
		t.Error("expected backend assignment")
	}
}

func TestGenerate_ExactPathMatch(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "test-route"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"foo.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  ptr(gatewayv1.PathMatchExact),
									Value: ptr("/api/v1/health"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "health-service",
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

	config := GeneratorConfig{BackendsFilePath: "/var/run/varnish/backends.conf"}
	result := Generate(routes, config)

	if !strings.Contains(result, `bereq.url == "/api/v1/health"`) {
		t.Errorf("expected exact path match, got:\n%s", result)
	}
}

func TestGenerate_RegexPathMatch(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "test-route"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"foo.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  ptr(gatewayv1.PathMatchRegularExpression),
									Value: ptr("^/api/v[0-9]+/.*"),
								},
							},
						},
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

	config := GeneratorConfig{BackendsFilePath: "/var/run/varnish/backends.conf"}
	result := Generate(routes, config)

	if !strings.Contains(result, `bereq.url ~ "^/api/v[0-9]+/.*"`) {
		t.Errorf("expected regex path match, got:\n%s", result)
	}
}

func TestGenerate_WildcardHostname(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "test-route"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"*.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  ptr(gatewayv1.PathMatchPathPrefix),
									Value: ptr("/"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "wildcard-service",
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

	config := GeneratorConfig{BackendsFilePath: "/var/run/varnish/backends.conf"}
	result := Generate(routes, config)

	if !strings.Contains(result, `bereq.http.host ~ "^[^.]+\.example\.com$"`) {
		t.Errorf("expected wildcard hostname match, got:\n%s", result)
	}
}

func TestGenerate_MultipleRoutes(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route-b"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"b.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  ptr(gatewayv1.PathMatchPathPrefix),
									Value: ptr("/b"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "svc-b",
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
			ObjectMeta: metav1.ObjectMeta{Name: "route-a"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"a.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  ptr(gatewayv1.PathMatchPathPrefix),
									Value: ptr("/a"),
								},
							},
						},
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

	config := GeneratorConfig{BackendsFilePath: "/var/run/varnish/backends.conf"}
	result := Generate(routes, config)

	// Both services should be present
	if !strings.Contains(result, "svc_a_dir") {
		t.Error("expected svc-a director")
	}
	if !strings.Contains(result, "svc_b_dir") {
		t.Error("expected svc-b director")
	}

	// Routes should be sorted (route-a before route-b)
	aIdx := strings.Index(result, `bereq.http.host == "a.example.com"`)
	bIdx := strings.Index(result, `bereq.http.host == "b.example.com"`)
	if aIdx == -1 || bIdx == -1 {
		t.Errorf("expected both hostname matches, got:\n%s", result)
	}
	if aIdx > bIdx {
		t.Error("expected routes to be sorted alphabetically by name")
	}
}

func TestGenerate_DeterministicOutput(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route-b"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"b.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "svc-b",
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
			ObjectMeta: metav1.ObjectMeta{Name: "route-a"},
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

	config := GeneratorConfig{BackendsFilePath: "/var/run/varnish/backends.conf"}
	first := Generate(routes, config)

	for i := 0; i < 10; i++ {
		result := Generate(routes, config)
		if result != first {
			t.Errorf("output not deterministic on iteration %d", i)
		}
	}
}

func TestGenerate_DefaultBackendsPath(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "test-route"},
			Spec: gatewayv1.HTTPRouteSpec{
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "svc-foo",
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

	// Empty config should use default path
	result := Generate(routes, GeneratorConfig{})

	if !strings.Contains(result, DefaultBackendsFilePath) {
		t.Errorf("expected default backends path %q in output", DefaultBackendsFilePath)
	}
}

func TestCollectServices(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			Spec: gatewayv1.HTTPRouteSpec{
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "svc-b",
										Port: ptr(gatewayv1.PortNumber(8080)),
									},
								},
							},
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "svc-a",
										Port: ptr(gatewayv1.PortNumber(9090)),
									},
								},
							},
						},
					},
				},
			},
		},
		{
			Spec: gatewayv1.HTTPRouteSpec{
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "svc-a", // duplicate
										Port: ptr(gatewayv1.PortNumber(9090)),
									},
								},
							},
						},
					},
				},
			},
		},
	}

	services := CollectServices(routes)

	if len(services) != 2 {
		t.Errorf("expected 2 unique services, got %d", len(services))
	}

	// Should be sorted
	if services[0].Name != "svc_a" {
		t.Errorf("expected first service to be svc_a, got %s", services[0].Name)
	}
	if services[1].Name != "svc_b" {
		t.Errorf("expected second service to be svc_b, got %s", services[1].Name)
	}

	// Check ports
	if services[0].Port != 9090 {
		t.Errorf("expected svc_a port 9090, got %d", services[0].Port)
	}
	if services[1].Port != 8080 {
		t.Errorf("expected svc_b port 8080, got %d", services[1].Port)
	}
}

func TestCollectServices_DefaultPort(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			Spec: gatewayv1.HTTPRouteSpec{
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "svc-no-port",
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

	services := CollectServices(routes)

	if len(services) != 1 {
		t.Errorf("expected 1 service, got %d", len(services))
	}
	if services[0].Port != 80 {
		t.Errorf("expected default port 80, got %d", services[0].Port)
	}
}

func TestEscapeRegex(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/api", "/api"},
		{"/api.v1", "/api\\.v1"},
		{"/api/v1", "/api/v1"},
		{"example.com", "example\\.com"},
		{"/path?query", "/path\\?query"},
		{"/path[0]", "/path\\[0\\]"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := escapeRegex(tc.input)
			if result != tc.expected {
				t.Errorf("escapeRegex(%q) = %q, expected %q", tc.input, result, tc.expected)
			}
		})
	}
}

func TestGenerate_MultipleHostnames(t *testing.T) {
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "test-route"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"foo.example.com", "bar.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  ptr(gatewayv1.PathMatchPathPrefix),
									Value: ptr("/api"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "svc-foo",
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

	config := GeneratorConfig{BackendsFilePath: "/var/run/varnish/backends.conf"}
	result := Generate(routes, config)

	// Both hostnames should be present with OR
	if !strings.Contains(result, `bereq.http.host == "foo.example.com"`) {
		t.Error("expected foo.example.com hostname")
	}
	if !strings.Contains(result, `bereq.http.host == "bar.example.com"`) {
		t.Error("expected bar.example.com hostname")
	}
	if !strings.Contains(result, "||") {
		t.Error("expected OR condition for multiple hostnames")
	}
}
