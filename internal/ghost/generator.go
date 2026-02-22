package ghost

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Endpoint represents a discovered backend endpoint from Kubernetes.
type Endpoint struct {
	IP   string
	Port int
}

// String returns a string representation of the endpoint for comparison and logging.
func (e Endpoint) String() string {
	return fmt.Sprintf("%s:%d", e.IP, e.Port)
}

// ServiceEndpoints maps service keys (namespace/name) to their discovered endpoints.
type ServiceEndpoints map[string][]Endpoint

// ServiceKey returns the key used to look up endpoints for a routing rule.
func ServiceKey(namespace, name string) string {
	return namespace + "/" + name
}

// endpointsToBackends converts discovered endpoints to ghost backends using the routing rule.
// For multi-port services, only endpoints matching the rule's port are included.
func endpointsToBackends(rule RoutingRule, endpoints ServiceEndpoints) []Backend {
	key := ServiceKey(rule.Namespace, rule.Service)
	eps, ok := endpoints[key]
	if !ok {
		return []Backend{}
	}

	backends := make([]Backend, 0, len(eps))
	for _, ep := range eps {
		port := ep.Port
		if port == 0 {
			port = rule.Port
		} else if rule.Port != 0 && port != rule.Port {
			continue
		}
		weight := rule.Weight
		backends = append(backends, Backend{
			Address: ep.IP,
			Port:    port,
			Weight:  weight,
		})
	}
	return backends
}

// GenerateRoutingConfig creates a RoutingConfig from a map of hostname to routes.
// Routes should already be sorted by priority.
func GenerateRoutingConfig(routesByHost map[string][]Route, defaultBackend *RoutingRule) *RoutingConfig {
	config := &RoutingConfig{
		Version: 2,
		VHosts:  make(map[string]VHostRouting),
		Default: defaultBackend,
	}

	for hostname, routes := range routesByHost {
		config.VHosts[hostname] = VHostRouting{
			Routes: routes,
		}
	}

	return config
}

// GroupRoutesByHostname groups routes by hostname for config generation.
func GroupRoutesByHostname(routes []Route, hostnames []string) map[string][]Route {
	grouped := make(map[string][]Route)

	for _, hostname := range hostnames {
		for _, route := range routes {
			// Routes are already sorted by priority, just group them
			grouped[hostname] = append(grouped[hostname], route)
		}
	}

	return grouped
}

// mergeRoutesByMatchCriteria groups routes with identical match criteria and merges their backends.
// This enables traffic splitting where multiple backendRefs in the same HTTPRoute rule
// are combined into a single route with weighted backends.
func mergeRoutesByMatchCriteria(routes []Route, endpoints ServiceEndpoints) []RouteBackends {
	// Group routes by their match criteria
	type routeKey struct {
		pathMatch   string // JSON serialization of PathMatch
		method      string
		headers     string // JSON serialization of Headers
		queryParams string // JSON serialization of QueryParams
		filters     string // JSON serialization of Filters
		priority    int
		ruleIndex   int
	}

	grouped := make(map[routeKey][]Route)
	for _, route := range routes {
		key := routeKey{
			pathMatch:   serializePathMatch(route.PathMatch),
			method:      serializeMethod(route.Method),
			headers:     serializeHeaders(route.Headers),
			queryParams: serializeQueryParams(route.QueryParams),
			filters:     serializeFilters(route.Filters),
			priority:    route.Priority,
			ruleIndex:   route.RuleIndex,
		}
		grouped[key] = append(grouped[key], route)
	}

	// Convert grouped routes to RouteBackends
	result := make([]RouteBackends, 0, len(grouped))
	for key, routeGroup := range grouped {
		// Collect all backends from all routes in this group
		// Initialize to empty slice, not nil - nil marshals to null in JSON
		allBackends := make([]Backend, 0)
		for _, route := range routeGroup {
			backends := routeToBackends(route, endpoints)
			allBackends = append(allBackends, backends...)
		}

		// Always include routes — routes with empty backends will get 500 from ghost VMOD.
		// Use the first route's match criteria (all routes in group have identical criteria)
		firstRoute := routeGroup[0]
		result = append(result, RouteBackends{
			PathMatch:   firstRoute.PathMatch,
			Method:      firstRoute.Method,
			Headers:     firstRoute.Headers,
			QueryParams: firstRoute.QueryParams,
			Filters:     firstRoute.Filters,
			Backends:    allBackends,
			Priority:    key.priority,
			RuleIndex:   key.ruleIndex,
		})
	}

	return result
}

// serializePathMatch converts PathMatch to a string for grouping.
func serializePathMatch(pm *PathMatch) string {
	if pm == nil {
		return ""
	}
	return string(pm.Type) + ":" + pm.Value
}

// serializeMethod converts method pointer to string for grouping.
func serializeMethod(m *string) string {
	if m == nil {
		return ""
	}
	return *m
}

// serializeHeaders converts headers to string for grouping.
func serializeHeaders(headers []HeaderMatch) string {
	if len(headers) == 0 {
		return ""
	}
	var parts []string
	for _, h := range headers {
		parts = append(parts, string(h.Type)+":"+h.Name+":"+h.Value)
	}
	return strings.Join(parts, "|")
}

// serializeQueryParams converts query params to string for grouping.
func serializeQueryParams(params []QueryParamMatch) string {
	if len(params) == 0 {
		return ""
	}
	var parts []string
	for _, p := range params {
		parts = append(parts, string(p.Type)+":"+p.Name+":"+p.Value)
	}
	return strings.Join(parts, "|")
}

// serializeFilters converts filters to string for grouping.
func serializeFilters(filters *RouteFilters) string {
	if filters == nil {
		return ""
	}
	data, _ := json.Marshal(filters)
	return string(data)
}

// Generate creates a ghost.json Config by merging routing rules with discovered endpoints.
// routingConfig contains the vhost-to-service mappings with path-based routes from the operator.
// endpoints contains the discovered pod IPs for each service.
func Generate(routingConfig *RoutingConfig, endpoints ServiceEndpoints) *Config {
	config := NewConfig()

	// Process each vhost
	for hostname, vhostRouting := range routingConfig.VHosts {
		// Initialize to empty slice, not nil - nil marshals to null in JSON
		routeBackends := make([]RouteBackends, 0)

		// Merge routes with identical match criteria (for traffic splitting)
		mergedRoutes := mergeRoutesByMatchCriteria(vhostRouting.Routes, endpoints)
		routeBackends = append(routeBackends, mergedRoutes...)

		// Process default route if present
		// Initialize to empty slice, not nil - nil marshals to null in JSON
		defaultBackends := make([]Backend, 0)
		if vhostRouting.DefaultRoute != nil {
			defaultBackends = endpointsToBackends(*vhostRouting.DefaultRoute, endpoints)
		}

		config.VHosts[hostname] = VHostConfig{
			Routes:          routeBackends,
			DefaultBackends: defaultBackends,
		}
	}

	// Process global default if present
	if routingConfig.Default != nil {
		backends := endpointsToBackends(*routingConfig.Default, endpoints)
		config.Default = &VHost{Backends: backends}
	}

	return config
}

// routeToBackends converts a route with endpoints to backend list.
// For multi-port services, only endpoints matching the route's port are included.
func routeToBackends(route Route, endpoints ServiceEndpoints) []Backend {
	key := ServiceKey(route.Namespace, route.Service)
	eps, ok := endpoints[key]
	if !ok {
		return []Backend{}
	}

	backends := make([]Backend, 0, len(eps))
	for _, ep := range eps {
		port := ep.Port
		if port == 0 {
			port = route.Port
		} else if route.Port != 0 && port != route.Port {
			// Skip endpoints whose port doesn't match the route's target port.
			// This filters out irrelevant ports for multi-port services.
			continue
		}
		weight := route.Weight
		backends = append(backends, Backend{
			Address: ep.IP,
			Port:    port,
			Weight:  weight,
		})
	}
	return backends
}
