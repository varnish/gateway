package vcl

import (
	"fmt"
	"slices"
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// GeneratorConfig holds configuration for VCL generation.
type GeneratorConfig struct {
	BackendsFilePath string // Path to backends.conf (default: /var/run/varnish/backends.conf)
}

// Service represents a backend service for services.json generation.
type Service struct {
	Name string
	Port int
}

// DefaultBackendsFilePath is the default location for backends.conf.
const DefaultBackendsFilePath = "/var/run/varnish/backends.conf"

// Generate produces VCL from Gateway API HTTPRoutes.
// Output is deterministic: services and routes are sorted for consistent output.
func Generate(routes []gatewayv1.HTTPRoute, config GeneratorConfig) string {
	if config.BackendsFilePath == "" {
		config.BackendsFilePath = DefaultBackendsFilePath
	}

	var sb strings.Builder

	sb.WriteString("vcl 4.1;\n\n")
	sb.WriteString("import nodes;\n")
	sb.WriteString("import udo;\n\n")

	// Collect unique services
	services := CollectServices(routes)

	// Generate vcl_init
	sb.WriteString("sub vcl_init {\n")
	for _, svc := range services {
		sanitized := SanitizeServiceName(svc.Name)
		fmt.Fprintf(&sb, "    new %s_conf = nodes.config_group(%q, %q);\n",
			sanitized, config.BackendsFilePath, sanitized)
		fmt.Fprintf(&sb, "    new %s_dir = udo.director(hash);\n", sanitized)
		fmt.Fprintf(&sb, "    %s_dir.subscribe(%s_conf.get_tag());\n\n", sanitized, sanitized)
	}
	sb.WriteString("}\n\n")

	// Generate gateway_backend_fetch
	sb.WriteString("sub gateway_backend_fetch {\n")
	generateRouteMatches(&sb, routes)
	sb.WriteString("}\n\n")

	// Generate vcl_backend_fetch
	sb.WriteString("sub vcl_backend_fetch {\n")
	sb.WriteString("    call gateway_backend_fetch;\n")
	sb.WriteString("}\n\n")

	sb.WriteString("# --- User VCL concatenated below ---\n")

	return sb.String()
}

// generateRouteMatches generates the if-statements for route matching.
func generateRouteMatches(sb *strings.Builder, routes []gatewayv1.HTTPRoute) {
	// Sort routes for deterministic output
	sortedRoutes := make([]gatewayv1.HTTPRoute, len(routes))
	copy(sortedRoutes, routes)
	slices.SortFunc(sortedRoutes, func(a, b gatewayv1.HTTPRoute) int {
		return strings.Compare(a.Name, b.Name)
	})

	for _, route := range sortedRoutes {
		hostnames := route.Spec.Hostnames
		for _, rule := range route.Spec.Rules {
			if len(rule.BackendRefs) == 0 {
				continue
			}
			backend := rule.BackendRefs[0] // Use first backend for now
			if backend.Name == "" {
				continue
			}

			serviceName := SanitizeServiceName(string(backend.Name))
			conditions := buildConditions(hostnames, rule.Matches)

			if len(conditions) == 0 {
				// No specific conditions, match all
				sb.WriteString(fmt.Sprintf("    set bereq.backend = %s_dir.backend();\n", serviceName))
				sb.WriteString("    return;\n")
			} else {
				sb.WriteString(fmt.Sprintf("    if (%s) {\n", strings.Join(conditions, " || ")))
				sb.WriteString(fmt.Sprintf("        set bereq.backend = %s_dir.backend();\n", serviceName))
				sb.WriteString("        return;\n")
				sb.WriteString("    }\n")
			}
		}
	}
}

// buildConditions builds VCL condition strings from hostnames and matches.
func buildConditions(hostnames []gatewayv1.Hostname, matches []gatewayv1.HTTPRouteMatch) []string {
	var conditions []string

	for _, match := range matches {
		var parts []string

		// Add hostname conditions
		if len(hostnames) > 0 {
			hostConds := buildHostnameConditions(hostnames)
			if len(hostConds) > 0 {
				if len(hostConds) == 1 {
					parts = append(parts, hostConds[0])
				} else {
					parts = append(parts, "("+strings.Join(hostConds, " || ")+")")
				}
			}
		}

		// Add path condition
		if match.Path != nil {
			pathCond := buildPathCondition(match.Path)
			if pathCond != "" {
				parts = append(parts, pathCond)
			}
		}

		if len(parts) > 0 {
			conditions = append(conditions, strings.Join(parts, " && "))
		}
	}

	// If no matches specified, use hostnames only
	if len(matches) == 0 && len(hostnames) > 0 {
		return buildHostnameConditions(hostnames)
	}

	return conditions
}

// buildHostnameConditions builds VCL conditions for hostname matching.
func buildHostnameConditions(hostnames []gatewayv1.Hostname) []string {
	var conditions []string
	for _, hostname := range hostnames {
		h := string(hostname)
		if strings.HasPrefix(h, "*.") {
			// Wildcard hostname: *.example.com
			suffix := strings.TrimPrefix(h, "*.")
			escapedSuffix := escapeRegex(suffix)
			conditions = append(conditions, fmt.Sprintf(`bereq.http.host ~ "^[^.]+\.%s$"`, escapedSuffix))
		} else {
			// Exact hostname
			conditions = append(conditions, fmt.Sprintf(`bereq.http.host == %q`, h))
		}
	}
	return conditions
}

// buildPathCondition builds a VCL condition for path matching.
func buildPathCondition(path *gatewayv1.HTTPPathMatch) string {
	if path == nil || path.Value == nil {
		return ""
	}

	value := *path.Value
	matchType := gatewayv1.PathMatchPathPrefix // default
	if path.Type != nil {
		matchType = *path.Type
	}

	switch matchType {
	case gatewayv1.PathMatchExact:
		return fmt.Sprintf(`bereq.url == %q`, value)
	case gatewayv1.PathMatchPathPrefix:
		if value == "/" {
			// Root prefix matches everything
			return ""
		}
		// PathPrefix /api matches /api, /api/, /api/anything but not /apiv2
		escapedValue := escapeRegex(value)
		return fmt.Sprintf(`bereq.url ~ "^%s(/|$)"`, escapedValue)
	case gatewayv1.PathMatchRegularExpression:
		return fmt.Sprintf(`bereq.url ~ %q`, value)
	default:
		return ""
	}
}

// escapeRegex escapes special regex characters in a string.
func escapeRegex(s string) string {
	// Escape backslash first to avoid double-escaping
	result := strings.ReplaceAll(s, "\\", "\\\\")
	// Then escape other special characters
	special := []string{".", "^", "$", "*", "+", "?", "{", "}", "[", "]", "|", "(", ")"}
	for _, char := range special {
		result = strings.ReplaceAll(result, char, "\\"+char)
	}
	return result
}

// SanitizeServiceName converts a Kubernetes service name to a valid VCL identifier.
// Dots and hyphens are replaced with underscores.
func SanitizeServiceName(name string) string {
	s := strings.ReplaceAll(name, ".", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

// CollectServices extracts unique services from routes for services.json generation.
// Returns a sorted slice of services referenced by the routes.
func CollectServices(routes []gatewayv1.HTTPRoute) []Service {
	seen := make(map[string]Service)

	for _, route := range routes {
		for _, rule := range route.Spec.Rules {
			for _, backend := range rule.BackendRefs {
				if backend.Name == "" {
					continue
				}
				name := SanitizeServiceName(string(backend.Name))
				port := 80 // default
				if backend.Port != nil {
					port = int(*backend.Port)
				}

				key := fmt.Sprintf("%s:%d", name, port)
				if _, exists := seen[key]; !exists {
					seen[key] = Service{
						Name: name,
						Port: port,
					}
				}
			}
		}
	}

	services := make([]Service, 0, len(seen))
	for _, svc := range seen {
		services = append(services, svc)
	}

	slices.SortFunc(services, func(a, b Service) int {
		return strings.Compare(a.Name, b.Name)
	})

	return services
}
