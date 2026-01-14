package ghost

import (
	"testing"
)

func TestGenerate(t *testing.T) {
	routingConfig := &RoutingConfig{
		Version: 1,
		VHosts: map[string]RoutingRule{
			"api.example.com": {
				Service:   "api-service",
				Namespace: "default",
				Port:      8080,
				Weight:    100,
			},
			"web.example.com": {
				Service:   "web-service",
				Namespace: "production",
				Port:      80,
				Weight:    50,
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
	if config.Version != 1 {
		t.Errorf("expected version 1, got %d", config.Version)
	}

	// Check api.example.com
	apiVhost, ok := config.VHosts["api.example.com"]
	if !ok {
		t.Fatal("api.example.com vhost not found")
	}
	if len(apiVhost.Backends) != 2 {
		t.Errorf("expected 2 backends for api.example.com, got %d", len(apiVhost.Backends))
	}

	// Check web.example.com
	webVhost, ok := config.VHosts["web.example.com"]
	if !ok {
		t.Fatal("web.example.com vhost not found")
	}
	if len(webVhost.Backends) != 1 {
		t.Errorf("expected 1 backend for web.example.com, got %d", len(webVhost.Backends))
	}
	if webVhost.Backends[0].Weight != 50 {
		t.Errorf("expected weight 50, got %d", webVhost.Backends[0].Weight)
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
		Version: 1,
		VHosts: map[string]RoutingRule{
			"api.example.com": {
				Service:   "api-service",
				Namespace: "default",
				Port:      8080,
				Weight:    100,
			},
		},
	}

	// No endpoints discovered yet
	endpoints := ServiceEndpoints{}

	config := Generate(routingConfig, endpoints)

	apiVhost, ok := config.VHosts["api.example.com"]
	if !ok {
		t.Fatal("api.example.com vhost not found")
	}
	if len(apiVhost.Backends) != 0 {
		t.Errorf("expected 0 backends (no endpoints), got %d", len(apiVhost.Backends))
	}
}

func TestGenerateDefaultWeight(t *testing.T) {
	routingConfig := &RoutingConfig{
		Version: 1,
		VHosts: map[string]RoutingRule{
			"api.example.com": {
				Service:   "api-service",
				Namespace: "default",
				Port:      8080,
				Weight:    0, // no weight specified
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
	if len(apiVhost.Backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(apiVhost.Backends))
	}
	// Should default to 100
	if apiVhost.Backends[0].Weight != 100 {
		t.Errorf("expected default weight 100, got %d", apiVhost.Backends[0].Weight)
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
	backends := []HTTPRouteBackend{
		{
			Hostname:  "api.example.com",
			Service:   "api-service",
			Namespace: "default",
			Port:      8080,
			Weight:    100,
		},
		{
			Hostname:  "web.example.com",
			Service:   "web-service",
			Namespace: "production",
			Port:      80,
			Weight:    0, // should default to 100
		},
	}

	defaultBackend := &HTTPRouteBackend{
		Service:   "default-backend",
		Namespace: "default",
		Port:      80,
		Weight:    0,
	}

	config := GenerateRoutingConfig(backends, defaultBackend)

	if config.Version != 1 {
		t.Errorf("expected version 1, got %d", config.Version)
	}

	if len(config.VHosts) != 2 {
		t.Errorf("expected 2 vhosts, got %d", len(config.VHosts))
	}

	apiRule := config.VHosts["api.example.com"]
	if apiRule.Service != "api-service" {
		t.Errorf("expected api-service, got %s", apiRule.Service)
	}
	if apiRule.Weight != 100 {
		t.Errorf("expected weight 100, got %d", apiRule.Weight)
	}

	webRule := config.VHosts["web.example.com"]
	if webRule.Weight != 100 {
		t.Errorf("expected default weight 100, got %d", webRule.Weight)
	}

	if config.Default == nil {
		t.Fatal("expected default rule")
	}
	if config.Default.Service != "default-backend" {
		t.Errorf("expected default-backend, got %s", config.Default.Service)
	}
}

func TestGenerateRoutingConfigNoDefault(t *testing.T) {
	backends := []HTTPRouteBackend{
		{
			Hostname:  "api.example.com",
			Service:   "api-service",
			Namespace: "default",
			Port:      8080,
			Weight:    100,
		},
	}

	config := GenerateRoutingConfig(backends, nil)

	if config.Default != nil {
		t.Error("expected no default rule")
	}
}
