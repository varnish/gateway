package invalidation

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestWatcher(varnishAddr, gatewayName, namespace, podName string) *Watcher {
	return &Watcher{
		varnishAddr: varnishAddr,
		gatewayName: gatewayName,
		namespace:   namespace,
		podName:     podName,
		logger:      testLogger(),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		processed: make(map[string]struct{}),
	}
}

// newTestWatcherWithK8s creates a watcher with a fake k8s client (for tests
// that call handleInvalidation, which needs k8sClient for computePhase).
func newTestWatcherWithK8s(varnishAddr, gatewayName, namespace, podName string) *Watcher {
	w := newTestWatcher(varnishAddr, gatewayName, namespace, podName)
	w.k8sClient = fake.NewSimpleClientset()
	return w
}

// --- executePurge tests ---

func TestExecutePurge_Success(t *testing.T) {
	var gotMethod, gotHost, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotHost = r.Host
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	w := newTestWatcher(addr, "my-gw", "default", "pod-0")

	err := w.executePurge(context.Background(), "example.com", "/images/logo.png")
	if err != nil {
		t.Fatalf("executePurge returned error: %v", err)
	}
	if gotMethod != "PURGE" {
		t.Errorf("method: got %q, want PURGE", gotMethod)
	}
	if gotHost != "example.com" {
		t.Errorf("Host header: got %q, want example.com", gotHost)
	}
	if gotPath != "/images/logo.png" {
		t.Errorf("path: got %q, want /images/logo.png", gotPath)
	}
}

func TestExecutePurge_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	w := newTestWatcher(addr, "my-gw", "default", "pod-0")

	err := w.executePurge(context.Background(), "example.com", "/foo")
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention 500: %v", err)
	}
}

// --- executeBan tests ---

func TestExecuteBan_Success(t *testing.T) {
	var gotMethod, gotHost, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotHost = r.Host
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	w := newTestWatcher(addr, "my-gw", "default", "pod-0")

	err := w.executeBan(context.Background(), "api.example.com", "/v1/.*")
	if err != nil {
		t.Fatalf("executeBan returned error: %v", err)
	}
	if gotMethod != "BAN" {
		t.Errorf("method: got %q, want BAN", gotMethod)
	}
	if gotHost != "api.example.com" {
		t.Errorf("Host header: got %q, want api.example.com", gotHost)
	}
	if gotPath != "/v1/.*" {
		t.Errorf("path: got %q, want /v1/.*", gotPath)
	}
}

func TestExecuteBan_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	w := newTestWatcher(addr, "my-gw", "default", "pod-0")

	err := w.executeBan(context.Background(), "example.com", "/foo")
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention 500: %v", err)
	}
}

// --- podAlreadyReported tests ---

func TestPodAlreadyReported(t *testing.T) {
	w := newTestWatcher("localhost:80", "my-gw", "default", "pod-0")

	tests := []struct {
		name       string
		podResults []any
		want       bool
	}{
		{
			name:       "no podResults field",
			podResults: nil,
			want:       false,
		},
		{
			name:       "empty podResults",
			podResults: []any{},
			want:       false,
		},
		{
			name: "other pod reported",
			podResults: []any{
				map[string]any{"podName": "pod-1", "success": true},
			},
			want: false,
		},
		{
			name: "this pod already reported",
			podResults: []any{
				map[string]any{"podName": "pod-0", "success": true},
			},
			want: true,
		},
		{
			name: "this pod among several",
			podResults: []any{
				map[string]any{"podName": "pod-1", "success": true},
				map[string]any{"podName": "pod-0", "success": false},
				map[string]any{"podName": "pod-2", "success": true},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "gateway.varnish-software.com/v1alpha1",
					"kind":       "VarnishCacheInvalidation",
					"metadata":   map[string]any{"name": "test", "namespace": "default"},
				},
			}
			if tt.podResults != nil {
				_ = unstructured.SetNestedSlice(obj.Object, tt.podResults, "status", "podResults")
			}

			got := w.podAlreadyReported(obj)
			if got != tt.want {
				t.Errorf("podAlreadyReported() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- handleInvalidation: gatewayRef filtering ---

func TestHandleInvalidation_GatewayRefFiltering(t *testing.T) {
	requestReceived := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReceived = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	w := newTestWatcher(addr, "my-gw", "default", "pod-0")

	// VarnishCacheInvalidation targeting a different gateway
	obj := makeVarnishCacheInvalidation("inv-1", "default", "uid-wrong-gw", "purge", "example.com", "/foo", "other-gateway", "default")

	w.handleInvalidation(context.Background(), obj)

	if requestReceived {
		t.Error("expected no HTTP request for mismatched gatewayRef, but one was sent")
	}

	// Also verify it was not added to processed map
	w.mu.Lock()
	_, inProcessed := w.processed["uid-wrong-gw"]
	w.mu.Unlock()
	if inProcessed {
		t.Error("mismatched gatewayRef should not be added to processed map")
	}
}

func TestHandleInvalidation_GatewayRefNamespaceDefault(t *testing.T) {
	// When gatewayRef.namespace is empty, it should default to the resource namespace.
	requestReceived := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReceived = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	w := newTestWatcherWithK8s(addr, "my-gw", "test-ns", "pod-0")

	// gatewayRef without explicit namespace - should default to resource namespace "test-ns"
	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "gateway.varnish-software.com/v1alpha1",
			"kind":       "VarnishCacheInvalidation",
			"metadata": map[string]any{
				"name":      "inv-2",
				"namespace": "test-ns",
				"uid":       "uid-ns-default",
			},
			"spec": map[string]any{
				"type":     "purge",
				"hostname": "example.com",
				"path":     "/bar",
				"gatewayRef": map[string]any{
					"name": "my-gw",
					// namespace omitted - should default to "test-ns"
				},
			},
		},
	}

	w.handleInvalidation(context.Background(), obj)

	if !requestReceived {
		t.Error("expected HTTP request when gatewayRef namespace defaults to resource namespace")
	}
}

// --- handleInvalidation: already processed ---

func TestHandleInvalidation_AlreadyProcessed(t *testing.T) {
	requestCount := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	w := newTestWatcherWithK8s(addr, "my-gw", "default", "pod-0")

	obj := makeVarnishCacheInvalidation("inv-dup", "default", "uid-123", "purge", "example.com", "/dup", "my-gw", "default")

	// First call - should send HTTP request
	w.handleInvalidation(context.Background(), obj)
	mu.Lock()
	firstCount := requestCount
	mu.Unlock()
	if firstCount != 1 {
		t.Fatalf("expected 1 request after first call, got %d", firstCount)
	}

	// Second call with same UID - should be a no-op
	w.handleInvalidation(context.Background(), obj)
	mu.Lock()
	secondCount := requestCount
	mu.Unlock()
	if secondCount != 1 {
		t.Errorf("expected still 1 request after second call (no-op), got %d", secondCount)
	}
}

// --- computePhase tests ---

func TestComputePhase(t *testing.T) {
	tests := []struct {
		name        string
		podResults  []any
		runningPods int // number of Running pods to create in fake client
		wantPhase   string
	}{
		{
			name:        "0 results, 1 expected pod",
			podResults:  []any{},
			runningPods: 1,
			wantPhase:   "InProgress",
		},
		{
			name: "1 success, 1 expected pod",
			podResults: []any{
				map[string]any{"podName": "pod-0", "success": true},
			},
			runningPods: 1,
			wantPhase:   "Complete",
		},
		{
			name: "1 success, 3 expected pods",
			podResults: []any{
				map[string]any{"podName": "pod-0", "success": true},
			},
			runningPods: 3,
			wantPhase:   "InProgress",
		},
		{
			name: "3 successes, 3 expected pods",
			podResults: []any{
				map[string]any{"podName": "pod-0", "success": true},
				map[string]any{"podName": "pod-1", "success": true},
				map[string]any{"podName": "pod-2", "success": true},
			},
			runningPods: 3,
			wantPhase:   "Complete",
		},
		{
			name: "1 failure + 2 successes, 3 expected pods",
			podResults: []any{
				map[string]any{"podName": "pod-0", "success": true},
				map[string]any{"podName": "pod-1", "success": false},
				map[string]any{"podName": "pod-2", "success": true},
			},
			runningPods: 3,
			wantPhase:   "Failed",
		},
		{
			name: "1 failure only, 3 expected pods - not all reported yet",
			podResults: []any{
				map[string]any{"podName": "pod-0", "success": false},
			},
			runningPods: 3,
			wantPhase:   "InProgress",
		},
		{
			name: "all failures, all reported",
			podResults: []any{
				map[string]any{"podName": "pod-0", "success": false},
				map[string]any{"podName": "pod-1", "success": false},
			},
			runningPods: 2,
			wantPhase:   "Failed",
		},
		{
			name: "0 running pods falls back to 1 expected, 1 success",
			podResults: []any{
				map[string]any{"podName": "pod-0", "success": true},
			},
			runningPods: 0,
			wantPhase:   "Complete",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build fake pods
			var pods []runtime.Object
			for i := 0; i < tt.runningPods; i++ {
				pods = append(pods, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-" + string(rune('0'+i)),
						Namespace: "default",
						Labels: map[string]string{
							"app.kubernetes.io/managed-by":                "varnish-gateway-operator",
							"gateway.networking.k8s.io/gateway-name":      "my-gw",
							"gateway.networking.k8s.io/gateway-namespace": "default",
						},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
					},
				})
			}

			k8sClient := fake.NewSimpleClientset(pods...)

			w := &Watcher{
				k8sClient:   k8sClient,
				gatewayName: "my-gw",
				namespace:   "default",
				podName:     "pod-0",
				logger:      testLogger(),
				processed:   make(map[string]struct{}),
			}

			got := w.computePhase(context.Background(), tt.podResults)
			if got != tt.wantPhase {
				t.Errorf("computePhase() = %q, want %q", got, tt.wantPhase)
			}
		})
	}
}

// --- getExpectedPodCount tests ---

func TestGetExpectedPodCount(t *testing.T) {
	tests := []struct {
		name      string
		pods      []runtime.Object
		wantCount int
	}{
		{
			name:      "no pods returns 1 (fallback)",
			pods:      nil,
			wantCount: 1,
		},
		{
			name: "one running pod",
			pods: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gw-pod-0", Namespace: "ns1",
						Labels: map[string]string{
							"app.kubernetes.io/managed-by":                "varnish-gateway-operator",
							"gateway.networking.k8s.io/gateway-name":      "gw1",
							"gateway.networking.k8s.io/gateway-namespace": "ns1",
						},
					},
					Status: corev1.PodStatus{Phase: corev1.PodRunning},
				},
			},
			wantCount: 1,
		},
		{
			name: "mixed phases only counts Running",
			pods: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gw-pod-0", Namespace: "ns1",
						Labels: map[string]string{
							"app.kubernetes.io/managed-by":                "varnish-gateway-operator",
							"gateway.networking.k8s.io/gateway-name":      "gw1",
							"gateway.networking.k8s.io/gateway-namespace": "ns1",
						},
					},
					Status: corev1.PodStatus{Phase: corev1.PodRunning},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gw-pod-1", Namespace: "ns1",
						Labels: map[string]string{
							"app.kubernetes.io/managed-by":                "varnish-gateway-operator",
							"gateway.networking.k8s.io/gateway-name":      "gw1",
							"gateway.networking.k8s.io/gateway-namespace": "ns1",
						},
					},
					Status: corev1.PodStatus{Phase: corev1.PodPending},
				},
			},
			wantCount: 1,
		},
		{
			name: "three running pods",
			pods: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gw-pod-0", Namespace: "ns1",
						Labels: map[string]string{
							"app.kubernetes.io/managed-by":                "varnish-gateway-operator",
							"gateway.networking.k8s.io/gateway-name":      "gw1",
							"gateway.networking.k8s.io/gateway-namespace": "ns1",
						},
					},
					Status: corev1.PodStatus{Phase: corev1.PodRunning},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gw-pod-1", Namespace: "ns1",
						Labels: map[string]string{
							"app.kubernetes.io/managed-by":                "varnish-gateway-operator",
							"gateway.networking.k8s.io/gateway-name":      "gw1",
							"gateway.networking.k8s.io/gateway-namespace": "ns1",
						},
					},
					Status: corev1.PodStatus{Phase: corev1.PodRunning},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gw-pod-2", Namespace: "ns1",
						Labels: map[string]string{
							"app.kubernetes.io/managed-by":                "varnish-gateway-operator",
							"gateway.networking.k8s.io/gateway-name":      "gw1",
							"gateway.networking.k8s.io/gateway-namespace": "ns1",
						},
					},
					Status: corev1.PodStatus{Phase: corev1.PodRunning},
				},
			},
			wantCount: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := fake.NewSimpleClientset(tt.pods...)
			w := &Watcher{
				k8sClient:   k8sClient,
				gatewayName: "gw1",
				namespace:   "ns1",
				logger:      testLogger(),
			}
			got := w.getExpectedPodCount(context.Background())
			if got != tt.wantCount {
				t.Errorf("getExpectedPodCount() = %d, want %d", got, tt.wantCount)
			}
		})
	}
}

// --- handleInvalidation: correct method dispatch ---

func TestHandleInvalidation_DispatchesPurge(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	w := newTestWatcherWithK8s(addr, "my-gw", "default", "pod-0")

	obj := makeVarnishCacheInvalidation("inv-purge", "default", "uid-purge", "purge", "example.com", "/foo", "my-gw", "default")
	w.handleInvalidation(context.Background(), obj)

	if gotMethod != "PURGE" {
		t.Errorf("expected PURGE method, got %q", gotMethod)
	}
}

func TestHandleInvalidation_DispatchesBan(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	w := newTestWatcherWithK8s(addr, "my-gw", "default", "pod-0")

	obj := makeVarnishCacheInvalidation("inv-ban", "default", "uid-ban", "ban", "example.com", "/pattern/.*", "my-gw", "default")
	w.handleInvalidation(context.Background(), obj)

	if gotMethod != "BAN" {
		t.Errorf("expected BAN method, got %q", gotMethod)
	}
}

// makeVarnishCacheInvalidation is a helper to build an unstructured VarnishCacheInvalidation object.
func makeVarnishCacheInvalidation(name, ns, uid, invType, hostname, path, gwName, gwNS string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "gateway.varnish-software.com/v1alpha1",
			"kind":       "VarnishCacheInvalidation",
			"metadata": map[string]any{
				"name":      name,
				"namespace": ns,
				"uid":       uid,
			},
			"spec": map[string]any{
				"type":     invType,
				"hostname": hostname,
				"path":     path,
				"gatewayRef": map[string]any{
					"name":      gwName,
					"namespace": gwNS,
				},
			},
		},
	}
}

// Test that the GVR constant is set correctly.
func TestVarnishCacheInvalidationGVR(t *testing.T) {
	want := schema.GroupVersionResource{
		Group:    "gateway.varnish-software.com",
		Version:  "v1alpha1",
		Resource: "varnishcacheinvalidations",
	}
	if varnishCacheInvalidationGVR != want {
		t.Errorf("varnishCacheInvalidationGVR = %v, want %v", varnishCacheInvalidationGVR, want)
	}
}
