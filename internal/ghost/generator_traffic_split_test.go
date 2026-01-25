package ghost

import (
	"testing"
)

// TestMergeRoutesByMatchCriteria verifies that routes with identical match criteria
// are merged into a single RouteBackends entry with all backends combined.
func TestMergeRoutesByMatchCriteria(t *testing.T) {
	// Create two routes with identical match criteria but different services/weights
	// This simulates what the operator generates for traffic splitting
	routes := []Route{
		{
			Hostname: "canary.example.com",
			PathMatch: &PathMatch{
				Type:  PathMatchPathPrefix,
				Value: "/",
			},
			Service:   "app-stable",
			Namespace: "default",
			Port:      8080,
			Weight:    90,
			Priority:  1010,
		},
		{
			Hostname: "canary.example.com",
			PathMatch: &PathMatch{
				Type:  PathMatchPathPrefix,
				Value: "/",
			},
			Service:   "app-canary",
			Namespace: "default",
			Port:      8080,
			Weight:    10,
			Priority:  1010,
		},
	}

	// Create endpoints for both services
	endpoints := ServiceEndpoints{
		"default/app-stable": {
			{IP: "10.0.1.1", Port: 8080},
			{IP: "10.0.1.2", Port: 8080},
		},
		"default/app-canary": {
			{IP: "10.0.2.1", Port: 8080},
			{IP: "10.0.2.2", Port: 8080},
		},
	}

	// Merge routes
	merged := mergeRoutesByMatchCriteria(routes, endpoints)

	// Should result in a single RouteBackends entry
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged route, got %d", len(merged))
	}

	route := merged[0]

	// Should have 4 backends total (2 stable + 2 canary)
	if len(route.Backends) != 4 {
		t.Fatalf("expected 4 backends, got %d", len(route.Backends))
	}

	// Count backends by weight
	var weight90Count, weight10Count int
	for _, backend := range route.Backends {
		switch backend.Weight {
		case 90:
			weight90Count++
		case 10:
			weight10Count++
		default:
			t.Errorf("unexpected weight: %d", backend.Weight)
		}
	}

	// Should have 2 backends with weight 90 (stable pods)
	if weight90Count != 2 {
		t.Errorf("expected 2 backends with weight 90, got %d", weight90Count)
	}

	// Should have 2 backends with weight 10 (canary pods)
	if weight10Count != 2 {
		t.Errorf("expected 2 backends with weight 10, got %d", weight10Count)
	}

	// Check priority is preserved
	if route.Priority != 1010 {
		t.Errorf("expected priority 1010, got %d", route.Priority)
	}

	// Check path match is preserved
	if route.PathMatch == nil {
		t.Fatal("path match should not be nil")
	}
	if route.PathMatch.Type != PathMatchPathPrefix {
		t.Errorf("expected PathPrefix, got %s", route.PathMatch.Type)
	}
	if route.PathMatch.Value != "/" {
		t.Errorf("expected path '/', got '%s'", route.PathMatch.Value)
	}
}

// TestMergeRoutesByMatchCriteriaDifferentPaths verifies that routes with different
// path matches are NOT merged.
func TestMergeRoutesByMatchCriteriaDifferentPaths(t *testing.T) {
	routes := []Route{
		{
			Hostname: "api.example.com",
			PathMatch: &PathMatch{
				Type:  PathMatchPathPrefix,
				Value: "/v1/",
			},
			Service:   "api-v1",
			Namespace: "default",
			Port:      8080,
			Weight:    100,
			Priority:  1010,
		},
		{
			Hostname: "api.example.com",
			PathMatch: &PathMatch{
				Type:  PathMatchPathPrefix,
				Value: "/v2/",
			},
			Service:   "api-v2",
			Namespace: "default",
			Port:      8080,
			Weight:    100,
			Priority:  1010,
		},
	}

	endpoints := ServiceEndpoints{
		"default/api-v1": {{IP: "10.0.1.1", Port: 8080}},
		"default/api-v2": {{IP: "10.0.2.1", Port: 8080}},
	}

	merged := mergeRoutesByMatchCriteria(routes, endpoints)

	// Should result in TWO separate routes (different paths)
	if len(merged) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(merged))
	}

	// Each should have 1 backend
	for _, route := range merged {
		if len(route.Backends) != 1 {
			t.Errorf("expected 1 backend per route, got %d", len(route.Backends))
		}
	}
}
