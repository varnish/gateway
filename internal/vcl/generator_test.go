package vcl

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/varnish/gateway/internal/ghost"
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

	// Should call router.last_error() on failure
	if !strings.Contains(result, "set req.http.X-Ghost-Error = router.last_error()") {
		t.Error("expected vcl_recv to call router.last_error() and store in req.http.X-Ghost-Error on failed reload")
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

func TestGenerate_GhostErrorSurfacing(t *testing.T) {
	result := Generate(nil, GeneratorConfig{})

	// Check vcl_synth is generated
	if !strings.Contains(result, "sub vcl_synth {") {
		t.Error("expected vcl_synth subroutine to be generated")
	}

	// Check vcl_synth copies error header for reload endpoint
	if !strings.Contains(result, `if (req.url == "/.varnish-ghost/reload")`) {
		t.Error("expected vcl_synth to check for reload endpoint")
	}

	// Check vcl_synth copies req.http.X-Ghost-Error to resp.http.x-ghost-error
	if !strings.Contains(result, "set resp.http.x-ghost-error = req.http.X-Ghost-Error") {
		t.Error("expected vcl_synth to copy X-Ghost-Error from request to response header")
	}

	// Check comment explaining purpose
	if !strings.Contains(result, "Surface ghost reload errors") {
		t.Error("expected comment explaining error surfacing in vcl_synth")
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

func TestCalculateRoutePriority(t *testing.T) {
	tests := []struct {
		name      string
		pathMatch *ghost.PathMatch
		expected  int
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
			expected: 100000,
		},
		{
			name: "path prefix short",
			pathMatch: &ghost.PathMatch{
				Type:  ghost.PathMatchPathPrefix,
				Value: "/api",
			},
			expected: 10000 + 400, // 10000 + len("/api")*100
		},
		{
			name: "path prefix long",
			pathMatch: &ghost.PathMatch{
				Type:  ghost.PathMatchPathPrefix,
				Value: "/api/v2/users",
			},
			expected: 10000 + 1300, // 10000 + len("/api/v2/users")*100 (13 chars)
		},
		{
			name: "regex match",
			pathMatch: &ghost.PathMatch{
				Type:  ghost.PathMatchRegularExpression,
				Value: "/files/.*",
			},
			expected: 5000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := CalculateRoutePriority(tc.pathMatch, nil, nil, nil)
			if result != tc.expected {
				t.Errorf("CalculateRoutePriority() = %d, expected %d", result, tc.expected)
			}
		})
	}

	// Test method vs header precedence (Gateway API spec: method > headers > query params)
	t.Run("method-only beats header-only", func(t *testing.T) {
		methodOnly := CalculateRoutePriority(nil, ptr("GET"), nil, nil)
		headerOnly := CalculateRoutePriority(nil, nil, []ghost.HeaderMatch{{Name: "version", Value: "four", Type: ghost.MatchTypeExact}}, nil)
		if methodOnly <= headerOnly {
			t.Errorf("method-only (%d) should beat header-only (%d)", methodOnly, headerOnly)
		}
	})

	t.Run("method beats max headers", func(t *testing.T) {
		methodOnly := CalculateRoutePriority(nil, ptr("PATCH"), nil, nil)
		// 16 headers is the max
		headers := make([]ghost.HeaderMatch, 16)
		for i := range headers {
			headers[i] = ghost.HeaderMatch{Name: "h", Value: "v", Type: ghost.MatchTypeExact}
		}
		maxHeaders := CalculateRoutePriority(nil, nil, headers, nil)
		if methodOnly <= maxHeaders {
			t.Errorf("method-only (%d) should beat 16 headers (%d)", methodOnly, maxHeaders)
		}
	})

	t.Run("method+header beats header-only", func(t *testing.T) {
		header := []ghost.HeaderMatch{{Name: "version", Value: "four", Type: ghost.MatchTypeExact}}
		methodAndHeader := CalculateRoutePriority(nil, ptr("PATCH"), header, nil)
		headerOnly := CalculateRoutePriority(nil, nil, header, nil)
		if methodAndHeader <= headerOnly {
			t.Errorf("method+header (%d) should beat header-only (%d)", methodAndHeader, headerOnly)
		}
	})

	t.Run("conformance: method:PATCH beats header:version=four", func(t *testing.T) {
		methodPriority := CalculateRoutePriority(nil, ptr("PATCH"), nil, nil)
		headerPriority := CalculateRoutePriority(nil, nil, []ghost.HeaderMatch{{Name: "version", Value: "four", Type: ghost.MatchTypeExact}}, nil)
		if methodPriority <= headerPriority {
			t.Errorf("method:PATCH (%d) should beat header:version=four (%d) per Gateway API spec", methodPriority, headerPriority)
		}
	})
}

func TestCollectHTTPRouteBackends_WithPathMatches(t *testing.T) {
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

	collectedRoutes := CollectHTTPRouteBackends(routes, "default")

	if len(collectedRoutes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(collectedRoutes))
	}

	// Routes should be sorted by priority (descending - higher priority first)
	// Exact match (100000) should come before prefix match (10400)
	if collectedRoutes[0].Hostname != "api.example.com" {
		t.Errorf("expected first route hostname api.example.com, got %s", collectedRoutes[0].Hostname)
	}
	if collectedRoutes[0].Service != "users-v2" {
		t.Errorf("expected first route to be users-v2 (exact match), got %s", collectedRoutes[0].Service)
	}
	if collectedRoutes[0].Priority != 100000 {
		t.Errorf("expected first route priority 100000, got %d", collectedRoutes[0].Priority)
	}

	if collectedRoutes[1].Hostname != "api.example.com" {
		t.Errorf("expected second route hostname api.example.com, got %s", collectedRoutes[1].Hostname)
	}
	if collectedRoutes[1].Service != "api-v1" {
		t.Errorf("expected second route to be api-v1 (prefix match), got %s", collectedRoutes[1].Service)
	}
	if collectedRoutes[1].Priority != 10400 {
		t.Errorf("expected second route priority 10400, got %d", collectedRoutes[1].Priority)
	}
}

// TestMethodMatchingConformanceFullPipeline constructs the exact HTTPRoute object from
// the httproute-method-matching.yaml conformance test and runs it through the full pipeline:
// CollectHTTPRouteBackends → GenerateRoutingConfig → ghost.Generate → JSON marshal → verify
//
// This reproduces the conditions of conformance test #10: PATCH /path5 should route to
// infra-backend-v1 (PathPrefix /path5, priority 10600), NOT infra-backend-v2 (method PATCH, priority 5000).
func TestMethodMatchingConformanceFullPipeline(t *testing.T) {
	ns := "gateway-conformance-infra"
	prefixType := gatewayv1.PathMatchPathPrefix

	// Construct the exact HTTPRoute from httproute-method-matching.yaml
	httpRoute := gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "method-matching",
			Namespace: ns,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			// No hostnames - matches all (becomes "*")
			Rules: []gatewayv1.HTTPRouteRule{
				// NOTE: All matches below include PathPrefix "/" where the YAML has no path.
				// The K8s API server applies defaulting: matches without an explicit path
				// get PathPrefix "/" added. This test mirrors what the operator actually
				// reads from the API server, not the raw YAML.

				// Rule 0: method POST -> v1 (API-defaulted PathPrefix /)
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path:   &gatewayv1.HTTPPathMatch{Type: &prefixType, Value: ptr("/")},
							Method: ptr(gatewayv1.HTTPMethodPost),
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "infra-backend-v1",
								Port: ptr(gatewayv1.PortNumber(8080)),
							},
						},
					}},
				},
				// Rule 1: method GET -> v2 (API-defaulted PathPrefix /)
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path:   &gatewayv1.HTTPPathMatch{Type: &prefixType, Value: ptr("/")},
							Method: ptr(gatewayv1.HTTPMethodGet),
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "infra-backend-v2",
								Port: ptr(gatewayv1.PortNumber(8080)),
							},
						},
					}},
				},
				// Rule 2: PathPrefix /path1 + method GET -> v1
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path:   &gatewayv1.HTTPPathMatch{Type: &prefixType, Value: ptr("/path1")},
							Method: ptr(gatewayv1.HTTPMethodGet),
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "infra-backend-v1",
								Port: ptr(gatewayv1.PortNumber(8080)),
							},
						},
					}},
				},
				// Rule 3: method PUT + header version=one -> v2 (API-defaulted PathPrefix /)
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path:   &gatewayv1.HTTPPathMatch{Type: &prefixType, Value: ptr("/")},
							Method: ptr(gatewayv1.HTTPMethodPut),
							Headers: []gatewayv1.HTTPHeaderMatch{
								{Name: "version", Value: "one"},
							},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "infra-backend-v2",
								Port: ptr(gatewayv1.PortNumber(8080)),
							},
						},
					}},
				},
				// Rule 4: PathPrefix /path2 + method POST + header version=two -> v3
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path:   &gatewayv1.HTTPPathMatch{Type: &prefixType, Value: ptr("/path2")},
							Method: ptr(gatewayv1.HTTPMethodPost),
							Headers: []gatewayv1.HTTPHeaderMatch{
								{Name: "version", Value: "two"},
							},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "infra-backend-v3",
								Port: ptr(gatewayv1.PortNumber(8080)),
							},
						},
					}},
				},
				// Rule 5: Two matches (OR'd)
				//   match 0: PathPrefix /path3 + method PATCH -> v1
				//   match 1: PathPrefix /path4 + method DELETE + header version=three -> v1
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path:   &gatewayv1.HTTPPathMatch{Type: &prefixType, Value: ptr("/path3")},
							Method: ptr(gatewayv1.HTTPMethodPatch),
						},
						{
							Path:   &gatewayv1.HTTPPathMatch{Type: &prefixType, Value: ptr("/path4")},
							Method: ptr(gatewayv1.HTTPMethodDelete),
							Headers: []gatewayv1.HTTPHeaderMatch{
								{Name: "version", Value: "three"},
							},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "infra-backend-v1",
								Port: ptr(gatewayv1.PortNumber(8080)),
							},
						},
					}},
				},
				// Rule 6: PathPrefix /path5 (NO method) -> v1
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path: &gatewayv1.HTTPPathMatch{Type: &prefixType, Value: ptr("/path5")},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "infra-backend-v1",
								Port: ptr(gatewayv1.PortNumber(8080)),
							},
						},
					}},
				},
				// Rule 7: method PATCH (NO path in YAML) -> v2 (API-defaulted PathPrefix /)
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path:   &gatewayv1.HTTPPathMatch{Type: &prefixType, Value: ptr("/")},
							Method: ptr(gatewayv1.HTTPMethodPatch),
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "infra-backend-v2",
								Port: ptr(gatewayv1.PortNumber(8080)),
							},
						},
					}},
				},
				// Rule 8: header version=four -> v3 (API-defaulted PathPrefix /)
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path: &gatewayv1.HTTPPathMatch{Type: &prefixType, Value: ptr("/")},
							Headers: []gatewayv1.HTTPHeaderMatch{
								{Name: "version", Value: "four"},
							},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: "infra-backend-v3",
								Port: ptr(gatewayv1.PortNumber(8080)),
							},
						},
					}},
				},
			},
		},
	}

	// Step 1: CollectHTTPRouteBackends (what the operator does)
	collectedRoutes := CollectHTTPRouteBackends([]gatewayv1.HTTPRoute{httpRoute}, ns)

	t.Logf("CollectHTTPRouteBackends produced %d routes:", len(collectedRoutes))
	for i, r := range collectedRoutes {
		pathStr := "<nil>"
		if r.PathMatch != nil {
			pathStr = string(r.PathMatch.Type) + ":" + r.PathMatch.Value
		}
		methodStr := "<nil>"
		if r.Method != nil {
			methodStr = *r.Method
		}
		t.Logf("  [%d] path=%s method=%s service=%s priority=%d ruleIndex=%d",
			i, pathStr, methodStr, r.Service, r.Priority, r.RuleIndex)
	}

	// Verify Rule 6 (PathPrefix /path5) has higher priority than Rule 7 (method PATCH)
	var rule6Route, rule7Route *ghost.Route
	for i := range collectedRoutes {
		r := &collectedRoutes[i]
		if r.PathMatch != nil && r.PathMatch.Value == "/path5" && r.Method == nil {
			rule6Route = r
		}
		if r.PathMatch == nil && r.Method != nil && *r.Method == "PATCH" && r.RuleIndex == 7 {
			rule7Route = r
		}
	}

	if rule6Route == nil {
		t.Fatal("Rule 6 (PathPrefix /path5) not found in collected routes")
	}
	if rule7Route == nil {
		t.Fatal("Rule 7 (method PATCH) not found in collected routes")
	}

	if rule6Route.Priority <= rule7Route.Priority {
		t.Errorf("Rule 6 PathPrefix /path5 (priority %d) must outrank Rule 7 method PATCH (priority %d)",
			rule6Route.Priority, rule7Route.Priority)
	}
	if rule6Route.Priority != 10600 {
		t.Errorf("Rule 6 expected priority 10600, got %d", rule6Route.Priority)
	}
	if rule7Route.Priority != 5000 {
		t.Errorf("Rule 7 expected priority 5000, got %d", rule7Route.Priority)
	}

	// Step 2: Group by hostname and generate routing config (what the operator does)
	routesByHost := make(map[string][]ghost.Route)
	for _, route := range collectedRoutes {
		routesByHost[route.Hostname] = append(routesByHost[route.Hostname], route)
	}
	routingConfig := ghost.GenerateRoutingConfig(routesByHost, nil)

	// Step 3: Simulate endpoint discovery and generate ghost.json (what the chaperone does)
	endpoints := ghost.ServiceEndpoints{
		ns + "/infra-backend-v1": {{IP: "10.0.0.1", Port: 8080}},
		ns + "/infra-backend-v2": {{IP: "10.0.0.2", Port: 8080}},
		ns + "/infra-backend-v3": {{IP: "10.0.0.3", Port: 8080}},
	}
	ghostConfig := ghost.Generate(routingConfig, endpoints)

	// Step 4: Marshal to JSON (what gets written to ghost.json)
	jsonData, err := json.MarshalIndent(ghostConfig, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal ghost config: %v", err)
	}
	t.Logf("ghost.json:\n%s", string(jsonData))

	// Step 5: Parse back (what ghost VMOD does)
	parsed, err := ghost.ParseConfig(jsonData)
	if err != nil {
		t.Fatalf("failed to parse ghost config: %v", err)
	}

	// Step 6: Verify the parsed config has correct routes
	vhost, ok := parsed.VHosts["*"]
	if !ok {
		t.Fatal("expected * vhost in parsed ghost config")
	}

	t.Logf("Parsed ghost.json has %d routes for * vhost:", len(vhost.Routes))
	for i, r := range vhost.Routes {
		pathStr := "<nil>"
		if r.PathMatch != nil {
			pathStr = string(r.PathMatch.Type) + ":" + r.PathMatch.Value
		}
		methodStr := "<nil>"
		if r.Method != nil {
			methodStr = *r.Method
		}
		t.Logf("  [%d] path=%s method=%s priority=%d ruleIndex=%d backends=%d",
			i, pathStr, methodStr, r.Priority, r.RuleIndex, len(r.Backends))
	}

	// Find the critical routes in the parsed output
	var parsedPathRoute, parsedMethodRoute *ghost.RouteBackends
	for i := range vhost.Routes {
		r := &vhost.Routes[i]
		if r.PathMatch != nil && r.PathMatch.Value == "/path5" && r.Method == nil {
			parsedPathRoute = r
		}
		if r.PathMatch == nil && r.Method != nil && *r.Method == "PATCH" && r.RuleIndex == 7 {
			parsedMethodRoute = r
		}
	}

	if parsedPathRoute == nil {
		t.Fatal("PathPrefix /path5 route not found in parsed ghost config")
	}
	if parsedMethodRoute == nil {
		t.Fatal("method PATCH route (ruleIndex=7) not found in parsed ghost config")
	}

	// THE KEY CHECK: PathPrefix /path5 must outrank method PATCH
	if parsedPathRoute.Priority <= parsedMethodRoute.Priority {
		t.Errorf("CONFORMANCE BUG: PathPrefix /path5 (priority %d, ruleIndex %d) must outrank method PATCH (priority %d, ruleIndex %d)",
			parsedPathRoute.Priority, parsedPathRoute.RuleIndex,
			parsedMethodRoute.Priority, parsedMethodRoute.RuleIndex)
	}

	// Verify backends resolve correctly
	if len(parsedPathRoute.Backends) == 0 {
		t.Error("PathPrefix /path5 route has no backends - ghost would return 500")
	} else if parsedPathRoute.Backends[0].Address != "10.0.0.1" {
		t.Errorf("PathPrefix /path5 should route to v1 (10.0.0.1), got %s", parsedPathRoute.Backends[0].Address)
	}

	if len(parsedMethodRoute.Backends) == 0 {
		t.Error("method PATCH route has no backends")
	} else if parsedMethodRoute.Backends[0].Address != "10.0.0.2" {
		t.Errorf("method PATCH should route to v2 (10.0.0.2), got %s", parsedMethodRoute.Backends[0].Address)
	}

	// Run the pipeline 100 times to check for non-determinism from Go map iteration
	for iter := 0; iter < 100; iter++ {
		gc := ghost.Generate(routingConfig, endpoints)
		v, ok := gc.VHosts["*"]
		if !ok {
			t.Fatalf("iteration %d: missing * vhost", iter)
		}
		var pr, mr *ghost.RouteBackends
		for i := range v.Routes {
			r := &v.Routes[i]
			if r.PathMatch != nil && r.PathMatch.Value == "/path5" && r.Method == nil {
				pr = r
			}
			if r.PathMatch == nil && r.Method != nil && *r.Method == "PATCH" && r.RuleIndex == 7 {
				mr = r
			}
		}
		if pr == nil || mr == nil {
			t.Fatalf("iteration %d: missing routes (pathRoute=%v, methodRoute=%v)", iter, pr != nil, mr != nil)
		}
		if pr.Priority <= mr.Priority {
			t.Fatalf("iteration %d: priority inversion! path=%d method=%d", iter, pr.Priority, mr.Priority)
		}
		if len(pr.Backends) == 0 {
			t.Fatalf("iteration %d: PathPrefix /path5 has no backends", iter)
		}
	}
}

func TestCollectHTTPRouteBackends_NoMatches(t *testing.T) {
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

	collectedRoutes := CollectHTTPRouteBackends(routes, "default")

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
