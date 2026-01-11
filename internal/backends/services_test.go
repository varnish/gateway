package backends

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseServicesConfig(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int // number of services
		wantErr bool
	}{
		{
			name: "valid config",
			input: `{
				"services": [
					{"name": "svc_foo", "port": 8080},
					{"name": "svc_bar", "port": 9090}
				]
			}`,
			want:    2,
			wantErr: false,
		},
		{
			name:    "empty services",
			input:   `{"services": []}`,
			want:    0,
			wantErr: false,
		},
		{
			name:    "invalid json",
			input:   `{invalid}`,
			want:    0,
			wantErr: true,
		},
		{
			name:    "empty input",
			input:   ``,
			want:    0,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseServicesConfig([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseServicesConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(got.Services) != tt.want {
				t.Errorf("ParseServicesConfig() got %d services, want %d", len(got.Services), tt.want)
			}
		})
	}
}

func TestLoadServicesConfig(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a valid services.json
	validPath := filepath.Join(tmpDir, "services.json")
	validContent := `{"services": [{"name": "test_svc", "port": 8080}]}`
	if err := os.WriteFile(validPath, []byte(validContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Test loading valid file
	config, err := LoadServicesConfig(validPath)
	if err != nil {
		t.Fatalf("LoadServicesConfig() error = %v", err)
	}
	if len(config.Services) != 1 {
		t.Errorf("expected 1 service, got %d", len(config.Services))
	}
	if config.Services[0].Name != "test_svc" {
		t.Errorf("expected service name 'test_svc', got %q", config.Services[0].Name)
	}

	// Test loading non-existent file
	_, err = LoadServicesConfig(filepath.Join(tmpDir, "nonexistent.json"))
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestServicesConfig_ToMap(t *testing.T) {
	config := &ServicesConfig{
		Services: []Service{
			{Name: "svc_a", Port: 8080},
			{Name: "svc_b", Port: 9090},
		},
	}

	m := config.ToMap()

	if len(m) != 2 {
		t.Errorf("expected 2 entries, got %d", len(m))
	}

	if svc, ok := m["svc_a"]; !ok || svc.Port != 8080 {
		t.Errorf("svc_a not found or wrong port")
	}

	if svc, ok := m["svc_b"]; !ok || svc.Port != 9090 {
		t.Errorf("svc_b not found or wrong port")
	}
}

func TestWriteServicesConfig(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "services.json")

	services := []Service{
		{Name: "svc_foo", Port: 8080},
		{Name: "svc_bar", Port: 9090},
	}

	// Test writing
	err := WriteServicesConfig(path, services)
	if err != nil {
		t.Fatalf("WriteServicesConfig() error = %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("services.json was not created")
	}

	// Test round-trip: read back and verify
	config, err := LoadServicesConfig(path)
	if err != nil {
		t.Fatalf("LoadServicesConfig() error = %v", err)
	}

	if len(config.Services) != 2 {
		t.Errorf("expected 2 services, got %d", len(config.Services))
	}

	// Check services are present (order may differ from input due to JSON)
	m := config.ToMap()
	if svc, ok := m["svc_foo"]; !ok || svc.Port != 8080 {
		t.Errorf("svc_foo not found or wrong port")
	}
	if svc, ok := m["svc_bar"]; !ok || svc.Port != 9090 {
		t.Errorf("svc_bar not found or wrong port")
	}
}

func TestWriteServicesConfig_EmptyServices(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "services.json")

	err := WriteServicesConfig(path, []Service{})
	if err != nil {
		t.Fatalf("WriteServicesConfig() error = %v", err)
	}

	config, err := LoadServicesConfig(path)
	if err != nil {
		t.Fatalf("LoadServicesConfig() error = %v", err)
	}

	if len(config.Services) != 0 {
		t.Errorf("expected 0 services, got %d", len(config.Services))
	}
}

func TestWriteServicesConfig_Overwrite(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "services.json")

	// Write initial data
	err := WriteServicesConfig(path, []Service{{Name: "old_svc", Port: 1234}})
	if err != nil {
		t.Fatalf("first WriteServicesConfig() error = %v", err)
	}

	// Overwrite with new data
	err = WriteServicesConfig(path, []Service{{Name: "new_svc", Port: 5678}})
	if err != nil {
		t.Fatalf("second WriteServicesConfig() error = %v", err)
	}

	// Verify only new data exists
	config, err := LoadServicesConfig(path)
	if err != nil {
		t.Fatalf("LoadServicesConfig() error = %v", err)
	}

	if len(config.Services) != 1 {
		t.Errorf("expected 1 service, got %d", len(config.Services))
	}
	if config.Services[0].Name != "new_svc" {
		t.Errorf("expected new_svc, got %s", config.Services[0].Name)
	}
}
