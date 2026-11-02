package ghost

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestDiffEndpoints(t *testing.T) {
	tests := []struct {
		name         string
		oldEndpoints []Endpoint
		newEndpoints []Endpoint
		wantAdded    int
		wantRemoved  int
	}{
		{
			name:         "empty old and new",
			oldEndpoints: []Endpoint{},
			newEndpoints: []Endpoint{},
			wantAdded:    0,
			wantRemoved:  0,
		},
		{
			name:         "nil old and new",
			oldEndpoints: nil,
			newEndpoints: nil,
			wantAdded:    0,
			wantRemoved:  0,
		},
		{
			name:         "empty old, non-empty new",
			oldEndpoints: []Endpoint{},
			newEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080},
				{IP: "10.0.0.2", Port: 8080},
			},
			wantAdded:   2,
			wantRemoved: 0,
		},
		{
			name: "non-empty old, empty new",
			oldEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080},
				{IP: "10.0.0.2", Port: 8080},
			},
			newEndpoints: []Endpoint{},
			wantAdded:    0,
			wantRemoved:  2,
		},
		{
			name: "same endpoints no changes",
			oldEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080},
				{IP: "10.0.0.2", Port: 8080},
			},
			newEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080},
				{IP: "10.0.0.2", Port: 8080},
			},
			wantAdded:   0,
			wantRemoved: 0,
		},
		{
			name: "some added some removed",
			oldEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080},
				{IP: "10.0.0.2", Port: 8080},
			},
			newEndpoints: []Endpoint{
				{IP: "10.0.0.2", Port: 8080},
				{IP: "10.0.0.3", Port: 8080},
			},
			wantAdded:   1, // 10.0.0.3
			wantRemoved: 1, // 10.0.0.1
		},
		{
			name: "port change counts as add and remove",
			oldEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080},
			},
			newEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 9090},
			},
			wantAdded:   1,
			wantRemoved: 1,
		},
		{
			name: "all replaced",
			oldEndpoints: []Endpoint{
				{IP: "10.0.0.1", Port: 8080},
				{IP: "10.0.0.2", Port: 8080},
			},
			newEndpoints: []Endpoint{
				{IP: "10.0.0.3", Port: 8080},
				{IP: "10.0.0.4", Port: 8080},
			},
			wantAdded:   2,
			wantRemoved: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			added, removed := diffEndpoints(tt.oldEndpoints, tt.newEndpoints)
			if len(added) != tt.wantAdded {
				t.Errorf("added: got %d, want %d", len(added), tt.wantAdded)
			}
			if len(removed) != tt.wantRemoved {
				t.Errorf("removed: got %d, want %d", len(removed), tt.wantRemoved)
			}
		})
	}
}

func TestDiffEndpointsContent(t *testing.T) {
	old := []Endpoint{
		{IP: "10.0.0.1", Port: 8080},
		{IP: "10.0.0.2", Port: 8080},
	}
	new := []Endpoint{
		{IP: "10.0.0.2", Port: 8080},
		{IP: "10.0.0.3", Port: 8080},
	}

	added, removed := diffEndpoints(old, new)

	// Check added contains the right endpoint
	if len(added) != 1 {
		t.Fatalf("expected 1 added, got %d", len(added))
	}
	if added[0].IP != "10.0.0.3" || added[0].Port != 8080 {
		t.Errorf("unexpected added endpoint: %v", added[0])
	}

	// Check removed contains the right endpoint
	if len(removed) != 1 {
		t.Fatalf("expected 1 removed, got %d", len(removed))
	}
	if removed[0].IP != "10.0.0.1" || removed[0].Port != 8080 {
		t.Errorf("unexpected removed endpoint: %v", removed[0])
	}
}

func TestExtractEndpoints(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }
	int32Ptr := func(i int32) *int32 { return &i }

	tests := []struct {
		name      string
		slice     *discoveryv1.EndpointSlice
		wantCount int
	}{
		{
			name: "single ready endpoint single address",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				},
				Ports: []discoveryv1.EndpointPort{
					{Port: int32Ptr(8080)},
				},
			},
			wantCount: 1,
		},
		{
			name: "single endpoint multiple addresses",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				},
				Ports: []discoveryv1.EndpointPort{
					{Port: int32Ptr(8080)},
				},
			},
			wantCount: 3,
		},
		{
			name: "multiple endpoints mixed ready states",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
					{
						Addresses:  []string{"10.0.0.2"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(false)},
					},
					{
						Addresses:  []string{"10.0.0.3"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				},
				Ports: []discoveryv1.EndpointPort{
					{Port: int32Ptr(8080)},
				},
			},
			wantCount: 2, // only ready ones
		},
		{
			name: "endpoint with nil ready treated as ready",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: nil},
					},
				},
				Ports: []discoveryv1.EndpointPort{
					{Port: int32Ptr(8080)},
				},
			},
			wantCount: 1,
		},
		{
			name: "empty endpoints list",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{},
				Ports: []discoveryv1.EndpointPort{
					{Port: int32Ptr(8080)},
				},
			},
			wantCount: 0,
		},
		{
			name: "no ports defaults to port 0",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				},
				Ports: []discoveryv1.EndpointPort{},
			},
			wantCount: 1,
		},
		{
			name: "nil port value defaults to port 0",
			slice: &discoveryv1.EndpointSlice{
				Endpoints: []discoveryv1.Endpoint{
					{
						Addresses:  []string{"10.0.0.1"},
						Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
					},
				},
				Ports: []discoveryv1.EndpointPort{
					{Port: nil},
				},
			},
			wantCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoints := extractEndpoints(tt.slice)
			if len(endpoints) != tt.wantCount {
				t.Errorf("got %d endpoints, want %d", len(endpoints), tt.wantCount)
			}
		})
	}
}

func TestExtractEndpointsPortValue(t *testing.T) {
	int32Ptr := func(i int32) *int32 { return &i }
	boolPtr := func(b bool) *bool { return &b }

	slice := &discoveryv1.EndpointSlice{
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{Port: int32Ptr(9090)},
		},
	}

	endpoints := extractEndpoints(slice)
	if len(endpoints) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(endpoints))
	}
	if endpoints[0].Port != 9090 {
		t.Errorf("expected port 9090, got %d", endpoints[0].Port)
	}
	if endpoints[0].IP != "10.0.0.1" {
		t.Errorf("expected IP 10.0.0.1, got %s", endpoints[0].IP)
	}
}

func TestExtractEndpointsNoPorts(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	slice := &discoveryv1.EndpointSlice{
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
			},
		},
		Ports: []discoveryv1.EndpointPort{},
	}

	endpoints := extractEndpoints(slice)
	if len(endpoints) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(endpoints))
	}
	if endpoints[0].Port != 0 {
		t.Errorf("expected port 0 (default), got %d", endpoints[0].Port)
	}
}

func TestExtractEndpointsMultiPort(t *testing.T) {
	int32Ptr := func(i int32) *int32 { return &i }
	boolPtr := func(b bool) *bool { return &b }

	slice := &discoveryv1.EndpointSlice{
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1", "10.0.0.2"},
				Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{Port: int32Ptr(8080)},
			{Port: int32Ptr(9090)},
		},
	}

	endpoints := extractEndpoints(slice)
	// 2 addresses * 2 ports = 4 endpoints
	if len(endpoints) != 4 {
		t.Fatalf("expected 4 endpoints, got %d", len(endpoints))
	}

	// Verify we get all combinations
	portCounts := make(map[int]int)
	for _, ep := range endpoints {
		portCounts[ep.Port]++
	}
	if portCounts[8080] != 2 {
		t.Errorf("expected 2 endpoints on port 8080, got %d", portCounts[8080])
	}
	if portCounts[9090] != 2 {
		t.Errorf("expected 2 endpoints on port 9090, got %d", portCounts[9090])
	}
}

func TestExtractEndpointsWithPortNames(t *testing.T) {
	int32Ptr := func(i int32) *int32 { return &i }
	strPtr := func(s string) *string { return &s }
	boolPtr := func(b bool) *bool { return &b }

	slice := &discoveryv1.EndpointSlice{
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{Name: strPtr("http"), Port: int32Ptr(80)},
			{Name: strPtr("https"), Port: int32Ptr(443)},
		},
	}

	endpoints := extractEndpoints(slice)
	if len(endpoints) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(endpoints))
	}

	// Verify port names are captured
	namesByPort := make(map[int]string)
	for _, ep := range endpoints {
		namesByPort[ep.Port] = ep.PortName
	}
	if namesByPort[80] != "http" {
		t.Errorf("expected port 80 name 'http', got %q", namesByPort[80])
	}
	if namesByPort[443] != "https" {
		t.Errorf("expected port 443 name 'https', got %q", namesByPort[443])
	}
}

// TestWatcherReloadFailureFatal verifies that ghost reload failures cause the watcher to exit
func TestWatcherReloadFailureFatal(t *testing.T) {
	// Create a fake HTTP server that returns 503 for reload requests
	reloadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.varnish-ghost/reload" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer reloadServer.Close()

	// Create temp directory for ghost.json
	tmpDir := t.TempDir()
	ghostConfigPath := filepath.Join(tmpDir, "ghost.json")

	// Prepare routing config data
	routingConfig := &RoutingConfig{
		Version: 2,
		VHosts: map[string]VHostRouting{
			"test.example.com": {
				Routes: []Route{
					{
						PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/"},
						Service:   "test-service",
						Namespace: "default",
						Port:      8080,
						Weight:    100,
						Priority:  100,
					},
				},
			},
		},
	}
	data, err := json.Marshal(routingConfig)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// Create ConfigMap with routing.json
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-configmap",
			Namespace: "default",
		},
		Data: map[string]string{
			"routing.json": string(data),
		},
	}

	// Create fake Kubernetes client with ConfigMap
	client := fake.NewSimpleClientset(configMap)

	// Create watcher pointing at our fake server
	// Extract host:port from the test server URL (strip http://)
	varnishAddr := strings.TrimPrefix(reloadServer.URL, "http://")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	watcher := NewWatcher(
		client,
		ghostConfigPath,
		varnishAddr,
		"default",
		"test-configmap",
		logger,
	)

	// Create a context with timeout - long enough for retry backoff (~3.5s) plus margin
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Create varnish ready channel that closes immediately
	varnishReady := make(chan struct{})
	close(varnishReady)

	// Start watcher in goroutine
	errCh := make(chan error, 1)
	go func() {
		errCh <- watcher.Run(ctx, varnishReady)
	}()

	// Wait a bit for initial sync to complete
	time.Sleep(100 * time.Millisecond)

	// Create an EndpointSlice that will trigger a reload
	int32Ptr := func(i int32) *int32 { return &i }
	boolPtr := func(b bool) *bool { return &b }

	endpointSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service-abc",
			Namespace: "default",
			Labels: map[string]string{
				"kubernetes.io/service-name": "test-service",
			},
		},
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{Port: int32Ptr(8080)},
		},
	}

	// Add the endpoint slice - this should trigger a reload which will fail
	_, err = client.DiscoveryV1().EndpointSlices("default").Create(ctx, endpointSlice, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create EndpointSlice: %v", err)
	}

	// Wait for the watcher to exit with an error
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected watcher to exit with error, got nil")
		}
		if err == context.DeadlineExceeded || err == context.Canceled {
			t.Fatal("watcher exited due to context timeout/cancel, not reload failure")
		}
		// Verify the error is related to ghost reload
		errStr := err.Error()
		if !strings.Contains(errStr, "ghost reload") && !strings.Contains(errStr, "503") {
			t.Errorf("expected error to mention 'ghost reload' or '503', got: %v", err)
		}
		t.Logf("watcher exited with expected error: %v", err)

	case <-time.After(10 * time.Second):
		t.Fatal("watcher did not exit within 10 seconds after reload failure (includes retry backoff)")
	}
}

// TestWatcherRunCancelledBeforeVarnishReady verifies that Run returns cleanly when
// the context is cancelled while still waiting for varnish to become ready.
func TestWatcherRunCancelledBeforeVarnishReady(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	watcher := NewWatcher(
		fake.NewSimpleClientset(),
		filepath.Join(t.TempDir(), "ghost.json"),
		"127.0.0.1:1",
		"default",
		"test-configmap",
		logger,
	)

	// Never closed, so ctx.Done() is the only ready case in Run()'s wait on
	// varnish readiness. Closing it as well would leave the select picking at
	// random, which is how this path used to be reached — intermittently, and
	// only as a side effect of another test's teardown.
	varnishReady := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := watcher.Run(ctx, varnishReady)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
}

// TestWatcherReloadTransientFailure verifies that a transient reload failure is retried
// and the watcher does NOT exit when the retry succeeds.
func TestWatcherReloadTransientFailure(t *testing.T) {
	var reloadCount int

	// Create temp directory for ghost.json
	tmpDir := t.TempDir()
	ghostConfigPath := filepath.Join(tmpDir, "ghost.json")

	// Prepare routing config data
	routingConfig := &RoutingConfig{
		Version: 2,
		VHosts: map[string]VHostRouting{
			"test.example.com": {
				Routes: []Route{
					{
						PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/"},
						Service:   "test-service",
						Namespace: "default",
						Port:      8080,
						Weight:    100,
						Priority:  100,
					},
				},
			},
		},
	}
	data, err := json.Marshal(routingConfig)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// Create ConfigMap with routing.json
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-configmap",
			Namespace: "default",
		},
		Data: map[string]string{
			"routing.json": string(data),
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// The initial reload in Run() is not retried — a failure there is returned
	// directly — so the server has to succeed on the initial sync and fail only
	// on the reload the EndpointSlice below triggers.
	reloadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.varnish-ghost/reload" {
			reloadCount++
			if reloadCount == 2 {
				// Second reload (first from endpoint change) fails
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			// First (initial sync) and third+ (retry) succeed
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer reloadServer.Close()

	varnishAddr := strings.TrimPrefix(reloadServer.URL, "http://")
	watcher := NewWatcher(
		fake.NewSimpleClientset(configMap),
		ghostConfigPath,
		varnishAddr,
		"default",
		"test-configmap",
		logger,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	varnishReady := make(chan struct{})
	close(varnishReady)

	errCh := make(chan error, 1)
	go func() {
		errCh <- watcher.Run(ctx, varnishReady)
	}()

	// Wait for ready
	select {
	case <-watcher.Ready():
		t.Log("watcher is ready")
	case err := <-errCh:
		t.Fatalf("watcher exited before becoming ready: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not become ready within 3 seconds")
	}

	// Create an EndpointSlice that will trigger a reload (which fails once, then retries succeed)
	int32Ptr := func(i int32) *int32 { return &i }
	boolPtr := func(b bool) *bool { return &b }

	endpointSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service-abc",
			Namespace: "default",
			Labels: map[string]string{
				"kubernetes.io/service-name": "test-service",
			},
		},
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{Port: int32Ptr(8080)},
		},
	}

	// Directly trigger endpoint update on the watcher
	watcher.handleEndpointSliceUpdate(ctx, endpointSlice)

	// Wait for retry to complete (500ms backoff + processing)
	time.Sleep(2 * time.Second)

	// Watcher should still be running (not exited with fatal error)
	select {
	case err := <-errCh:
		if err != context.DeadlineExceeded && err != context.Canceled {
			t.Fatalf("watcher exited unexpectedly: %v", err)
		}
	default:
		// Good - watcher is still running
		t.Log("watcher still running after transient failure recovery - test passed")
		cancel()
	}
}

// TestWatcherReloadSuccessDoesNotExit verifies that successful reloads don't cause exit
func TestWatcherReloadSuccessDoesNotExit(t *testing.T) {
	// Create a fake HTTP server that returns 200 for reload requests
	reloadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.varnish-ghost/reload" {
			w.Header().Set("x-ghost-reload", "success")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer reloadServer.Close()

	// Create temp directory for ghost.json
	tmpDir := t.TempDir()
	ghostConfigPath := filepath.Join(tmpDir, "ghost.json")

	// Prepare routing config data
	routingConfig := &RoutingConfig{
		Version: 2,
		VHosts: map[string]VHostRouting{
			"test.example.com": {
				Routes: []Route{
					{
						PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/"},
						Service:   "test-service",
						Namespace: "default",
						Port:      8080,
						Weight:    100,
						Priority:  100,
					},
				},
			},
		},
	}
	data, err := json.Marshal(routingConfig)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// Create ConfigMap with routing.json
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-configmap",
			Namespace: "default",
		},
		Data: map[string]string{
			"routing.json": string(data),
		},
	}

	// Create fake Kubernetes client with ConfigMap
	client := fake.NewSimpleClientset(configMap)

	// Create watcher
	varnishAddr := strings.TrimPrefix(reloadServer.URL, "http://")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	watcher := NewWatcher(
		client,
		ghostConfigPath,
		varnishAddr,
		"default",
		"test-configmap",
		logger,
	)

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Create varnish ready channel
	varnishReady := make(chan struct{})
	close(varnishReady)

	// Start watcher
	errCh := make(chan error, 1)
	go func() {
		errCh <- watcher.Run(ctx, varnishReady)
	}()

	// Wait for ready signal
	select {
	case <-watcher.Ready():
		t.Log("watcher is ready")
	case <-time.After(1 * time.Second):
		t.Fatal("watcher did not become ready within 1 second")
	}

	// Create an endpoint slice - this should trigger a successful reload
	int32Ptr := func(i int32) *int32 { return &i }
	boolPtr := func(b bool) *bool { return &b }

	endpointSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service-abc",
			Namespace: "default",
			Labels: map[string]string{
				"kubernetes.io/service-name": "test-service",
			},
		},
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{"10.0.0.1"},
				Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(true)},
			},
		},
		Ports: []discoveryv1.EndpointPort{
			{Port: int32Ptr(8080)},
		},
	}

	_, err = client.DiscoveryV1().EndpointSlices("default").Create(ctx, endpointSlice, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create EndpointSlice: %v", err)
	}

	// Wait a bit to ensure reload happens
	time.Sleep(200 * time.Millisecond)

	// Watcher should still be running - check that errCh is empty
	select {
	case err := <-errCh:
		// Should only exit on context cancel
		if err != context.DeadlineExceeded && err != context.Canceled {
			t.Fatalf("watcher exited unexpectedly with error: %v", err)
		}
		t.Log("watcher exited as expected on context cancel")
	case <-time.After(500 * time.Millisecond):
		// Good - watcher is still running
		t.Log("watcher still running after successful reload - test passed")
		cancel() // Clean up
	}
}

// routingConfigMap builds the watcher's ConfigMap with a single-route
// routing.json for hostname.
func routingConfigMap(t *testing.T, hostname string) *corev1.ConfigMap {
	t.Helper()
	data, err := json.Marshal(&RoutingConfig{
		Version: 2,
		VHosts: map[string]VHostRouting{
			hostname: {
				Routes: []Route{{
					PathMatch: &PathMatch{Type: PathMatchPathPrefix, Value: "/"},
					Service:   "test-service",
					Namespace: "default",
					Port:      8080,
					Weight:    100,
					Priority:  100,
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-configmap", Namespace: "default"},
		Data:       map[string]string{"routing.json": string(data)},
	}
}

// TestWatcherRetryPicksUpCorrectedConfigMap reproduces issue #104 on the
// chaperone side: a ConfigMap that ghost refuses to load is followed by a
// corrected one while chaperone is still retrying. The ConfigMap informer
// callback must not be blocked by the retries, and the next retry must use
// the corrected routing config instead of the stale one, so the watcher
// recovers rather than exiting.
func TestWatcherRetryPicksUpCorrectedConfigMap(t *testing.T) {
	ghostConfigPath := filepath.Join(t.TempDir(), "ghost.json")
	client := fake.NewSimpleClientset(routingConfigMap(t, "good.example.com"))

	var badReloads, goodReloadsAfterFix atomic.Int32
	var fixed atomic.Bool
	recovered := make(chan struct{})
	reloadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.varnish-ghost/reload" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		content, err := os.ReadFile(ghostConfigPath)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if strings.Contains(string(content), "bad.example.com") {
			// Ghost rejects this config. On the first rejection, fix the
			// ConfigMap; the informer has to deliver it during the backoff.
			if badReloads.Add(1) == 1 {
				fix := routingConfigMap(t, "fixed.example.com")
				if _, err := client.CoreV1().ConfigMaps("default").Update(
					context.Background(), fix, metav1.UpdateOptions{}); err != nil {
					t.Errorf("ConfigMaps.Update: %v", err)
				}
				fixed.Store(true)
			}
			w.Header().Set("x-ghost-error", "invalid regex")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if fixed.Load() && strings.Contains(string(content), "fixed.example.com") {
			if goodReloadsAfterFix.Add(1) == 1 {
				close(recovered)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer reloadServer.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	watcher := NewWatcher(client, ghostConfigPath, strings.TrimPrefix(reloadServer.URL, "http://"),
		"default", "test-configmap", logger)
	// Three retries spanning 3s: before the fix, the corrected ConfigMap was
	// only processed after the blocked callback gave up and went fatal.
	watcher.retryBackoffs = []time.Duration{time.Second, time.Second, time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	varnishReady := make(chan struct{})
	close(varnishReady)
	errCh := make(chan error, 1)
	go func() { errCh <- watcher.Run(ctx, varnishReady) }()

	select {
	case <-watcher.Ready():
	case err := <-errCh:
		t.Fatalf("watcher exited before becoming ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not become ready")
	}

	// Push the config ghost will reject.
	if _, err := client.CoreV1().ConfigMaps("default").Update(
		ctx, routingConfigMap(t, "bad.example.com"), metav1.UpdateOptions{}); err != nil {
		t.Fatalf("ConfigMaps.Update: %v", err)
	}

	select {
	case <-recovered:
	case err := <-errCh:
		t.Fatalf("watcher exited instead of picking up the corrected ConfigMap: %v", err)
	case <-time.After(2500 * time.Millisecond):
		t.Fatalf("corrected ConfigMap not reloaded within one backoff (bad reloads: %d)", badReloads.Load())
	}
	if n := badReloads.Load(); n != 1 {
		t.Errorf("expected exactly one rejected reload before recovery, got %d", n)
	}

	// The watcher must still be running.
	select {
	case err := <-errCh:
		t.Fatalf("watcher exited after recovery: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestRegenerateRetryBudgetResetsOnRoutingChange verifies that a routing
// config change during the retry backoff restarts the retry budget, while a
// config that keeps failing still ends in a fatal error.
func TestRegenerateRetryBudgetResetsOnRoutingChange(t *testing.T) {
	var reloads atomic.Int32
	var watcher *Watcher
	reloadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Replace the routing config during the first attempt, as an
		// informer callback would while the retry loop is backing off.
		if reloads.Add(1) == 1 {
			watcher.handleConfigMapUpdate(context.Background(), routingConfigMap(t, "second.example.com"))
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer reloadServer.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	watcher = NewWatcher(fake.NewSimpleClientset(), filepath.Join(t.TempDir(), "ghost.json"),
		strings.TrimPrefix(reloadServer.URL, "http://"), "default", "test-configmap", logger)
	watcher.retryBackoffs = []time.Duration{50 * time.Millisecond}
	watcher.handleConfigMapUpdate(context.Background(), routingConfigMap(t, "first.example.com"))
	watcher.mu.Lock()
	watcher.initialSyncDone = true
	watcher.mu.Unlock()

	watcher.regenerateConfigWithRetry(context.Background())

	// Without a change: 1 attempt + 1 retry. The change during the first
	// backoff grants the new config its own attempt + retry: 3 in total.
	if n := reloads.Load(); n != 3 {
		t.Errorf("expected 3 reload attempts, got %d", n)
	}
	select {
	case err := <-watcher.fatalErrCh:
		if err == nil || !strings.Contains(err.Error(), "ghost reload failed") {
			t.Errorf("unexpected fatal error: %v", err)
		}
	default:
		t.Fatal("expected a fatal error once the retry budget was exhausted")
	}
}

// TestRequestRegenerateNeverBlocks verifies that informer callbacks only
// enqueue regeneration (coalescing duplicates) instead of running it inline.
func TestRequestRegenerateNeverBlocks(t *testing.T) {
	watcher := NewWatcher(fake.NewSimpleClientset(), filepath.Join(t.TempDir(), "ghost.json"),
		"127.0.0.1:1", "default", "test-configmap", nil)
	watcher.handleConfigMapUpdate(context.Background(), routingConfigMap(t, "a.example.com"))
	watcher.mu.Lock()
	watcher.initialSyncDone = true
	watcher.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// No worker is running, so any inline reload attempt against the
		// unreachable address would retry for seconds.
		watcher.handleConfigMapUpdate(context.Background(), routingConfigMap(t, "b.example.com"))
		watcher.handleConfigMapUpdate(context.Background(), routingConfigMap(t, "c.example.com"))
		watcher.requestRegenerate()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ConfigMap update handling blocked")
	}
	if n := len(watcher.regenCh); n != 1 {
		t.Errorf("expected requests to coalesce into 1 pending, got %d", n)
	}
	watcher.mu.RLock()
	_, latest := watcher.routingConfig.VHosts["c.example.com"]
	watcher.mu.RUnlock()
	if !latest {
		t.Error("expected the latest ConfigMap to be the current routing config")
	}
}
