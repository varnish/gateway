package vcl

import (
	"fmt"
	"slices"
	"strings"

	"github.com/varnish/gateway/internal/ghost"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// GeneratorConfig holds configuration for VCL generation.
type GeneratorConfig struct {
	GhostConfigPath string // Path to ghost.json (default: /var/run/varnish/ghost.json)
}

// DefaultGhostConfigPath is the default location for ghost.json.
const DefaultGhostConfigPath = "/var/run/varnish/ghost.json"

// Generate produces VCL preamble that integrates with the ghost VMOD.
// The ghost VMOD handles all routing logic internally; VCL just initializes it.
func Generate(routes []gatewayv1.HTTPRoute, config GeneratorConfig) string {
	if config.GhostConfigPath == "" {
		config.GhostConfigPath = DefaultGhostConfigPath
	}

	var sb strings.Builder

	sb.WriteString("vcl 4.1;\n\n")
	sb.WriteString("import ghost;\n\n")

	// Dummy backend to satisfy VCL compiler requirement (ghost handles actual routing)
	sb.WriteString("backend dummy { .host = \"127.0.0.1\"; .port = \"80\"; }\n\n")

	// Generate vcl_init
	sb.WriteString("sub vcl_init {\n")
	fmt.Fprintf(&sb, "    ghost.init(%q);\n", config.GhostConfigPath)
	sb.WriteString("    new router = ghost.ghost_backend();\n")
	sb.WriteString("}\n\n")

	// Generate vcl_recv - intercept reload requests
	sb.WriteString("sub vcl_recv {\n")
	sb.WriteString("    # Handle reload endpoint (localhost only)\n")
	sb.WriteString("    if (req.url == \"/.varnish-ghost/reload\" && (client.ip == \"127.0.0.1\" || client.ip == \"::1\")) {\n")
	sb.WriteString("        if (router.reload()) {\n")
	sb.WriteString("            return (synth(200, \"OK\"));\n")
	sb.WriteString("        } else {\n")
	sb.WriteString("            return (synth(500, \"Reload failed\"));\n")
	sb.WriteString("        }\n")
	sb.WriteString("    }\n")
	sb.WriteString("}\n\n")

	// Generate vcl_backend_fetch
	sb.WriteString("sub vcl_backend_fetch {\n")
	sb.WriteString("    set bereq.backend = router.backend();\n")
	sb.WriteString("}\n\n")

	sb.WriteString("# --- User VCL concatenated below ---\n")

	return sb.String()
}

// SanitizeServiceName converts a Kubernetes service name to a valid identifier.
// Dots and hyphens are replaced with underscores.
func SanitizeServiceName(name string) string {
	s := strings.ReplaceAll(name, ".", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

// CalculateRoutePriority calculates the priority for a route based on all match criteria.
// Higher priority = more specific match:
// Path specificity:
//   - Exact: 10000
//   - PathPrefix: 1000 + length*10
//   - RegularExpression: 100
//   - No match (default route): 0
// Additional bonuses:
//   - Method specified: +5000
//   - Header matches: +1000 per header (max 16)
//   - Query param matches: +500 per param (max 16)
func CalculateRoutePriority(
	pathMatch *ghost.PathMatch,
	method *string,
	headers []ghost.HeaderMatch,
	queryParams []ghost.QueryParamMatch,
) int {
	priority := 0

	// Path specificity (0-10000+)
	if pathMatch != nil {
		switch pathMatch.Type {
		case ghost.PathMatchExact:
			priority += 10000
		case ghost.PathMatchPathPrefix:
			priority += 1000 + len(pathMatch.Value)*10
		case ghost.PathMatchRegularExpression:
			priority += 100
		}
	}

	// Method specificity (+5000)
	if method != nil {
		priority += 5000
	}

	// Header specificity (1000 per header, max 16000)
	headerCount := len(headers)
	if headerCount > 16 {
		headerCount = 16
	}
	priority += headerCount * 1000

	// Query param specificity (500 per param, max 8000)
	queryCount := len(queryParams)
	if queryCount > 16 {
		queryCount = 16
	}
	priority += queryCount * 500

	return priority
}

// CollectHTTPRouteBackends extracts backend information from HTTPRoutes for ghost config generation.
// Returns a list of HTTPRouteBackend structs that can be used to generate routing.json.
func CollectHTTPRouteBackends(routes []gatewayv1.HTTPRoute, namespace string) []ghost.HTTPRouteBackend {
	var backends []ghost.HTTPRouteBackend

	for _, route := range routes {
		routeNS := route.Namespace
		if routeNS == "" {
			routeNS = namespace
		}

		for _, hostname := range route.Spec.Hostnames {
			for _, rule := range route.Spec.Rules {
				for _, backend := range rule.BackendRefs {
					if backend.Name == "" {
						continue
					}

					// Determine backend namespace
					backendNS := routeNS
					if backend.Namespace != nil {
						backendNS = string(*backend.Namespace)
					}

					port := 80
					if backend.Port != nil {
						port = int(*backend.Port)
					}

					weight := 100
					if backend.Weight != nil {
						weight = int(*backend.Weight)
					}

					backends = append(backends, ghost.HTTPRouteBackend{
						Hostname:  string(hostname),
						Service:   string(backend.Name),
						Namespace: backendNS,
						Port:      port,
						Weight:    weight,
					})
				}
			}
		}
	}

	// Sort for deterministic output
	slices.SortFunc(backends, func(a, b ghost.HTTPRouteBackend) int {
		if a.Hostname != b.Hostname {
			return strings.Compare(a.Hostname, b.Hostname)
		}
		return strings.Compare(a.Service, b.Service)
	})

	return backends
}

// CollectHTTPRouteBackendsV2 extracts backend and path match information from HTTPRoutes for v2 config.
// Returns a list of Route structs that include path matching rules.
func CollectHTTPRouteBackendsV2(routes []gatewayv1.HTTPRoute, namespace string) []ghost.Route {
	var collectedRoutes []ghost.Route

	for _, route := range routes {
		routeNS := route.Namespace
		if routeNS == "" {
			routeNS = namespace
		}

		for _, hostname := range route.Spec.Hostnames {
			for _, rule := range route.Spec.Rules {
				// Process each match in the rule
				if len(rule.Matches) == 0 {
					// No matches specified - create default route with PathPrefix "/"
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

						weight := 100
						if backend.Weight != nil {
							weight = int(*backend.Weight)
						}

						// Default route with PathPrefix "/"
						pathMatch := &ghost.PathMatch{
							Type:  ghost.PathMatchPathPrefix,
							Value: "/",
						}

						collectedRoutes = append(collectedRoutes, ghost.Route{
							Hostname:  string(hostname),
							PathMatch: pathMatch,
							Service:   string(backend.Name),
							Namespace: backendNS,
							Port:      port,
							Weight:    weight,
							Priority:  CalculateRoutePriority(pathMatch, nil, nil, nil),
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

							pathMatch = &ghost.PathMatch{
								Type:  pathType,
								Value: pathValue,
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

						// Create a route entry for each backend
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

							weight := 100
							if backend.Weight != nil {
								weight = int(*backend.Weight)
							}

							collectedRoutes = append(collectedRoutes, ghost.Route{
								Hostname:    string(hostname),
								PathMatch:   pathMatch,
								Method:      method,
								Headers:     headers,
								QueryParams: queryParams,
								Service:     string(backend.Name),
								Namespace:   backendNS,
								Port:        port,
								Weight:      weight,
								Priority:    CalculateRoutePriority(pathMatch, method, headers, queryParams),
							})
						}
					}
				}
			}
		}
	}

	// Sort by priority (descending), then by hostname and service for deterministic output
	slices.SortFunc(collectedRoutes, func(a, b ghost.Route) int {
		// Higher priority first
		if a.Priority != b.Priority {
			return b.Priority - a.Priority
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
