package ghost

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config represents the ghost.json configuration file.
// This matches the schema expected by the ghost VMOD.
type Config struct {
	Version int              `json:"version"`
	VHosts  map[string]VHost `json:"vhosts"`
	Default *VHost           `json:"default,omitempty"`
}

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

// RoutingConfig represents the routing configuration from the operator.
// This contains vhost definitions without endpoint IPs (those come from EndpointSlices).
type RoutingConfig struct {
	Version int                    `json:"version"`
	VHosts  map[string]RoutingRule `json:"vhosts"`
	Default *RoutingRule           `json:"default,omitempty"`
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

// Route represents a path-based routing rule (v2 config).
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

// VHostRouting represents routing configuration for a vhost with path-based rules (v2).
type VHostRouting struct {
	Routes       []Route       `json:"routes"`
	DefaultRoute *RoutingRule  `json:"default_route,omitempty"`
}

// RoutingConfigV2 represents the v2 routing configuration from the operator.
type RoutingConfigV2 struct {
	Version int                     `json:"version"`
	VHosts  map[string]VHostRouting `json:"vhosts"`
	Default *RoutingRule            `json:"default,omitempty"`
}

// RouteBackends represents a route with resolved backend IPs (v2 ghost.json).
type RouteBackends struct {
	PathMatch   *PathMatch        `json:"path_match,omitempty"`
	Method      *string           `json:"method,omitempty"`
	Headers     []HeaderMatch     `json:"headers,omitempty"`
	QueryParams []QueryParamMatch `json:"query_params,omitempty"`
	Filters     *RouteFilters     `json:"filters,omitempty"`
	Backends    []Backend         `json:"backends"`
	Priority    int               `json:"priority"`
}

// VHostV2 represents a virtual host with path-based routing (v2 ghost.json).
type VHostV2 struct {
	Routes          []RouteBackends `json:"routes"`
	DefaultBackends []Backend       `json:"default_backends,omitempty"`
}

// ConfigV2 represents the v2 ghost.json configuration file.
type ConfigV2 struct {
	Version int                `json:"version"`
	VHosts  map[string]VHostV2 `json:"vhosts"`
	Default *VHost             `json:"default,omitempty"`
}

// NewConfig creates a new Config with the current version.
func NewConfig() *Config {
	return &Config{
		Version: 1,
		VHosts:  make(map[string]VHost),
	}
}

// LoadConfig reads and parses a ghost.json configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("os.ReadFile(%s): %w", path, err)
	}
	return ParseConfig(data)
}

// ParseConfig parses ghost.json content from bytes.
func ParseConfig(data []byte) (*Config, error) {
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %w", err)
	}
	if config.Version != 1 {
		return nil, fmt.Errorf("unsupported config version: %d (expected 1)", config.Version)
	}
	return &config, nil
}

// LoadRoutingConfig reads and parses a routing configuration file from the operator.
func LoadRoutingConfig(path string) (*RoutingConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("os.ReadFile(%s): %w", path, err)
	}
	return ParseRoutingConfig(data)
}

// ParseRoutingConfig parses routing configuration content from bytes.
func ParseRoutingConfig(data []byte) (*RoutingConfig, error) {
	var config RoutingConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %w", err)
	}
	if config.Version != 1 {
		return nil, fmt.Errorf("unsupported routing config version: %d (expected 1)", config.Version)
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

// AddVHost adds a virtual host with its backends to the config.
func (c *Config) AddVHost(hostname string, backends []Backend) {
	c.VHosts[hostname] = VHost{Backends: backends}
}

// SetDefault sets the default backend for requests that don't match any vhost.
func (c *Config) SetDefault(backends []Backend) {
	c.Default = &VHost{Backends: backends}
}

// NewConfigV2 creates a new ConfigV2 with version 2.
func NewConfigV2() *ConfigV2 {
	return &ConfigV2{
		Version: 2,
		VHosts:  make(map[string]VHostV2),
	}
}

// ParseRoutingConfigV2 parses v2 routing configuration content from bytes.
func ParseRoutingConfigV2(data []byte) (*RoutingConfigV2, error) {
	var config RoutingConfigV2
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %w", err)
	}
	if config.Version != 2 {
		return nil, fmt.Errorf("unsupported routing config version: %d (expected 2)", config.Version)
	}
	return &config, nil
}

// ParseConfigV2 parses v2 ghost.json content from bytes.
func ParseConfigV2(data []byte) (*ConfigV2, error) {
	var config ConfigV2
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %w", err)
	}
	if config.Version != 2 {
		return nil, fmt.Errorf("unsupported config version: %d (expected 2)", config.Version)
	}
	return &config, nil
}

// WriteConfigV2 writes a v2 ghost.json configuration file atomically.
func WriteConfigV2(path string, config *ConfigV2) error {
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
