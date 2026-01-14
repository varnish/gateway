package ghost

// Endpoint represents a discovered backend endpoint from Kubernetes.
type Endpoint struct {
	IP   string
	Port int
}

// ServiceEndpoints maps service keys (namespace/name) to their discovered endpoints.
type ServiceEndpoints map[string][]Endpoint

// ServiceKey returns the key used to look up endpoints for a routing rule.
func ServiceKey(namespace, name string) string {
	return namespace + "/" + name
}

// Generate creates a ghost.json Config by merging routing rules with discovered endpoints.
// routingConfig contains the vhost-to-service mappings from the operator.
// endpoints contains the discovered pod IPs for each service.
func Generate(routingConfig *RoutingConfig, endpoints ServiceEndpoints) *Config {
	config := NewConfig()

	// Process each vhost
	for hostname, rule := range routingConfig.VHosts {
		backends := endpointsToBackends(rule, endpoints)
		config.AddVHost(hostname, backends)
	}

	// Process default if present
	if routingConfig.Default != nil {
		backends := endpointsToBackends(*routingConfig.Default, endpoints)
		config.SetDefault(backends)
	}

	return config
}

// endpointsToBackends converts discovered endpoints to ghost backends using the routing rule.
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
		}
		weight := rule.Weight
		if weight == 0 {
			weight = 100 // default weight
		}
		backends = append(backends, Backend{
			Address: ep.IP,
			Port:    port,
			Weight:  weight,
		})
	}
	return backends
}

// GenerateFromHTTPRoutes creates a RoutingConfig from Gateway API HTTPRoute data.
// This is used by the operator to generate the routing configuration.
type HTTPRouteBackend struct {
	Hostname  string
	Service   string
	Namespace string
	Port      int
	Weight    int
}

// GenerateRoutingConfig creates a RoutingConfig from a list of HTTPRoute backends.
func GenerateRoutingConfig(backends []HTTPRouteBackend, defaultBackend *HTTPRouteBackend) *RoutingConfig {
	config := &RoutingConfig{
		Version: 1,
		VHosts:  make(map[string]RoutingRule),
	}

	for _, b := range backends {
		weight := b.Weight
		if weight == 0 {
			weight = 100
		}
		config.VHosts[b.Hostname] = RoutingRule{
			Service:   b.Service,
			Namespace: b.Namespace,
			Port:      b.Port,
			Weight:    weight,
		}
	}

	if defaultBackend != nil {
		weight := defaultBackend.Weight
		if weight == 0 {
			weight = 100
		}
		config.Default = &RoutingRule{
			Service:   defaultBackend.Service,
			Namespace: defaultBackend.Namespace,
			Port:      defaultBackend.Port,
			Weight:    weight,
		}
	}

	return config
}
