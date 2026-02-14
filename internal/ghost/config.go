package ghost

import (
	"encoding/json"
	"fmt"
	"os"
)

// VHost represents a virtual host configuration with its backends.
type VHost struct {
	Backends []Backend `json:"backends"`
}

// Backend represents a single backend endpoint.
type Backend struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	Weight  int    `json:"weight"`
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
	RequestHeaderModifier  *RequestHeaderFilter  `json:"request_header_modifier,omitempty"`
	ResponseHeaderModifier *ResponseHeaderFilter `json:"response_header_modifier,omitempty"`
	URLRewrite             *URLRewriteFilter     `json:"url_rewrite,omitempty"`
	RequestRedirect        *RequestRedirectFilter `json:"request_redirect,omitempty"`
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
	Weight      int               `json:"weight"`
	Priority    int               `json:"priority"`
}

// VHostRouting represents routing configuration for a vhost with path-based rules.
type VHostRouting struct {
	Routes       []Route       `json:"routes"`
	DefaultRoute *RoutingRule  `json:"default_route,omitempty"`
}

// RoutingConfig represents the routing configuration from the operator.
type RoutingConfig struct {
	Version int                     `json:"version"`
	VHosts  map[string]VHostRouting `json:"vhosts"`
	Default *RoutingRule            `json:"default,omitempty"`
}

// RouteBackends represents a route with resolved backend IPs.
type RouteBackends struct {
	PathMatch   *PathMatch        `json:"path_match,omitempty"`
	Method      *string           `json:"method,omitempty"`
	Headers     []HeaderMatch     `json:"headers,omitempty"`
	QueryParams []QueryParamMatch `json:"query_params,omitempty"`
	Filters     *RouteFilters     `json:"filters,omitempty"`
	Backends    []Backend         `json:"backends"`
	Priority    int               `json:"priority"`
}

// VHostConfig represents a virtual host with path-based routing in ghost.json.
type VHostConfig struct {
	Routes          []RouteBackends `json:"routes"`
	DefaultBackends []Backend       `json:"default_backends,omitempty"`
}

// Config represents the ghost.json configuration file.
type Config struct {
	Version int                   `json:"version"`
	VHosts  map[string]VHostConfig `json:"vhosts"`
	Default *VHost                `json:"default,omitempty"`
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
