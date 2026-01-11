package backends

import (
	"encoding/json"
	"fmt"
	"os"
)

// Service represents a service entry from services.json
type Service struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// ServicesConfig represents the services.json file format
type ServicesConfig struct {
	Services []Service `json:"services"`
}

// LoadServicesConfig reads and parses a services.json file
func LoadServicesConfig(path string) (*ServicesConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("os.ReadFile(%s): %w", path, err)
	}
	return ParseServicesConfig(data)
}

// ParseServicesConfig parses services.json content
func ParseServicesConfig(data []byte) (*ServicesConfig, error) {
	var config ServicesConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %w", err)
	}
	return &config, nil
}

// ToMap converts the services list to a map keyed by service name
func (c *ServicesConfig) ToMap() map[string]Service {
	m := make(map[string]Service, len(c.Services))
	for _, svc := range c.Services {
		m[svc.Name] = svc
	}
	return m
}

// WriteServicesConfig writes a services.json file atomically.
// It writes to a temp file first, then renames to ensure atomicity.
func WriteServicesConfig(path string, services []Service) error {
	config := ServicesConfig{Services: services}
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
