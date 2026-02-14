package ghost

import (
	"encoding/json"
	"testing"
)

func TestGenerate(t *testing.T) {
	routingConfig := &RoutingConfig{
		Version: 2,
		VHosts: map[string]VHostRouting{
			"api.example.com": {
				Routes: []Route{
					{
						PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/"},
						Service:   "api-service",
						Namespace: "default",
						Port:      8080,
						Weight:    100,
						Priority:  100,
					},
				},
			},
			"web.example.com": {
				Routes: []Route{
					{
						PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/"},
						Service:   "web-service",
						Namespace: "production",
						Port:      80,
						Weight:    50,
						Priority:  100,
					},
				},
			},
		},
		Default: &RoutingRule{
			Service:   "default-backend",
			Namespace: "default",
			Port:      80,
			Weight:    100,
		},
	}

	endpoints := ServiceEndpoints{
		"default/api-service": {
			{IP: "10.0.0.1", Port: 8080},
			{IP: "10.0.0.2", Port: 8080},
		},
		"production/web-service": {
			{IP: "10.1.0.1", Port: 80},
		},
		"default/default-backend": {
			{IP: "10.99.0.1", Port: 80},
		},
	}

	config := Generate(routingConfig, endpoints)

	// Check version
	if config.Version != 2 {
		t.Errorf("expected version 2, got %d", config.Version)
	}

	// Check api.example.com
	apiVhost, ok := config.VHosts["api.example.com"]
	if !ok {
		t.Fatal("api.example.com vhost not found")
	}
	if len(apiVhost.Routes) != 1 {
		t.Fatalf("expected 1 route for api.example.com, got %d", len(apiVhost.Routes))
	}
	if len(apiVhost.Routes[0].Backends) != 2 {
		t.Errorf("expected 2 backends for api.example.com, got %d", len(apiVhost.Routes[0].Backends))
	}

	// Check web.example.com
	webVhost, ok := config.VHosts["web.example.com"]
	if !ok {
		t.Fatal("web.example.com vhost not found")
	}
	if len(webVhost.Routes) != 1 {
		t.Fatalf("expected 1 route for web.example.com, got %d", len(webVhost.Routes))
	}
	if len(webVhost.Routes[0].Backends) != 1 {
		t.Errorf("expected 1 backend for web.example.com, got %d", len(webVhost.Routes[0].Backends))
	}
	if webVhost.Routes[0].Backends[0].Weight != 50 {
		t.Errorf("expected weight 50, got %d", webVhost.Routes[0].Backends[0].Weight)
	}

	// Check default
	if config.Default == nil {
		t.Fatal("expected default vhost")
	}
	if len(config.Default.Backends) != 1 {
		t.Errorf("expected 1 default backend, got %d", len(config.Default.Backends))
	}
}

func TestGenerateNoEndpoints(t *testing.T) {
	routingConfig := &RoutingConfig{
		Version: 2,
		VHosts: map[string]VHostRouting{
			"api.example.com": {
				Routes: []Route{
					{
						PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/api"},
						Service:   "api-service",
						Namespace: "default",
						Port:      8080,
						Weight:    100,
						Priority:  100,
					},
				},
			},
		},
	}

	// No endpoints discovered yet
	endpoints := ServiceEndpoints{}

	config := Generate(routingConfig, endpoints)

	// Verify structure exists
	vhost, ok := config.VHosts["api.example.com"]
	if !ok {
		t.Fatal("api.example.com vhost not found")
	}

	// Routes should be preserved with empty backends (ghost returns 500 for these)
	if vhost.Routes == nil {
		t.Error("Routes should be empty array, not nil")
	}
	if len(vhost.Routes) != 1 {
		t.Errorf("expected 1 route (with empty backends), got %d", len(vhost.Routes))
	}
	if len(vhost.Routes) > 0 && len(vhost.Routes[0].Backends) != 0 {
		t.Errorf("expected 0 backends for route, got %d", len(vhost.Routes[0].Backends))
	}

	// DefaultBackends should be empty array, not nil
	if vhost.DefaultBackends == nil {
		t.Error("DefaultBackends should be empty array, not nil")
	}
	if len(vhost.DefaultBackends) != 0 {
		t.Errorf("expected 0 default backends, got %d", len(vhost.DefaultBackends))
	}

	// Marshal to JSON and verify it produces [] not null
	jsonData, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("failed to marshal config: %v", err)
	}

	// Parse back to map to check raw JSON values
	var rawConfig map[string]interface{}
	if err := json.Unmarshal(jsonData, &rawConfig); err != nil {
		t.Fatalf("failed to parse marshaled config: %v", err)
	}

	vhosts, ok := rawConfig["vhosts"].(map[string]interface{})
	if !ok {
		t.Fatal("vhosts not found in JSON")
	}

	apiVhost, ok := vhosts["api.example.com"].(map[string]interface{})
	if !ok {
		t.Fatal("api.example.com not found in vhosts")
	}

	// Check that routes is an array (slice), not nil
	routes, ok := apiVhost["routes"].([]interface{})
	if !ok {
		t.Errorf("routes is not an array in JSON, got type %T: %v", apiVhost["routes"], apiVhost["routes"])
	} else if routes == nil {
		t.Error("routes should be empty array [], not null")
	}
}

func TestGenerateDefaultWeight(t *testing.T) {
	routingConfig := &RoutingConfig{
		Version: 2,
		VHosts: map[string]VHostRouting{
			"api.example.com": {
				Routes: []Route{
					{
						PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/"},
						Service:   "api-service",
						Namespace: "default",
						Port:      8080,
						Weight:    0, // no weight specified
						Priority:  100,
					},
				},
			},
		},
	}

	endpoints := ServiceEndpoints{
		"default/api-service": {
			{IP: "10.0.0.1", Port: 8080},
		},
	}

	config := Generate(routingConfig, endpoints)

	apiVhost := config.VHosts["api.example.com"]
	if len(apiVhost.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(apiVhost.Routes))
	}
	if len(apiVhost.Routes[0].Backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(apiVhost.Routes[0].Backends))
	}
	// weight=0 should pass through as-is (no conversion)
	if apiVhost.Routes[0].Backends[0].Weight != 0 {
		t.Errorf("expected weight 0 (pass-through), got %d", apiVhost.Routes[0].Backends[0].Weight)
	}
}

func TestServiceKey(t *testing.T) {
	key := ServiceKey("my-namespace", "my-service")
	expected := "my-namespace/my-service"
	if key != expected {
		t.Errorf("expected %q, got %q", expected, key)
	}
}

func TestGenerateRoutingConfig(t *testing.T) {
	routesByHost := map[string][]Route{
		"api.example.com": {
			{
				PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/api"},
				Service:   "api-service",
				Namespace: "default",
				Port:      8080,
				Weight:    100,
				Priority:  100,
			},
		},
		"web.example.com": {
			{
				PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/"},
				Service:   "web-service",
				Namespace: "production",
				Port:      80,
				Weight:    50,
				Priority:  100,
			},
		},
	}

	defaultBackend := &RoutingRule{
		Service:   "default-backend",
		Namespace: "default",
		Port:      80,
		Weight:    100,
	}

	config := GenerateRoutingConfig(routesByHost, defaultBackend)

	if config.Version != 2 {
		t.Errorf("expected version 2, got %d", config.Version)
	}

	if len(config.VHosts) != 2 {
		t.Errorf("expected 2 vhosts, got %d", len(config.VHosts))
	}

	apiVhost, ok := config.VHosts["api.example.com"]
	if !ok {
		t.Fatal("api.example.com not found")
	}
	if len(apiVhost.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(apiVhost.Routes))
	}
	if apiVhost.Routes[0].Service != "api-service" {
		t.Errorf("expected api-service, got %s", apiVhost.Routes[0].Service)
	}

	if config.Default == nil {
		t.Fatal("expected default rule")
	}
	if config.Default.Service != "default-backend" {
		t.Errorf("expected default-backend, got %s", config.Default.Service)
	}
}

func TestGenerateRoutingConfigNoDefault(t *testing.T) {
	routesByHost := map[string][]Route{
		"api.example.com": {
			{
				PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/"},
				Service:   "api-service",
				Namespace: "default",
				Port:      8080,
				Weight:    100,
				Priority:  100,
			},
		},
	}

	config := GenerateRoutingConfig(routesByHost, nil)

	if config.Default != nil {
		t.Error("expected no default rule")
	}
}

// TestMethodMatchingConformancePipeline simulates the full pipeline for the
// HTTPRouteMethodMatching conformance test. It verifies that the ghost.json
// produced by the operator→chaperone pipeline contains correct priorities
// so that PathPrefix /path5 (priority 10600) outranks method PATCH (priority 5000).
func TestMethodMatchingConformancePipeline(t *testing.T) {
	ns := "gateway-conformance-infra"

	// Simulate the method-matching HTTPRoute rules as Route objects
	// (output of vcl.CollectHTTPRouteBackends)
	patchMethod := "PATCH"
	postMethod := "POST"
	getMethod := "GET"
	putMethod := "PUT"
	deleteMethod := "DELETE"

	routes := []Route{
		// Rule 0: method POST -> v1
		{Hostname: "*", Method: &postMethod, Service: "infra-backend-v1", Namespace: ns, Port: 8080, Weight: 100, Priority: 5000, RuleIndex: 0},
		// Rule 1: method GET -> v2
		{Hostname: "*", Method: &getMethod, Service: "infra-backend-v2", Namespace: ns, Port: 8080, Weight: 100, Priority: 5000, RuleIndex: 1},
		// Rule 2: PathPrefix /path1 + method GET -> v1
		{Hostname: "*", PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/path1"}, Method: &getMethod, Service: "infra-backend-v1", Namespace: ns, Port: 8080, Weight: 100, Priority: 15600, RuleIndex: 2},
		// Rule 3: method PUT + header version=one -> v2
		{Hostname: "*", Method: &putMethod, Headers: []HeaderMatch{{Name: "version", Value: "one", Type: MatchTypeExact}}, Service: "infra-backend-v2", Namespace: ns, Port: 8080, Weight: 100, Priority: 5200, RuleIndex: 3},
		// Rule 4: PathPrefix /path2 + method POST + header version=two -> v3
		{Hostname: "*", PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/path2"}, Method: &postMethod, Headers: []HeaderMatch{{Name: "version", Value: "two", Type: MatchTypeExact}}, Service: "infra-backend-v3", Namespace: ns, Port: 8080, Weight: 100, Priority: 15800, RuleIndex: 4},
		// Rule 5 match 0: PathPrefix /path3 + method PATCH -> v1
		{Hostname: "*", PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/path3"}, Method: &patchMethod, Service: "infra-backend-v1", Namespace: ns, Port: 8080, Weight: 100, Priority: 15600, RuleIndex: 5},
		// Rule 5 match 1: PathPrefix /path4 + method DELETE + header version=three -> v1
		{Hostname: "*", PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/path4"}, Method: &deleteMethod, Headers: []HeaderMatch{{Name: "version", Value: "three", Type: MatchTypeExact}}, Service: "infra-backend-v1", Namespace: ns, Port: 8080, Weight: 100, Priority: 15800, RuleIndex: 5},
		// Rule 6: PathPrefix /path5 (NO method) -> v1 (priority 10600)
		{Hostname: "*", PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/path5"}, Service: "infra-backend-v1", Namespace: ns, Port: 8080, Weight: 100, Priority: 10600, RuleIndex: 6},
		// Rule 7: method PATCH (NO path) -> v2 (priority 5000)
		{Hostname: "*", Method: &patchMethod, Service: "infra-backend-v2", Namespace: ns, Port: 8080, Weight: 100, Priority: 5000, RuleIndex: 7},
		// Rule 8: header version=four -> v3
		{Hostname: "*", Headers: []HeaderMatch{{Name: "version", Value: "four", Type: MatchTypeExact}}, Service: "infra-backend-v3", Namespace: ns, Port: 8080, Weight: 100, Priority: 200, RuleIndex: 8},
	}

	// Group by hostname
	routesByHost := map[string][]Route{
		"*": routes,
	}

	// Generate routing config
	routingConfig := GenerateRoutingConfig(routesByHost, nil)

	// Simulate endpoints for all three backends
	endpoints := ServiceEndpoints{
		ns + "/infra-backend-v1": {{IP: "10.0.0.1", Port: 8080}},
		ns + "/infra-backend-v2": {{IP: "10.0.0.2", Port: 8080}},
		ns + "/infra-backend-v3": {{IP: "10.0.0.3", Port: 8080}},
	}

	// Generate ghost.json (this is what the chaperone does)
	ghostConfig := Generate(routingConfig, endpoints)

	// Check that the * vhost exists
	vhost, ok := ghostConfig.VHosts["*"]
	if !ok {
		t.Fatal("expected * vhost in ghost config")
	}

	// Find the PathPrefix /path5 route and the method PATCH route
	var pathRoute, methodRoute *RouteBackends
	for i := range vhost.Routes {
		r := &vhost.Routes[i]
		if r.PathMatch != nil && r.PathMatch.Value == "/path5" && r.Method == nil {
			pathRoute = r
		}
		if r.PathMatch == nil && r.Method != nil && *r.Method == "PATCH" {
			methodRoute = r
		}
	}

	if pathRoute == nil {
		t.Fatal("PathPrefix /path5 route not found in ghost config")
	}
	if methodRoute == nil {
		t.Fatal("method PATCH route not found in ghost config")
	}

	// THE KEY CHECK: PathPrefix /path5 must have higher priority than method PATCH
	if pathRoute.Priority <= methodRoute.Priority {
		t.Errorf("PathPrefix /path5 (priority %d) must outrank method PATCH (priority %d)",
			pathRoute.Priority, methodRoute.Priority)
	}

	t.Logf("PathPrefix /path5: priority=%d, ruleIndex=%d, backends=%d",
		pathRoute.Priority, pathRoute.RuleIndex, len(pathRoute.Backends))
	t.Logf("method PATCH: priority=%d, ruleIndex=%d, backends=%d",
		methodRoute.Priority, methodRoute.RuleIndex, len(methodRoute.Backends))

	// Also verify the route has backends (not empty)
	if len(pathRoute.Backends) == 0 {
		t.Error("PathPrefix /path5 route has no backends - ghost would return 500, not route to v1")
	}
	if pathRoute.Backends[0].Address != "10.0.0.1" {
		t.Errorf("PathPrefix /path5 should route to v1 (10.0.0.1), got %s", pathRoute.Backends[0].Address)
	}

	// Verify ghost.json serialization round-trip preserves priorities
	jsonData, err := json.MarshalIndent(ghostConfig, "", "  ")
	if err != nil {
		t.Fatalf("failed to marshal ghost config: %v", err)
	}

	// Parse back
	parsed, err := ParseConfig(jsonData)
	if err != nil {
		t.Fatalf("failed to parse ghost config: %v", err)
	}

	parsedVhost := parsed.VHosts["*"]
	var parsedPathRoute, parsedMethodRoute *RouteBackends
	for i := range parsedVhost.Routes {
		r := &parsedVhost.Routes[i]
		if r.PathMatch != nil && r.PathMatch.Value == "/path5" && r.Method == nil {
			parsedPathRoute = r
		}
		if r.PathMatch == nil && r.Method != nil && *r.Method == "PATCH" {
			parsedMethodRoute = r
		}
	}

	if parsedPathRoute == nil {
		t.Fatal("PathPrefix /path5 route lost during JSON round-trip")
	}
	if parsedMethodRoute == nil {
		t.Fatal("method PATCH route lost during JSON round-trip")
	}
	if parsedPathRoute.Priority <= parsedMethodRoute.Priority {
		t.Errorf("after round-trip: PathPrefix /path5 (priority %d) must outrank method PATCH (priority %d)",
			parsedPathRoute.Priority, parsedMethodRoute.Priority)
	}
}
