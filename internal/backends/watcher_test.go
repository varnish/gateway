package backends

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestExtractEndpoints(t *testing.T) {
	ready := true
	notReady := false
	port := int32(8080)

	tests := []struct {
		name        string
		slice       *discoveryv1.EndpointSlice
		defaultPort int
		wantCount   int
	}{
		{
			name: "ready endpoints",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1", "10.0.0.2"},
						Conditions: discoveryv1.EndpointConditions{Ready: &ready},
					},
				},
				Ports: []discoveryv1.EndpointPort{
					{Port: &port},
				},
			},
			defaultPort: 80,
			wantCount:   2,
		},
		{
			name: "mixed ready and not ready",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: &ready},
					},
					{
						Addresses:  []string{"10.0.0.2"},
						Conditions: discoveryv1.EndpointConditions{Ready: &notReady},
					},
				},
				Ports: []discoveryv1.EndpointPort{
					{Port: &port},
				},
			},
			defaultPort: 80,
			wantCount:   1, // only ready endpoint
		},
		{
			name: "no ports uses default",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: &ready},
					},
				},
				Ports: []discoveryv1.EndpointPort{},
			},
			defaultPort: 9090,
			wantCount:   1,
		},
		{
			name: "empty endpoints",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{},
			},
			defaultPort: 80,
			wantCount:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractEndpoints(tt.slice, tt.defaultPort)
			if len(got) != tt.wantCount {
				t.Errorf("extractEndpoints() got %d endpoints, want %d", len(got), tt.wantCount)
			}
		})
	}
}

func TestExtractEndpoints_PortFromSlice(t *testing.T) {
	ready := true
	port := int32(8080)

	slice := &discoveryv1.EndpointSlice{
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{Port: &port},
		},
	}

	endpoints := extractEndpoints(slice, 80)
	if len(endpoints) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(endpoints))
	}
	if endpoints[0].Port != 8080 {
		t.Errorf("expected port 8080 from slice, got %d", endpoints[0].Port)
	}
}

func TestWatcher_RegenerateBackends(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	client := fake.NewSimpleClientset()

	tmpDir := t.TempDir()
	servicesPath := filepath.Join(tmpDir, "services.json")
	backendsPath := filepath.Join(tmpDir, "backends.conf")

	// Create services.json
	servicesContent := `{"services": [{"name": "svc_foo", "port": 8080}]}`
	if err := os.WriteFile(servicesPath, []byte(servicesContent), 0644); err != nil {
		t.Fatal(err)
	}

	w := NewWatcher(client, servicesPath, backendsPath, "default", logger)

	// Load services first
	if err := w.loadServices(); err != nil {
		t.Fatalf("loadServices() error = %v", err)
	}

	// Add some endpoints manually
	w.mu.Lock()
	w.endpoints["svc_foo"] = []Endpoint{
		{IP: "10.0.0.1", Port: 8080},
		{IP: "10.0.0.2", Port: 8080},
	}
	w.mu.Unlock()

	// Regenerate backends
	if err := w.regenerateBackends(); err != nil {
		t.Fatalf("regenerateBackends() error = %v", err)
	}

	// Verify backends.conf was written
	content, err := os.ReadFile(backendsPath)
	if err != nil {
		t.Fatalf("failed to read backends.conf: %v", err)
	}

	// Check content
	contentStr := string(content)
	if !strings.Contains(contentStr, "[svc_foo]") {
		t.Error("backends.conf missing [svc_foo] section")
	}
	if !strings.Contains(contentStr, "10.0.0.1:8080") {
		t.Error("backends.conf missing 10.0.0.1:8080")
	}
	if !strings.Contains(contentStr, "10.0.0.2:8080") {
		t.Error("backends.conf missing 10.0.0.2:8080")
	}
}

func TestWatcher_LoadServices(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	client := fake.NewSimpleClientset()

	tmpDir := t.TempDir()
	servicesPath := filepath.Join(tmpDir, "services.json")
	backendsPath := filepath.Join(tmpDir, "backends.conf")

	// Create services.json with two services
	servicesContent := `{"services": [{"name": "svc_a", "port": 8080}, {"name": "svc_b", "port": 9090}]}`
	if err := os.WriteFile(servicesPath, []byte(servicesContent), 0644); err != nil {
		t.Fatal(err)
	}

	w := NewWatcher(client, servicesPath, backendsPath, "default", logger)

	if err := w.loadServices(); err != nil {
		t.Fatalf("loadServices() error = %v", err)
	}

	w.mu.RLock()
	defer w.mu.RUnlock()

	if len(w.currentServices) != 2 {
		t.Errorf("expected 2 services, got %d", len(w.currentServices))
	}

	if svc, ok := w.currentServices["svc_a"]; !ok || svc.Port != 8080 {
		t.Error("svc_a not found or wrong port")
	}

	if svc, ok := w.currentServices["svc_b"]; !ok || svc.Port != 9090 {
		t.Error("svc_b not found or wrong port")
	}
}

func TestWatcher_HandleEndpointSliceUpdate(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	client := fake.NewSimpleClientset()

	tmpDir := t.TempDir()
	servicesPath := filepath.Join(tmpDir, "services.json")
	backendsPath := filepath.Join(tmpDir, "backends.conf")

	// Create services.json
	servicesContent := `{"services": [{"name": "my-service", "port": 8080}]}`
	if err := os.WriteFile(servicesPath, []byte(servicesContent), 0644); err != nil {
		t.Fatal(err)
	}

	w := NewWatcher(client, servicesPath, backendsPath, "default", logger)

	// Load services first
	if err := w.loadServices(); err != nil {
		t.Fatalf("loadServices() error = %v", err)
	}

	ready := true
	port := int32(8080)

	// Create an EndpointSlice
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-service-abc",
			Namespace: "default",
			Labels: map[string]string{
				"kubernetes.io/service-name": "my-service",
			},
		},
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1", "10.0.0.2"},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{Port: &port},
		},
	}

	// Handle the update
	w.handleEndpointSliceUpdate(slice)

	// Verify endpoints were stored
	w.mu.RLock()
	eps := w.endpoints["my-service"]
	w.mu.RUnlock()

	if len(eps) != 2 {
		t.Errorf("expected 2 endpoints, got %d", len(eps))
	}

	// Verify backends.conf was written
	content, err := os.ReadFile(backendsPath)
	if err != nil {
		t.Fatalf("failed to read backends.conf: %v", err)
	}

	if !strings.Contains(string(content), "[my-service]") {
		t.Error("backends.conf missing [my-service] section")
	}
}

func TestWatcher_IgnoresUnwatchedServices(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	client := fake.NewSimpleClientset()

	tmpDir := t.TempDir()
	servicesPath := filepath.Join(tmpDir, "services.json")
	backendsPath := filepath.Join(tmpDir, "backends.conf")

	// Create services.json with only svc_a
	servicesContent := `{"services": [{"name": "svc_a", "port": 8080}]}`
	if err := os.WriteFile(servicesPath, []byte(servicesContent), 0644); err != nil {
		t.Fatal(err)
	}

	w := NewWatcher(client, servicesPath, backendsPath, "default", logger)

	if err := w.loadServices(); err != nil {
		t.Fatalf("loadServices() error = %v", err)
	}

	ready := true

	// Create an EndpointSlice for a service we're NOT watching
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"kubernetes.io/service-name": "svc_b", // not in our services.json
			},
		},
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			},
		},
	}

	// Handle the update - should be ignored
	w.handleEndpointSliceUpdate(slice)

	// Verify no endpoints were stored for svc_b
	w.mu.RLock()
	_, exists := w.endpoints["svc_b"]
	w.mu.RUnlock()

	if exists {
		t.Error("should not have stored endpoints for unwatched service")
	}
}

func TestWatcher_Run_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Create EndpointSlice in fake client
	ready := true
	port := int32(8080)
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-svc-abc",
			Namespace: "default",
			Labels: map[string]string{
				"kubernetes.io/service-name": "test-svc",
			},
		},
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{Port: &port},
		},
	}

	client := fake.NewSimpleClientset(slice)

	tmpDir := t.TempDir()
	servicesPath := filepath.Join(tmpDir, "services.json")
	backendsPath := filepath.Join(tmpDir, "backends.conf")

	// Create services.json
	servicesContent := `{"services": [{"name": "test-svc", "port": 8080}]}`
	if err := os.WriteFile(servicesPath, []byte(servicesContent), 0644); err != nil {
		t.Fatal(err)
	}

	w := NewWatcher(client, servicesPath, backendsPath, "default", logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- w.Run(ctx)
	}()

	// Wait for initial sync and file generation
	time.Sleep(500 * time.Millisecond)

	// Verify backends.conf was created
	content, err := os.ReadFile(backendsPath)
	if err != nil {
		t.Fatalf("backends.conf not created: %v", err)
	}

	if !strings.Contains(string(content), "[test-svc]") {
		t.Error("backends.conf missing [test-svc] section")
	}

	if !strings.Contains(string(content), "10.0.0.1:8080") {
		t.Error("backends.conf missing endpoint 10.0.0.1:8080")
	}

	// Clean shutdown
	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Errorf("Run() returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run() did not exit within timeout")
	}
}
