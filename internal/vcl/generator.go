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

	// Generate vcl_recv for ghost reload handling
	sb.WriteString("sub vcl_recv {\n")
	sb.WriteString("    set req.http.x-ghost-reload = ghost.recv();\n")
	sb.WriteString("    if (req.http.x-ghost-reload) {\n")
	sb.WriteString("        return (synth(200, \"Reload\"));\n")
	sb.WriteString("    }\n")
	sb.WriteString("}\n\n")

	// Generate vcl_synth for reload response
	sb.WriteString("sub vcl_synth {\n")
	sb.WriteString("    if (resp.reason == \"Reload\") {\n")
	sb.WriteString("        set resp.http.Content-Type = \"application/json\";\n")
	sb.WriteString("        synthetic(req.http.x-ghost-reload);\n")
	sb.WriteString("        return (deliver);\n")
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
