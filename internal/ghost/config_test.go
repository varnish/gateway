package ghost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name: "valid config with vhosts",
			input: `{
				"version": 2,
				"vhosts": {
					"api.example.com": {
						"routes": [
							{
								"path_match": {"type": "PathPrefix", "value": "/"},
								"backends": [
									{"address": "10.0.0.1", "port": 8080, "weight": 100}
								],
								"priority": 100
							}
						]
					}
				}
			}`,
			wantErr: false,
		},
		{
			name: "valid config with default",
			input: `{
				"version": 2,
				"vhosts": {},
				"default": {
					"backends": [
						{"address": "10.0.0.1", "port": 80, "weight": 100}
					]
				}
			}`,
			wantErr: false,
		},
		{
			name: "invalid version",
			input: `{
				"version": 1,
				"vhosts": {}
			}`,
			wantErr: true,
		},
		{
			name:    "invalid json",
			input:   `{not valid json}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := ParseConfig([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if config.Version != 2 {
				t.Errorf("expected version 2, got %d", config.Version)
			}
		})
	}
}

func TestParseRoutingConfig(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name: "valid routing config",
			input: `{
				"version": 2,
				"vhosts": {
					"api.example.com": {
						"routes": [
							{
								"path_match": {"type": "PathPrefix", "value": "/api"},
								"service": "api-service",
								"namespace": "default",
								"port": 8080,
								"weight": 100,
								"priority": 100
							}
						]
					}
				}
			}`,
			wantErr: false,
		},
		{
			name: "with default",
			input: `{
				"version": 2,
				"vhosts": {},
				"default": {
					"service": "default-backend",
					"namespace": "default",
					"port": 80,
					"weight": 100
				}
			}`,
			wantErr: false,
		},
		{
			name: "invalid version",
			input: `{
				"version": 99,
				"vhosts": {}
			}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := ParseRoutingConfig([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if config.Version != 2 {
				t.Errorf("expected version 2, got %d", config.Version)
			}
		})
	}
}

func TestWriteAndParseConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "ghost.json")

	// Create a config
	config := NewConfig()
	config.VHosts["api.example.com"] = VHostConfig{
		Routes: []RouteBackends{
			{
				PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/"},
				Backends: []Backend{
					{Address: "10.0.0.1", Port: 8080, Weight: 100},
					{Address: "10.0.0.2", Port: 8080, Weight: 100},
				},
				Priority: 100,
			},
		},
	}
	config.Default = &VHost{
		Backends: []Backend{
			{Address: "10.0.99.1", Port: 80, Weight: 100},
		},
	}

	// Write it
	if err := WriteConfig(configPath, config); err != nil {
		t.Fatalf("WriteConfig failed: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file not created: %v", err)
	}

	// Load and parse it back
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	loaded, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}

	// Verify contents
	if loaded.Version != 2 {
		t.Errorf("expected version 2, got %d", loaded.Version)
	}
	if len(loaded.VHosts) != 1 {
		t.Errorf("expected 1 vhost, got %d", len(loaded.VHosts))
	}
	vhost, ok := loaded.VHosts["api.example.com"]
	if !ok {
		t.Error("api.example.com vhost not found")
	}
	if len(vhost.Routes) != 1 {
		t.Errorf("expected 1 route, got %d", len(vhost.Routes))
	}
	if len(vhost.Routes[0].Backends) != 2 {
		t.Errorf("expected 2 backends, got %d", len(vhost.Routes[0].Backends))
	}
	if loaded.Default == nil {
		t.Error("expected default vhost")
	}
	if len(loaded.Default.Backends) != 1 {
		t.Errorf("expected 1 default backend, got %d", len(loaded.Default.Backends))
	}
}

func TestNewConfig(t *testing.T) {
	config := NewConfig()
	if config.Version != 2 {
		t.Errorf("expected version 2, got %d", config.Version)
	}
	if config.VHosts == nil {
		t.Error("VHosts map should be initialized")
	}
	if len(config.VHosts) != 0 {
		t.Error("VHosts map should be empty")
	}
	if config.Default != nil {
		t.Error("Default should be nil")
	}
}

func TestConfigJSONRoundTrip(t *testing.T) {
	config := NewConfig()
	config.VHosts["test.example.com"] = VHostConfig{
		Routes: []RouteBackends{
			{
				PathMatch: &PathMatch{Type: PathMatchExact, Value: "/health"},
				Backends: []Backend{
					{Address: "10.0.0.1", Port: 8080, Weight: 100},
				},
				Priority: 200,
			},
		},
		DefaultBackends: []Backend{
			{Address: "10.0.0.2", Port: 80, Weight: 100},
		},
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	parsed, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}

	if len(parsed.VHosts) != 1 {
		t.Errorf("expected 1 vhost, got %d", len(parsed.VHosts))
	}
	vhost := parsed.VHosts["test.example.com"]
	if len(vhost.Routes) != 1 {
		t.Errorf("expected 1 route, got %d", len(vhost.Routes))
	}
	if vhost.Routes[0].Priority != 200 {
		t.Errorf("expected priority 200, got %d", vhost.Routes[0].Priority)
	}
}
