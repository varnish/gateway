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

	// Routes should be empty array, not nil
	if vhost.Routes == nil {
		t.Error("Routes should be empty array, not nil")
	}
	if len(vhost.Routes) != 0 {
		t.Errorf("expected 0 routes (no endpoints), got %d", len(vhost.Routes))
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
	// Should default to 100
	if apiVhost.Routes[0].Backends[0].Weight != 100 {
		t.Errorf("expected default weight 100, got %d", apiVhost.Routes[0].Backends[0].Weight)
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
