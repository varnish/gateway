package ghost

import (
	"testing"
)

// TestMergeRoutesByMatchCriteria verifies that routes with identical match criteria
// are merged into a single RouteBackends entry with separate backend groups per service.
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

	// Should have 2 backend groups (one per service)
	if len(route.BackendGroups) != 2 {
		t.Fatalf("expected 2 backend groups, got %d", len(route.BackendGroups))
	}

	// Find groups by weight
	var group90, group10 *BackendGroup
	for i := range route.BackendGroups {
		switch route.BackendGroups[i].Weight {
		case 90:
			group90 = &route.BackendGroups[i]
		case 10:
			group10 = &route.BackendGroups[i]
		default:
			t.Errorf("unexpected group weight: %d", route.BackendGroups[i].Weight)
		}
	}

	if group90 == nil {
		t.Fatal("expected group with weight 90")
	}
	if group10 == nil {
		t.Fatal("expected group with weight 10")
	}

	// Each group should have 2 backends (pods)
	if len(group90.Backends) != 2 {
		t.Errorf("expected 2 backends in weight-90 group, got %d", len(group90.Backends))
	}
	if len(group10.Backends) != 2 {
		t.Errorf("expected 2 backends in weight-10 group, got %d", len(group10.Backends))
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

// TestMergeRoutesByMatchCriteriaDifferentCachePolicy verifies that routes with identical
// match criteria but different cache policies are NOT merged.
// Regression test for https://github.com/varnish/gateway/issues/8
func TestMergeRoutesByMatchCriteriaDifferentCachePolicy(t *testing.T) {
	defaultTTL := 60
	forcedTTL := 300

	routes := []Route{
		{
			Hostname: "api.example.com",
			PathMatch: &PathMatch{
				Type:  PathMatchPathPrefix,
				Value: "/",
			},
			Service:   "app-cached",
			Namespace: "default",
			Port:      8080,
			Weight:    100,
			Priority:  1010,
			CachePolicy: &CachePolicy{
				DefaultTTLSeconds: &defaultTTL,
				GraceSeconds:      30,
			},
		},
		{
			Hostname: "api.example.com",
			PathMatch: &PathMatch{
				Type:  PathMatchPathPrefix,
				Value: "/",
			},
			Service:   "app-forced",
			Namespace: "default",
			Port:      8080,
			Weight:    100,
			Priority:  1010,
			CachePolicy: &CachePolicy{
				ForcedTTLSeconds: &forcedTTL,
				GraceSeconds:     60,
			},
		},
	}

	endpoints := ServiceEndpoints{
		"default/app-cached": {{IP: "10.0.1.1", Port: 8080}},
		"default/app-forced": {{IP: "10.0.2.1", Port: 8080}},
	}

	merged := mergeRoutesByMatchCriteria(routes, endpoints)

	// Should result in TWO separate routes (different cache policies)
	if len(merged) != 2 {
		t.Fatalf("expected 2 routes (different cache policies should not merge), got %d", len(merged))
	}

	// Each should have 1 backend group
	for _, route := range merged {
		if len(route.BackendGroups) != 1 {
			t.Errorf("expected 1 backend group per route, got %d", len(route.BackendGroups))
		}
		if route.CachePolicy == nil {
			t.Error("expected cache policy to be preserved, got nil")
		}
	}
}

// TestMergeRoutesByMatchCriteriaSameCachePolicy verifies that routes with identical
// match criteria AND identical cache policies ARE still merged (traffic splitting).
func TestMergeRoutesByMatchCriteriaSameCachePolicy(t *testing.T) {
	ttl := 60

	routes := []Route{
		{
			Hostname: "api.example.com",
			PathMatch: &PathMatch{
				Type:  PathMatchPathPrefix,
				Value: "/",
			},
			Service:   "app-stable",
			Namespace: "default",
			Port:      8080,
			Weight:    90,
			Priority:  1010,
			CachePolicy: &CachePolicy{
				DefaultTTLSeconds: &ttl,
				GraceSeconds:      30,
			},
		},
		{
			Hostname: "api.example.com",
			PathMatch: &PathMatch{
				Type:  PathMatchPathPrefix,
				Value: "/",
			},
			Service:   "app-canary",
			Namespace: "default",
			Port:      8080,
			Weight:    10,
			Priority:  1010,
			CachePolicy: &CachePolicy{
				DefaultTTLSeconds: &ttl,
				GraceSeconds:      30,
			},
		},
	}

	endpoints := ServiceEndpoints{
		"default/app-stable": {{IP: "10.0.1.1", Port: 8080}},
		"default/app-canary": {{IP: "10.0.2.1", Port: 8080}},
	}

	merged := mergeRoutesByMatchCriteria(routes, endpoints)

	// Should merge into 1 route (same cache policy)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged route (identical cache policies), got %d", len(merged))
	}

	if len(merged[0].BackendGroups) != 2 {
		t.Errorf("expected 2 backend groups, got %d", len(merged[0].BackendGroups))
	}

	if merged[0].CachePolicy == nil {
		t.Error("expected cache policy to be preserved, got nil")
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

	// Each should have 1 backend group with 1 backend
	for _, route := range merged {
		if len(route.BackendGroups) != 1 {
			t.Errorf("expected 1 backend group per route, got %d", len(route.BackendGroups))
		}
		if len(route.BackendGroups) > 0 && len(route.BackendGroups[0].Backends) != 1 {
			t.Errorf("expected 1 backend per group, got %d", len(route.BackendGroups[0].Backends))
		}
	}
}
