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
