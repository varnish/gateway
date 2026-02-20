package ghost

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// TestWatcherReloadTransientFailure verifies that a transient reload failure is retried
// and the watcher does NOT exit when the retry succeeds.
func TestWatcherReloadTransientFailure(t *testing.T) {
	// Create a fake HTTP server that fails once then succeeds
	var reloadCount int
	reloadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.varnish-ghost/reload" {
			reloadCount++
			if reloadCount <= 2 {
				// First two reloads fail: initial sync + first attempt from endpoint change
				// (initial sync is not retried, so we need the endpoint-triggered one to fail once)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
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

	// Context long enough for retries
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Create varnish ready channel
	varnishReady := make(chan struct{})
	close(varnishReady)

	// Start watcher
	errCh := make(chan error, 1)
	go func() {
		errCh <- watcher.Run(ctx, varnishReady)
	}()

	// Wait for initial sync (which will fail the reload, but initial failure is fatal from Run itself)
	// The initial reload in Run() is NOT retried (it returns error directly).
	// We need to make initial succeed then subsequent fail-then-succeed.
	// Actually, reloadCount=1 is the initial sync → fails → Run returns error.
	// Let me adjust: make the server succeed first, then fail once, then succeed.

	// Cancel and redo with better server logic
	cancel()
	<-errCh

	reloadCount = 0
	reloadServer2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer reloadServer2.Close()

	varnishAddr2 := strings.TrimPrefix(reloadServer2.URL, "http://")
	watcher2 := NewWatcher(
		fake.NewSimpleClientset(configMap),
		ghostConfigPath,
		varnishAddr2,
		"default",
		"test-configmap",
		logger,
	)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel2()

	varnishReady2 := make(chan struct{})
	close(varnishReady2)

	errCh2 := make(chan error, 1)
	go func() {
		errCh2 <- watcher2.Run(ctx2, varnishReady2)
	}()

	// Wait for ready
	select {
	case <-watcher2.Ready():
		t.Log("watcher is ready")
	case err := <-errCh2:
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

	_, err = fake.NewSimpleClientset(configMap).DiscoveryV1().EndpointSlices("default").Create(ctx2, endpointSlice, metav1.CreateOptions{})
	// Directly trigger endpoint update on the watcher
	watcher2.handleEndpointSliceUpdate(ctx2, endpointSlice)

	// Wait for retry to complete (500ms backoff + processing)
	time.Sleep(2 * time.Second)

	// Watcher should still be running (not exited with fatal error)
	select {
	case err := <-errCh2:
		if err != context.DeadlineExceeded && err != context.Canceled {
			t.Fatalf("watcher exited unexpectedly: %v", err)
		}
	default:
		// Good - watcher is still running
		t.Log("watcher still running after transient failure recovery - test passed")
		cancel2()
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
