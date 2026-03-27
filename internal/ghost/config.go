package ghost

import (
	"encoding/json"
	"fmt"
	"os"
)

// VHost represents a virtual host configuration with its backends.
type VHost struct {
	Backends []BackendGroup `json:"backends"`
}

// Backend represents a single backend endpoint.
type Backend struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

// BackendGroup represents a group of backends sharing a weight.
// This enables correct weighted traffic distribution: the weight belongs to
// the service group (not individual pods), and selection is two-level:
// (1) pick a group by weight, (2) pick a random pod within the group.
type BackendGroup struct {
	Weight     int        `json:"weight"`
	Backends   []Backend  `json:"backends"`
	BackendTLS *BackendTLS `json:"backend_tls,omitempty"`
}

// RoutingRule defines which Kubernetes service handles a vhost.
type RoutingRule struct {
	Service   string `json:"service"`   // Kubernetes service name
	Namespace string `json:"namespace"` // Kubernetes namespace
	Port      int    `json:"port"`      // Service port
	Weight    int    `json:"weight"`    // Default weight for backends
}

// PathMatchType defines the type of path matching.
type PathMatchType string

const (
	PathMatchExact             PathMatchType = "Exact"
	PathMatchPathPrefix        PathMatchType = "PathPrefix"
	PathMatchRegularExpression PathMatchType = "RegularExpression"
)

// PathMatch represents a path matching rule.
type PathMatch struct {
	Type  PathMatchType `json:"type"`
	Value string        `json:"value"`
}

// MatchType defines the type of matching for headers and query params.
type MatchType string

const (
	MatchTypeExact             MatchType = "Exact"
	MatchTypeRegularExpression MatchType = "RegularExpression"
)

// HeaderMatch represents a header matching rule.
type HeaderMatch struct {
	Name  string    `json:"name"`
	Value string    `json:"value"`
	Type  MatchType `json:"type"`
}

// QueryParamMatch represents a query parameter matching rule.
type QueryParamMatch struct {
	Name  string    `json:"name"`
	Value string    `json:"value"`
	Type  MatchType `json:"type"`
}

// HTTPHeaderAction represents a header modification action
type HTTPHeaderAction struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// RequestHeaderFilter defines header modifications for requests
type RequestHeaderFilter struct {
	Set    []HTTPHeaderAction `json:"set,omitempty"`
	Add    []HTTPHeaderAction `json:"add,omitempty"`
	Remove []string           `json:"remove,omitempty"`
}

// ResponseHeaderFilter defines header modifications for responses
type ResponseHeaderFilter struct {
	Set    []HTTPHeaderAction `json:"set,omitempty"`
	Add    []HTTPHeaderAction `json:"add,omitempty"`
	Remove []string           `json:"remove,omitempty"`
}

// URLRewriteFilter defines URL rewrite configuration
type URLRewriteFilter struct {
	Hostname           *string `json:"hostname,omitempty"`
	PathType           *string `json:"path_type,omitempty"`
	ReplaceFullPath    *string `json:"replace_full_path,omitempty"`
	ReplacePrefixMatch *string `json:"replace_prefix_match,omitempty"`
}

// RequestRedirectFilter defines redirect configuration
type RequestRedirectFilter struct {
	Scheme             *string `json:"scheme,omitempty"`
	Hostname           *string `json:"hostname,omitempty"`
	PathType           *string `json:"path_type,omitempty"`
	ReplaceFullPath    *string `json:"replace_full_path,omitempty"`
	ReplacePrefixMatch *string `json:"replace_prefix_match,omitempty"`
	Port               *int    `json:"port,omitempty"`
	StatusCode         int     `json:"status_code"`
}

// RouteFilters holds all filters that can be applied to a route
type RouteFilters struct {
	RequestHeaderModifier  *RequestHeaderFilter   `json:"request_header_modifier,omitempty"`
	ResponseHeaderModifier *ResponseHeaderFilter  `json:"response_header_modifier,omitempty"`
	URLRewrite             *URLRewriteFilter      `json:"url_rewrite,omitempty"`
	RequestRedirect        *RequestRedirectFilter `json:"request_redirect,omitempty"`
}

// CachePolicy defines caching behavior for a route, derived from VarnishCachePolicy.
// Routes without a CachePolicy operate in pass-through mode (no caching).
type CachePolicy struct {
	// DefaultTTLSeconds is used when the origin does NOT send Cache-Control.
	// Mutually exclusive with ForcedTTLSeconds.
	DefaultTTLSeconds *int `json:"default_ttl_seconds,omitempty"`

	// ForcedTTLSeconds overrides origin Cache-Control entirely.
	// Mutually exclusive with DefaultTTLSeconds.
	ForcedTTLSeconds *int `json:"forced_ttl_seconds,omitempty"`

	// GraceSeconds is how long to serve stale content while revalidating.
	GraceSeconds int `json:"grace_seconds"`

	// KeepSeconds is how long to keep stale objects when backends are sick.
	KeepSeconds int `json:"keep_seconds"`

	// RequestCoalescing enables collapsed forwarding.
	RequestCoalescing bool `json:"request_coalescing"`

	// CacheKey customizes the cache key composition.
	CacheKey *CacheKeyConfig `json:"cache_key,omitempty"`

	// BypassHeaders defines header conditions that trigger cache bypass.
	BypassHeaders []BypassHeaderConfig `json:"bypass_headers,omitempty"`
}

// CacheKeyConfig controls cache key composition in ghost.json.
type CacheKeyConfig struct {
	Headers            []string `json:"headers,omitempty"`
	QueryParamsInclude []string `json:"query_params_include,omitempty"`
	QueryParamsExclude []string `json:"query_params_exclude,omitempty"`
}

// BypassHeaderConfig defines a header-based cache bypass rule.
type BypassHeaderConfig struct {
	Name       string `json:"name"`
	ValueRegex string `json:"value_regex,omitempty"`
}

// BackendTLS holds TLS configuration for backend connections.
// Derived from BackendTLSPolicy targeting the backend's Service.
type BackendTLS struct {
	// Hostname is used as the SNI server name and for certificate validation.
	Hostname string `json:"hostname"`
}

// Route represents a path-based routing rule.
type Route struct {
	Hostname    string            `json:"hostname,omitempty"` // Used when collecting from HTTPRoutes
	PathMatch   *PathMatch        `json:"path_match,omitempty"`
	Method      *string           `json:"method,omitempty"`
	Headers     []HeaderMatch     `json:"headers,omitempty"`
	QueryParams []QueryParamMatch `json:"query_params,omitempty"`
	Filters     *RouteFilters     `json:"filters,omitempty"`
	Service     string            `json:"service"`
	Namespace   string            `json:"namespace"`
	Port        int               `json:"port"`
	PortName    string            `json:"port_name,omitempty"` // Service port name for filtering when TargetPort is named
	Weight      int               `json:"weight"`
	Listeners   []string          `json:"listeners,omitempty"`    // Varnish socket names (e.g., ["http-80"])
	RouteName   string            `json:"route_name,omitempty"`   // HTTPRoute namespace/name
	RuleName    string            `json:"rule_name,omitempty"`    // HTTPRouteRule name for per-rule VCP targeting
	Priority    int               `json:"priority"`
	RuleIndex   int               `json:"rule_index"`             // Original rule ordering for tiebreaking
	CachePolicy *CachePolicy      `json:"cache_policy,omitempty"` // Caching behavior from VarnishCachePolicy
	BackendTLS  *BackendTLS       `json:"backend_tls,omitempty"`  // TLS config from BackendTLSPolicy
}

// VHostRouting represents routing configuration for a vhost with path-based rules.
type VHostRouting struct {
	Routes       []Route      `json:"routes"`
	DefaultRoute *RoutingRule `json:"default_route,omitempty"`
}

// RoutingConfig represents the routing configuration from the operator.
type RoutingConfig struct {
	Version int                     `json:"version"`
	VHosts  map[string]VHostRouting `json:"vhosts"`
	Default *RoutingRule            `json:"default,omitempty"`
}

// RouteBackends represents a route with resolved backend IPs.
type RouteBackends struct {
	PathMatch     *PathMatch        `json:"path_match,omitempty"`
	Method        *string           `json:"method,omitempty"`
	Headers       []HeaderMatch     `json:"headers,omitempty"`
	QueryParams   []QueryParamMatch `json:"query_params,omitempty"`
	Filters       *RouteFilters     `json:"filters,omitempty"`
	BackendGroups []BackendGroup    `json:"backend_groups"`
	Listeners     []string          `json:"listeners,omitempty"`    // Varnish socket names (e.g., ["http-80"])
	RouteName     string            `json:"route_name,omitempty"`   // HTTPRoute namespace/name
	Priority      int               `json:"priority"`
	RuleIndex     int               `json:"rule_index"`
	CachePolicy   *CachePolicy      `json:"cache_policy,omitempty"` // Caching behavior from VarnishCachePolicy
}

// VHostConfig represents a virtual host with path-based routing in ghost.json.
type VHostConfig struct {
	Routes          []RouteBackends `json:"routes"`
	DefaultBackends []BackendGroup  `json:"default_backends,omitempty"`
}

// Config represents the ghost.json configuration file.
type Config struct {
	Version int                    `json:"version"`
	VHosts  map[string]VHostConfig `json:"vhosts"`
	Default *VHost                 `json:"default,omitempty"`
}

// NewConfig creates a new Config with version 2.
func NewConfig() *Config {
	return &Config{
		Version: 2,
		VHosts:  make(map[string]VHostConfig),
	}
}

// ParseRoutingConfig parses routing configuration content from bytes.
func ParseRoutingConfig(data []byte) (*RoutingConfig, error) {
	var config RoutingConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %w", err)
	}
	if config.Version != 2 {
		return nil, fmt.Errorf("unsupported routing config version: %d (expected 2)", config.Version)
	}
	return &config, nil
}

// ParseConfig parses ghost.json content from bytes.
func ParseConfig(data []byte) (*Config, error) {
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %w", err)
	}
	if config.Version != 2 {
		return nil, fmt.Errorf("unsupported config version: %d (expected 2)", config.Version)
	}
	return &config, nil
}

// WriteConfig writes a ghost.json configuration file atomically.
func WriteConfig(path string, config *Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("json.MarshalIndent: %w", err)
	}

	// Write to temp file first for atomic operation
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("os.WriteFile(%s): %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // cleanup on failure
		return fmt.Errorf("os.Rename(%s, %s): %w", tmpPath, path, err)
	}

	return nil
}
