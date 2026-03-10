package vcl

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"

	"github.com/varnish/gateway/internal/ghost"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

//go:embed preamble.vcl
var preambleVCL string

//go:embed postamble.vcl
var postambleVCL string

// GeneratorConfig holds configuration for VCL generation.
type GeneratorConfig struct {
	GhostConfigPath string // Path to ghost.json (default: /var/run/varnish/ghost.json)
}

// DefaultGhostConfigPath is the default location for ghost.json.
const DefaultGhostConfigPath = "/var/run/varnish/ghost.json"

// Generate produces VCL preamble that integrates with the ghost VMOD.
// The ghost VMOD handles all routing logic internally; VCL just initializes it.
// The VCL template is embedded from preamble.vcl.
func Generate(routes []gatewayv1.HTTPRoute, config GeneratorConfig) string {
	if config.GhostConfigPath == "" {
		config.GhostConfigPath = DefaultGhostConfigPath
	}

	return fmt.Sprintf(preambleVCL, config.GhostConfigPath)
}

// CalculateRoutePriority calculates the priority for a route based on all match criteria.
// Higher priority = more specific match. Path specificity dominates over other criteria
// to ensure path-based routing works correctly per Gateway API spec.
//
// Path specificity:
//   - Exact: 100000
//   - PathPrefix: 10000 + length*100
//   - RegularExpression: 5000
//   - No path match: 0
//
// Additional bonuses (ordered per Gateway API spec precedence):
//   - Method specified: +5000 (must outweigh max headers+query)
//   - Header matches: +200 per header (max 16 = 3200)
//   - Query param matches: +100 per param (max 16 = 1600)
func CalculateRoutePriority(
	pathMatch *ghost.PathMatch,
	method *string,
	headers []ghost.HeaderMatch,
	queryParams []ghost.QueryParamMatch,
) int {
	priority := 0

	// Path specificity (dominates all other criteria)
	if pathMatch != nil {
		switch pathMatch.Type {
		case ghost.PathMatchExact:
			priority += 100000
		case ghost.PathMatchPathPrefix:
			priority += 10000 + len(pathMatch.Value)*100
		case ghost.PathMatchRegularExpression:
			priority += 5000
		}
	}

	// Method specificity (+5000, must outweigh max headers + query params)
	if method != nil {
		priority += 5000
	}

	// Header specificity (200 per header, max 3200)
	headerCount := len(headers)
	if headerCount > 16 {
		headerCount = 16
	}
	priority += headerCount * 200

	// Query param specificity (100 per param, max 1600)
	queryCount := len(queryParams)
	if queryCount > 16 {
		queryCount = 16
	}
	priority += queryCount * 100

	return priority
}

// ServicePortMap maps "namespace/service:servicePort" to the resolved targetPort.
// Used to translate HTTPRoute BackendRef service ports to actual pod ports.
type ServicePortMap map[string]int

// CollectHTTPRouteBackends extracts backend and path match information from HTTPRoutes for config generation.
// Returns a list of Route structs that include path matching rules.
// When gateway is provided, listener information is computed for each route based on parentRef sectionNames.
// When portMap is provided, service ports from BackendRefs are resolved to target ports.
func CollectHTTPRouteBackends(routes []gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway, namespace string, portMap ServicePortMap) []ghost.Route {
	var collectedRoutes []ghost.Route

	ruleIndex := 0
	for _, route := range routes {
		routeNS := route.Namespace
		if routeNS == "" {
			routeNS = namespace
		}

		// Compute which listeners this route applies to
		listeners := listenersForRoute(&route, gateway)

		// Route name for X-Gateway-Route header (namespace/name)
		routeName := routeNS + "/" + route.Name

		// When no hostnames are specified, the route matches all hostnames.
		// Use "*" as a sentinel that ghost VMOD treats as a catch-all.
		hostnames := make([]string, len(route.Spec.Hostnames))
		for i, h := range route.Spec.Hostnames {
			hostnames[i] = string(h)
		}
		if len(hostnames) == 0 {
			hostnames = []string{"*"}
		}

		for _, hostname := range hostnames {
			for _, rule := range route.Spec.Rules {
				// Extract rule name (from GEP-995 Named Route Rules)
				var ruleName string
				if rule.Name != nil {
					ruleName = string(*rule.Name)
				}

				// Process each match in the rule
				if len(rule.Matches) == 0 {
					// No matches specified - create default route with PathPrefix "/"
					// Extract filters even for no-match rules
					var filters *ghost.RouteFilters
					if len(rule.Filters) > 0 {
						filters = extractFilters(rule.Filters)
					}

					// Handle filter-only routes with no backends (e.g., redirects with no matches)
					if len(rule.BackendRefs) == 0 && filters != nil {
						pathMatch := &ghost.PathMatch{
							Type:  ghost.PathMatchPathPrefix,
							Value: "/",
						}
						collectedRoutes = append(collectedRoutes, ghost.Route{
							Hostname:  hostname,
							PathMatch: pathMatch,
							Filters:   filters,
							Service:   "",
							Namespace: routeNS,
							Port:      0,
							Weight:    0,
							Listeners: listeners,
							RouteName: routeName,
							RuleName:  ruleName,
							Priority:  CalculateRoutePriority(pathMatch, nil, nil, nil),
							RuleIndex: ruleIndex,
						})
					}

					for _, backend := range rule.BackendRefs {
						if backend.Name == "" {
							continue
						}

						backendNS := routeNS
						if backend.Namespace != nil {
							backendNS = string(*backend.Namespace)
						}

						port := 80
						if backend.Port != nil {
							port = int(*backend.Port)
						}
						// Resolve service port to target port
						if portMap != nil {
							key := fmt.Sprintf("%s/%s:%d", backendNS, backend.Name, port)
							if tp, ok := portMap[key]; ok {
								port = tp
							}
						}

						weight := 1 // Gateway API default when unspecified
						if backend.Weight != nil {
							weight = int(*backend.Weight)
						}

						// Default route with PathPrefix "/"
						pathMatch := &ghost.PathMatch{
							Type:  ghost.PathMatchPathPrefix,
							Value: "/",
						}

						collectedRoutes = append(collectedRoutes, ghost.Route{
							Hostname:  hostname,
							PathMatch: pathMatch,
							Filters:   filters,
							Service:   string(backend.Name),
							Namespace: backendNS,
							Port:      port,
							Weight:    weight,
							Listeners: listeners,
							RouteName: routeName,
							RuleName:  ruleName,
							Priority:  CalculateRoutePriority(pathMatch, nil, nil, nil),
							RuleIndex: ruleIndex,
						})
					}
				} else {
					// Process each match
					for _, match := range rule.Matches {
						var pathMatch *ghost.PathMatch

						// Extract path match if present
						if match.Path != nil {
							pathType := ghost.PathMatchPathPrefix // default
							if match.Path.Type != nil {
								switch *match.Path.Type {
								case gatewayv1.PathMatchExact:
									pathType = ghost.PathMatchExact
								case gatewayv1.PathMatchPathPrefix:
									pathType = ghost.PathMatchPathPrefix
								case gatewayv1.PathMatchRegularExpression:
									pathType = ghost.PathMatchRegularExpression
								}
							}

							pathValue := "/"
							if match.Path.Value != nil {
								pathValue = *match.Path.Value
							}

							// PathPrefix "/" is the K8s API server default when no path is specified.
							// It matches all paths, so it has no specificity — treat as no path match.
							// Without this, API-defaulted routes get inflated priorities that break
							// Gateway API precedence (e.g., method PATCH with implicit PathPrefix /
							// would outrank an explicit PathPrefix /path5).
							if pathType == ghost.PathMatchPathPrefix && pathValue == "/" {
								pathMatch = nil
							} else {
								pathMatch = &ghost.PathMatch{
									Type:  pathType,
									Value: pathValue,
								}
							}
						}

						// Extract method
						var method *string
						if match.Method != nil {
							m := string(*match.Method)
							method = &m
						}

						// Extract headers
						var headers []ghost.HeaderMatch
						for _, h := range match.Headers {
							matchType := ghost.MatchTypeExact
							if h.Type != nil && *h.Type == gatewayv1.HeaderMatchRegularExpression {
								matchType = ghost.MatchTypeRegularExpression
							}
							headers = append(headers, ghost.HeaderMatch{
								Name:  string(h.Name),
								Value: h.Value,
								Type:  matchType,
							})
						}

						// Extract query params
						var queryParams []ghost.QueryParamMatch
						for _, qp := range match.QueryParams {
							matchType := ghost.MatchTypeExact
							if qp.Type != nil && *qp.Type == gatewayv1.QueryParamMatchRegularExpression {
								matchType = ghost.MatchTypeRegularExpression
							}
							queryParams = append(queryParams, ghost.QueryParamMatch{
								Name:  string(qp.Name),
								Value: qp.Value,
								Type:  matchType,
							})
						}

						// Extract filters
						var filters *ghost.RouteFilters
						if len(rule.Filters) > 0 {
							filters = extractFilters(rule.Filters)
						}

						// Filter to only valid backendRefs (Kind must be Service or unset, Group must be core/"" or unset)
						var validBackendRefs []gatewayv1.HTTPBackendRef
						for _, backend := range rule.BackendRefs {
							if backend.Kind != nil && *backend.Kind != "Service" {
								continue
							}
							if backend.Group != nil && *backend.Group != "" {
								continue
							}
							validBackendRefs = append(validBackendRefs, backend)
						}

						// If there are no valid backends (filter-only routes, or all backends invalid),
						// create a single route entry so the path still matches.
						// Ghost will return 500 for routes with no backends.
						if len(validBackendRefs) == 0 {
							if filters != nil || len(rule.BackendRefs) > 0 {
								collectedRoutes = append(collectedRoutes, ghost.Route{
									Hostname:    hostname,
									PathMatch:   pathMatch,
									Method:      method,
									Headers:     headers,
									QueryParams: queryParams,
									Filters:     filters,
									Service:     "",
									Namespace:   routeNS,
									Port:        0,
									Weight:      0,
									Listeners:   listeners,
									RouteName:   routeName,
									RuleName:    ruleName,
									Priority:    CalculateRoutePriority(pathMatch, method, headers, queryParams),
									RuleIndex:   ruleIndex,
								})
							}
						}

						// Create a route entry for each valid backend
						for _, backend := range validBackendRefs {
							if backend.Name == "" {
								continue
							}

							backendNS := routeNS
							if backend.Namespace != nil {
								backendNS = string(*backend.Namespace)
							}

							port := 80
							if backend.Port != nil {
								port = int(*backend.Port)
							}
							// Resolve service port to target port
							if portMap != nil {
								key := fmt.Sprintf("%s/%s:%d", backendNS, backend.Name, port)
								if tp, ok := portMap[key]; ok {
									port = tp
								}
							}

							weight := 100
							if backend.Weight != nil {
								weight = int(*backend.Weight)
							}

							collectedRoutes = append(collectedRoutes, ghost.Route{
								Hostname:    hostname,
								PathMatch:   pathMatch,
								Method:      method,
								Headers:     headers,
								QueryParams: queryParams,
								Filters:     filters,
								Service:     string(backend.Name),
								Namespace:   backendNS,
								Port:        port,
								Weight:      weight,
								Listeners:   listeners,
								RouteName:   routeName,
								RuleName:    ruleName,
								Priority:    CalculateRoutePriority(pathMatch, method, headers, queryParams),
								RuleIndex:   ruleIndex,
							})
						}
					}
				}
				ruleIndex++
			}
		}
	}

	// Sort by priority (descending), then by rule index (ascending) for tiebreaking,
	// then by hostname and service for deterministic output
	slices.SortFunc(collectedRoutes, func(a, b ghost.Route) int {
		// Higher priority first
		if a.Priority != b.Priority {
			return b.Priority - a.Priority
		}
		// Lower rule index first (earlier rules win)
		if a.RuleIndex != b.RuleIndex {
			return a.RuleIndex - b.RuleIndex
		}
		if a.Hostname != b.Hostname {
			return strings.Compare(a.Hostname, b.Hostname)
		}
		if a.Service != b.Service {
			return strings.Compare(a.Service, b.Service)
		}
		return a.Port - b.Port
	})

	return collectedRoutes
}

// extractFilters converts Gateway API filters to ghost filter configuration
func extractFilters(filters []gatewayv1.HTTPRouteFilter) *ghost.RouteFilters {
	result := &ghost.RouteFilters{}
	hasFilters := false

	for _, filter := range filters {
		switch filter.Type {
		case gatewayv1.HTTPRouteFilterRequestHeaderModifier:
			if filter.RequestHeaderModifier != nil {
				result.RequestHeaderModifier = convertRequestHeaderFilter(filter.RequestHeaderModifier)
				hasFilters = true
			}
		case gatewayv1.HTTPRouteFilterResponseHeaderModifier:
			if filter.ResponseHeaderModifier != nil {
				result.ResponseHeaderModifier = convertResponseHeaderFilter(filter.ResponseHeaderModifier)
				hasFilters = true
			}
		case gatewayv1.HTTPRouteFilterURLRewrite:
			if filter.URLRewrite != nil {
				result.URLRewrite = convertURLRewriteFilter(filter.URLRewrite)
				hasFilters = true
			}
		case gatewayv1.HTTPRouteFilterRequestRedirect:
			if filter.RequestRedirect != nil {
				result.RequestRedirect = convertRequestRedirectFilter(filter.RequestRedirect)
				hasFilters = true
			}
		}
	}

	if !hasFilters {
		return nil
	}
	return result
}

func convertRequestHeaderFilter(f *gatewayv1.HTTPHeaderFilter) *ghost.RequestHeaderFilter {
	result := &ghost.RequestHeaderFilter{}
	for _, h := range f.Set {
		result.Set = append(result.Set, ghost.HTTPHeaderAction{
			Name:  string(h.Name),
			Value: h.Value,
		})
	}
	for _, h := range f.Add {
		result.Add = append(result.Add, ghost.HTTPHeaderAction{
			Name:  string(h.Name),
			Value: h.Value,
		})
	}
	result.Remove = f.Remove
	return result
}

func convertResponseHeaderFilter(f *gatewayv1.HTTPHeaderFilter) *ghost.ResponseHeaderFilter {
	result := &ghost.ResponseHeaderFilter{}
	for _, h := range f.Set {
		result.Set = append(result.Set, ghost.HTTPHeaderAction{
			Name:  string(h.Name),
			Value: h.Value,
		})
	}
	for _, h := range f.Add {
		result.Add = append(result.Add, ghost.HTTPHeaderAction{
			Name:  string(h.Name),
			Value: h.Value,
		})
	}
	result.Remove = f.Remove
	return result
}

func convertURLRewriteFilter(f *gatewayv1.HTTPURLRewriteFilter) *ghost.URLRewriteFilter {
	result := &ghost.URLRewriteFilter{}
	if f.Hostname != nil {
		hostname := string(*f.Hostname)
		result.Hostname = &hostname
	}
	if f.Path != nil {
		pathType := string(f.Path.Type)
		result.PathType = &pathType
		result.ReplaceFullPath = f.Path.ReplaceFullPath
		result.ReplacePrefixMatch = f.Path.ReplacePrefixMatch
	}
	return result
}

func convertRequestRedirectFilter(f *gatewayv1.HTTPRequestRedirectFilter) *ghost.RequestRedirectFilter {
	result := &ghost.RequestRedirectFilter{
		StatusCode: 302, // default
	}
	if f.StatusCode != nil {
		result.StatusCode = *f.StatusCode
	}
	result.Scheme = f.Scheme
	if f.Hostname != nil {
		hostname := string(*f.Hostname)
		result.Hostname = &hostname
	}
	if f.Path != nil {
		pathType := string(f.Path.Type)
		result.PathType = &pathType
		result.ReplaceFullPath = f.Path.ReplaceFullPath
		result.ReplacePrefixMatch = f.Path.ReplacePrefixMatch
	}
	if f.Port != nil {
		port := int(*f.Port)
		result.Port = &port
	}
	return result
}

// socketNameForListener returns the Varnish socket name for a Gateway listener.
// Format: {proto}-{port}, e.g. "http-80", "https-443"
func socketNameForListener(listener *gatewayv1.Listener) string {
	proto := "http"
	if listener.Protocol == gatewayv1.HTTPSProtocolType || listener.Protocol == gatewayv1.TLSProtocolType {
		proto = "https"
	}
	return fmt.Sprintf("%s-%d", proto, listener.Port)
}

// listenersForRoute computes which Varnish listeners a route applies to based on its
// parentRef sectionNames and the Gateway's listener configuration.
// Returns nil if the route applies to all listeners (no filtering needed).
func listenersForRoute(route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) []string {
	if gateway == nil {
		return nil
	}

	// Build map: listener name → socket name
	listenerSockets := make(map[string]string)
	for i := range gateway.Spec.Listeners {
		l := &gateway.Spec.Listeners[i]
		listenerSockets[string(l.Name)] = socketNameForListener(l)
	}

	// Build set of all gateway socket names for "covers all listeners" check
	allSockets := make(map[string]bool)
	for _, s := range listenerSockets {
		allSockets[s] = true
	}

	// Collect socket names from parentRefs targeting this gateway
	socketSet := make(map[string]bool)
	hasSectionName := false
	for _, parentRef := range route.Spec.ParentRefs {
		// Skip non-Gateway refs
		if parentRef.Kind != nil && *parentRef.Kind != "Gateway" {
			continue
		}
		// Check this parentRef targets the given gateway
		if string(parentRef.Name) != gateway.Name {
			continue
		}
		refNS := route.Namespace
		if parentRef.Namespace != nil {
			refNS = string(*parentRef.Namespace)
		}
		if refNS != gateway.Namespace {
			continue
		}

		if parentRef.SectionName == nil {
			// No sectionName = all listeners
			return nil
		}
		hasSectionName = true
		sn := string(*parentRef.SectionName)
		if socket, ok := listenerSockets[sn]; ok {
			socketSet[socket] = true
		}
	}

	if !hasSectionName {
		return nil
	}

	// If the route covers ALL listeners of the gateway, return nil (no filtering needed)
	coversAll := true
	for s := range allSockets {
		if !socketSet[s] {
			coversAll = false
			break
		}
	}
	if coversAll {
		return nil
	}

	// Build sorted result
	var result []string
	for s := range socketSet {
		result = append(result, s)
	}
	slices.Sort(result)
	return result
}
