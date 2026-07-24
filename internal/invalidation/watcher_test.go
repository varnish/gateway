package invalidation

import (
	"context"
	"fmt"
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
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

func newRunningGatewayPod(name, namespace, gatewayName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":                "varnish-gateway-operator",
				"gateway.networking.k8s.io/gateway-name":      gatewayName,
				"gateway.networking.k8s.io/gateway-namespace": namespace,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
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
	obj := makeVarnishCacheInvalidation("inv-1", "default", "uid-wrong-gw", "purge", "example.com", []string{"/foo"}, "other-gateway", "default")

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
				"paths":    []any{"/bar"},
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

	obj := makeVarnishCacheInvalidation("inv-dup", "default", "uid-123", "purge", "example.com", []string{"/dup"}, "my-gw", "default")

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

// --- updateStatus tests ---

func TestUpdateStatus_AppendsPodResultAndCompletes(t *testing.T) {
	obj := makeVarnishCacheInvalidation("inv-status", "default", "uid-status", "purge", "example.com", []string{"/ok"}, "my-gw", "default")
	dynClient := newMemoryDynamicClient(obj)
	w := newTestWatcher("localhost:80", "my-gw", "default", "pod-0")
	w.dynClient = dynClient
	w.k8sClient = fake.NewSimpleClientset(newRunningGatewayPod("pod-0", "default", "my-gw"))

	pathResults := []any{
		map[string]any{"path": "/ok", "success": true, "message": "Purge applied successfully"},
	}
	w.updateStatus(context.Background(), "default", "inv-status", true, "1/1 paths succeeded", pathResults)

	got := getStoredInvalidation(t, dynClient, "default", "inv-status")
	status := requireNestedMap(t, got.Object, "status")
	if phase := status["phase"]; phase != "Complete" {
		t.Fatalf("phase = %v, want Complete", phase)
	}
	if _, ok := status["completedAt"].(string); !ok {
		t.Fatalf("status.completedAt missing or not a string: %#v", status["completedAt"])
	}
	podResults := requireNestedSlice(t, got.Object, "status", "podResults")
	if len(podResults) != 1 {
		t.Fatalf("podResults length = %d, want 1", len(podResults))
	}
	result, ok := podResults[0].(map[string]any)
	if !ok {
		t.Fatalf("podResults[0] has type %T, want map[string]any", podResults[0])
	}
	if result["podName"] != "pod-0" {
		t.Errorf("podName = %v, want pod-0", result["podName"])
	}
	if result["success"] != true {
		t.Errorf("success = %v, want true", result["success"])
	}
	if result["message"] != "1/1 paths succeeded" {
		t.Errorf("message = %v, want 1/1 paths succeeded", result["message"])
	}
	storedPathResults, ok := result["pathResults"].([]any)
	if !ok {
		t.Fatalf("pathResults has type %T, want []any", result["pathResults"])
	}
	if len(storedPathResults) != 1 {
		t.Fatalf("pathResults length = %d, want 1", len(storedPathResults))
	}
}

func TestUpdateStatus_PreservesExistingResultsAndStaysInProgress(t *testing.T) {
	obj := makeVarnishCacheInvalidation("inv-progress", "default", "uid-progress", "purge", "example.com", []string{"/ok"}, "my-gw", "default")
	existing := []any{
		map[string]any{
			"podName":     "pod-1",
			"success":     true,
			"message":     "1/1 paths succeeded",
			"completedAt": "2026-01-01T00:00:00Z",
			"pathResults": []any{map[string]any{"path": "/ok", "success": true}},
		},
	}
	if err := unstructured.SetNestedSlice(obj.Object, existing, "status", "podResults"); err != nil {
		t.Fatalf("SetNestedSlice: %v", err)
	}

	dynClient := newMemoryDynamicClient(obj)
	w := newTestWatcher("localhost:80", "my-gw", "default", "pod-0")
	w.dynClient = dynClient
	w.k8sClient = fake.NewSimpleClientset(
		newRunningGatewayPod("pod-0", "default", "my-gw"),
		newRunningGatewayPod("pod-1", "default", "my-gw"),
		newRunningGatewayPod("pod-2", "default", "my-gw"),
	)

	w.updateStatus(context.Background(), "default", "inv-progress", true, "1/1 paths succeeded", nil)

	got := getStoredInvalidation(t, dynClient, "default", "inv-progress")
	status := requireNestedMap(t, got.Object, "status")
	if phase := status["phase"]; phase != "InProgress" {
		t.Fatalf("phase = %v, want InProgress", phase)
	}
	if _, found := status["completedAt"]; found {
		t.Fatalf("status.completedAt = %v, want absent while in progress", status["completedAt"])
	}
	podResults := requireNestedSlice(t, got.Object, "status", "podResults")
	if len(podResults) != 2 {
		t.Fatalf("podResults length = %d, want 2", len(podResults))
	}
	first := podResults[0].(map[string]any)
	second := podResults[1].(map[string]any)
	if first["podName"] != "pod-1" || second["podName"] != "pod-0" {
		t.Fatalf("pod result order/preservation = %#v, want existing pod-1 then pod-0", podResults)
	}
}

func TestUpdateStatus_FailsWhenAllPodsReportedAndAnyFailed(t *testing.T) {
	obj := makeVarnishCacheInvalidation("inv-failed", "default", "uid-failed", "purge", "example.com", []string{"/ok"}, "my-gw", "default")
	existing := []any{
		map[string]any{
			"podName":     "pod-1",
			"success":     false,
			"message":     "0/1 paths succeeded",
			"completedAt": "2026-01-01T00:00:00Z",
			"pathResults": []any{map[string]any{"path": "/ok", "success": false}},
		},
	}
	if err := unstructured.SetNestedSlice(obj.Object, existing, "status", "podResults"); err != nil {
		t.Fatalf("SetNestedSlice: %v", err)
	}

	dynClient := newMemoryDynamicClient(obj)
	w := newTestWatcher("localhost:80", "my-gw", "default", "pod-0")
	w.dynClient = dynClient
	w.k8sClient = fake.NewSimpleClientset(
		newRunningGatewayPod("pod-0", "default", "my-gw"),
		newRunningGatewayPod("pod-1", "default", "my-gw"),
	)

	w.updateStatus(context.Background(), "default", "inv-failed", true, "1/1 paths succeeded", nil)

	got := getStoredInvalidation(t, dynClient, "default", "inv-failed")
	status := requireNestedMap(t, got.Object, "status")
	if phase := status["phase"]; phase != "Failed" {
		t.Fatalf("phase = %v, want Failed", phase)
	}
	if _, ok := status["completedAt"].(string); !ok {
		t.Fatalf("status.completedAt missing or not a string: %#v", status["completedAt"])
	}
}

func TestUpdateStatus_DoesNotAppendDuplicatePodResult(t *testing.T) {
	obj := makeVarnishCacheInvalidation("inv-duplicate", "default", "uid-duplicate", "purge", "example.com", []string{"/ok"}, "my-gw", "default")
	existing := []any{
		map[string]any{
			"podName":     "pod-0",
			"success":     true,
			"message":     "1/1 paths succeeded",
			"completedAt": "2026-01-01T00:00:00Z",
		},
	}
	if err := unstructured.SetNestedSlice(obj.Object, existing, "status", "podResults"); err != nil {
		t.Fatalf("SetNestedSlice: %v", err)
	}

	dynClient := newMemoryDynamicClient(obj)
	w := newTestWatcher("localhost:80", "my-gw", "default", "pod-0")
	w.dynClient = dynClient
	w.k8sClient = fake.NewSimpleClientset(newRunningGatewayPod("pod-0", "default", "my-gw"))

	w.updateStatus(context.Background(), "default", "inv-duplicate", true, "1/1 paths succeeded", nil)

	got := getStoredInvalidation(t, dynClient, "default", "inv-duplicate")
	podResults := requireNestedSlice(t, got.Object, "status", "podResults")
	if len(podResults) != 1 {
		t.Fatalf("podResults length = %d, want 1", len(podResults))
	}
	if dynClient.updateStatusCount != 0 {
		t.Fatalf("UpdateStatus calls = %d, want 0 for duplicate pod result", dynClient.updateStatusCount)
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

	obj := makeVarnishCacheInvalidation("inv-purge", "default", "uid-purge", "purge", "example.com", []string{"/foo"}, "my-gw", "default")
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

	obj := makeVarnishCacheInvalidation("inv-ban", "default", "uid-ban", "ban", "example.com", []string{"/pattern/.*"}, "my-gw", "default")
	w.handleInvalidation(context.Background(), obj)

	if gotMethod != "BAN" {
		t.Errorf("expected BAN method, got %q", gotMethod)
	}
}

func TestHandleInvalidation_MultiplePaths(t *testing.T) {
	var mu sync.Mutex
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPaths = append(gotPaths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	w := newTestWatcherWithK8s(addr, "my-gw", "default", "pod-0")

	obj := makeVarnishCacheInvalidation("inv-multi", "default", "uid-multi", "purge", "example.com",
		[]string{"/page/1", "/page/2", "/page/3"}, "my-gw", "default")
	w.handleInvalidation(context.Background(), obj)

	mu.Lock()
	defer mu.Unlock()
	if len(gotPaths) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(gotPaths))
	}
	want := []string{"/page/1", "/page/2", "/page/3"}
	for i, p := range gotPaths {
		if p != want[i] {
			t.Errorf("path[%d] = %q, want %q", i, p, want[i])
		}
	}
}

func TestHandleInvalidation_MultiplePathsPartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	w := newTestWatcherWithK8s(addr, "my-gw", "default", "pod-0")

	obj := makeVarnishCacheInvalidation("inv-partial", "default", "uid-partial", "purge", "example.com",
		[]string{"/ok", "/fail", "/also-ok"}, "my-gw", "default")
	w.handleInvalidation(context.Background(), obj)

	// Verify it was marked as processed (meaning it ran to completion)
	w.mu.Lock()
	_, processed := w.processed["uid-partial"]
	w.mu.Unlock()
	if !processed {
		t.Error("expected invalidation to be marked as processed")
	}
}

func TestHandleInvalidation_WritesSuccessStatus(t *testing.T) {
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	obj := makeVarnishCacheInvalidation("inv-success-status", "default", "uid-success-status", "purge", "example.com",
		[]string{"/ok"}, "my-gw", "default")
	dynClient := newMemoryDynamicClient(obj)
	w := newTestWatcherWithK8s(addr, "my-gw", "default", "pod-0")
	w.dynClient = dynClient

	w.handleInvalidation(context.Background(), obj)

	if requestCount != 1 {
		t.Fatalf("request count = %d, want 1", requestCount)
	}
	got := getStoredInvalidation(t, dynClient, "default", "inv-success-status")
	status := requireNestedMap(t, got.Object, "status")
	if phase := status["phase"]; phase != "Complete" {
		t.Fatalf("phase = %v, want Complete", phase)
	}
	podResults := requireNestedSlice(t, got.Object, "status", "podResults")
	if len(podResults) != 1 {
		t.Fatalf("podResults length = %d, want 1", len(podResults))
	}
	result := podResults[0].(map[string]any)
	if result["podName"] != "pod-0" {
		t.Errorf("podName = %v, want pod-0", result["podName"])
	}
	if result["success"] != true {
		t.Errorf("success = %v, want true", result["success"])
	}
	if result["message"] != "1/1 paths succeeded" {
		t.Errorf("message = %v, want 1/1 paths succeeded", result["message"])
	}
	pathResults, ok := result["pathResults"].([]any)
	if !ok {
		t.Fatalf("pathResults has type %T, want []any", result["pathResults"])
	}
	if len(pathResults) != 1 {
		t.Fatalf("pathResults length = %d, want 1", len(pathResults))
	}
	pathResult := pathResults[0].(map[string]any)
	if pathResult["path"] != "/ok" {
		t.Errorf("path = %v, want /ok", pathResult["path"])
	}
	if pathResult["success"] != true {
		t.Errorf("path success = %v, want true", pathResult["success"])
	}
}

func TestHandleInvalidation_WritesPartialFailureStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	obj := makeVarnishCacheInvalidation("inv-failure-status", "default", "uid-failure-status", "purge", "example.com",
		[]string{"/ok", "/fail"}, "my-gw", "default")
	dynClient := newMemoryDynamicClient(obj)
	w := newTestWatcherWithK8s(addr, "my-gw", "default", "pod-0")
	w.dynClient = dynClient

	w.handleInvalidation(context.Background(), obj)

	got := getStoredInvalidation(t, dynClient, "default", "inv-failure-status")
	status := requireNestedMap(t, got.Object, "status")
	if phase := status["phase"]; phase != "Failed" {
		t.Fatalf("phase = %v, want Failed", phase)
	}
	podResults := requireNestedSlice(t, got.Object, "status", "podResults")
	if len(podResults) != 1 {
		t.Fatalf("podResults length = %d, want 1", len(podResults))
	}
	result := podResults[0].(map[string]any)
	if result["success"] != false {
		t.Errorf("success = %v, want false", result["success"])
	}
	if result["message"] != "1/2 paths succeeded" {
		t.Errorf("message = %v, want 1/2 paths succeeded", result["message"])
	}
	pathResults, ok := result["pathResults"].([]any)
	if !ok {
		t.Fatalf("pathResults has type %T, want []any", result["pathResults"])
	}
	if len(pathResults) != 2 {
		t.Fatalf("pathResults length = %d, want 2", len(pathResults))
	}
	failed := pathResults[1].(map[string]any)
	if failed["path"] != "/fail" {
		t.Errorf("failed path = %v, want /fail", failed["path"])
	}
	if failed["success"] != false {
		t.Errorf("failed path success = %v, want false", failed["success"])
	}
	msg, _ := failed["message"].(string)
	if !strings.Contains(msg, "HTTP 500") {
		t.Errorf("failure message = %q, want HTTP 500", msg)
	}
}

func TestProcessExisting_ProcessesMatchingGatewayOnly(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	matching := makeVarnishCacheInvalidation("inv-matching", "default", "uid-matching", "purge", "example.com",
		[]string{"/match"}, "my-gw", "default")
	otherGateway := makeVarnishCacheInvalidation("inv-other", "default", "uid-other", "purge", "example.com",
		[]string{"/other"}, "other-gw", "default")
	dynClient := newMemoryDynamicClient(matching, otherGateway)
	w := newTestWatcherWithK8s(addr, "my-gw", "default", "pod-0")
	w.dynClient = dynClient

	if err := w.processExisting(context.Background()); err != nil {
		t.Fatalf("processExisting returned error: %v", err)
	}

	if len(gotPaths) != 1 || gotPaths[0] != "/match" {
		t.Fatalf("got paths = %#v, want only /match", gotPaths)
	}
	gotMatching := getStoredInvalidation(t, dynClient, "default", "inv-matching")
	status := requireNestedMap(t, gotMatching.Object, "status")
	if phase := status["phase"]; phase != "Complete" {
		t.Fatalf("matching phase = %v, want Complete", phase)
	}
	gotOther := getStoredInvalidation(t, dynClient, "default", "inv-other")
	if _, found, err := unstructured.NestedMap(gotOther.Object, "status"); err != nil || found {
		t.Fatalf("other gateway status found=%v err=%v, want no status", found, err)
	}
}

// --- M-14: cross-namespace authorization ---

func TestHandleInvalidation_RejectsCrossNamespaceGatewayRef(t *testing.T) {
	requestReceived := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReceived = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")

	// CR lives in "other-ns" but targets a gateway in "default" (our namespace).
	// Even though the gatewayRef name/namespace match this chaperone's identity,
	// the CR is not in the gateway's namespace, so it must be rejected.
	obj := makeVarnishCacheInvalidation("inv-xns", "other-ns", "uid-xns", "purge", "example.com", []string{"/foo"}, "my-gw", "default")
	dynClient := newMemoryDynamicClient(obj)
	w := newTestWatcherWithK8s(addr, "my-gw", "default", "pod-0")
	w.dynClient = dynClient

	w.handleInvalidation(context.Background(), obj)

	if requestReceived {
		t.Error("expected no HTTP request for cross-namespace gatewayRef, but one was sent")
	}

	got := getStoredInvalidation(t, dynClient, "other-ns", "inv-xns")
	status := requireNestedMap(t, got.Object, "status")
	if phase := status["phase"]; phase != "Failed" {
		t.Fatalf("phase = %v, want Failed", phase)
	}
	podResults := requireNestedSlice(t, got.Object, "status", "podResults")
	if len(podResults) != 1 {
		t.Fatalf("podResults length = %d, want 1", len(podResults))
	}
	result := podResults[0].(map[string]any)
	if result["success"] != false {
		t.Errorf("success = %v, want false", result["success"])
	}
	msg, _ := result["message"].(string)
	if !strings.Contains(msg, "cross-namespace") {
		t.Errorf("message = %q, want it to mention cross-namespace", msg)
	}
}

// --- M-21: hostname/path validation ---

func TestValidateInvalidationSpec(t *testing.T) {
	tests := []struct {
		name     string
		hostname string
		paths    []string
		wantErr  bool
	}{
		{name: "valid host and path", hostname: "api.example.com", paths: []string{"/v1/users"}, wantErr: false},
		{name: "valid ban regex path", hostname: "example.com", paths: []string{"/api/.*"}, wantErr: false},
		{name: "empty hostname", hostname: "", paths: []string{"/foo"}, wantErr: true},
		{name: "hostname with quote", hostname: `x" && obj.status == 200`, paths: []string{"/foo"}, wantErr: true},
		{name: "hostname with space", hostname: "bad host", paths: []string{"/foo"}, wantErr: true},
		{name: "hostname with ampersand", hostname: "a&&b", paths: []string{"/foo"}, wantErr: true},
		{name: "no paths", hostname: "example.com", paths: nil, wantErr: true},
		{name: "path missing leading slash", hostname: "example.com", paths: []string{"foo"}, wantErr: true},
		{name: "path with space injection", hostname: "example.com", paths: []string{"/foo && obj.status == 200"}, wantErr: true},
		{name: "path with quote", hostname: "example.com", paths: []string{`/foo"`}, wantErr: true},
		{name: "one bad path among good", hostname: "example.com", paths: []string{"/ok", "bad"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateInvalidationSpec(tt.hostname, tt.paths)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateInvalidationSpec(%q, %v) err = %v, wantErr %v", tt.hostname, tt.paths, err, tt.wantErr)
			}
		})
	}
}

func TestHandleInvalidation_RejectsInvalidHostname(t *testing.T) {
	requestReceived := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReceived = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	obj := makeVarnishCacheInvalidation("inv-badhost", "default", "uid-badhost", "ban",
		`evil" && obj.http.x-cache-url ~ .`, []string{"/foo"}, "my-gw", "default")
	dynClient := newMemoryDynamicClient(obj)
	w := newTestWatcherWithK8s(addr, "my-gw", "default", "pod-0")
	w.dynClient = dynClient

	w.handleInvalidation(context.Background(), obj)

	if requestReceived {
		t.Error("expected no request for invalid hostname, but one was sent")
	}
	got := getStoredInvalidation(t, dynClient, "default", "inv-badhost")
	status := requireNestedMap(t, got.Object, "status")
	if phase := status["phase"]; phase != "Failed" {
		t.Fatalf("phase = %v, want Failed", phase)
	}
	result := requireNestedSlice(t, got.Object, "status", "podResults")[0].(map[string]any)
	msg, _ := result["message"].(string)
	if !strings.Contains(msg, "invalid cache invalidation spec") {
		t.Errorf("message = %q, want it to mention invalid spec", msg)
	}
}

func TestHandleInvalidation_RejectsInvalidPath(t *testing.T) {
	requestReceived := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestReceived = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	obj := makeVarnishCacheInvalidation("inv-badpath", "default", "uid-badpath", "purge",
		"example.com", []string{"no-leading-slash"}, "my-gw", "default")
	dynClient := newMemoryDynamicClient(obj)
	w := newTestWatcherWithK8s(addr, "my-gw", "default", "pod-0")
	w.dynClient = dynClient

	w.handleInvalidation(context.Background(), obj)

	if requestReceived {
		t.Error("expected no request for invalid path, but one was sent")
	}
	got := getStoredInvalidation(t, dynClient, "default", "inv-badpath")
	status := requireNestedMap(t, got.Object, "status")
	if phase := status["phase"]; phase != "Failed" {
		t.Fatalf("phase = %v, want Failed", phase)
	}
}

// --- M-19: PURGE of an uncached URL (404) is a success ---

func TestExecutePurge_NotInCacheIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Varnish returns 404 "Not in cache" when purging a URL that is not cached.
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	w := newTestWatcher(addr, "my-gw", "default", "pod-0")

	if err := w.executePurge(context.Background(), "example.com", "/never-cached"); err != nil {
		t.Fatalf("executePurge should treat 404 as success, got error: %v", err)
	}
}

func TestHandleInvalidation_PurgeMissReportsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // cache miss
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	obj := makeVarnishCacheInvalidation("inv-miss", "default", "uid-miss", "purge", "example.com",
		[]string{"/not-cached"}, "my-gw", "default")
	dynClient := newMemoryDynamicClient(obj)
	w := newTestWatcherWithK8s(addr, "my-gw", "default", "pod-0")
	w.dynClient = dynClient

	w.handleInvalidation(context.Background(), obj)

	got := getStoredInvalidation(t, dynClient, "default", "inv-miss")
	status := requireNestedMap(t, got.Object, "status")
	if phase := status["phase"]; phase != "Complete" {
		t.Fatalf("phase = %v, want Complete (404 purge miss is a success)", phase)
	}
}

// --- M-20: processed only recorded after a successful status write ---

func TestHandleInvalidation_NotProcessedWhenStatusUpdateFails(t *testing.T) {
	var requestCount int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	obj := makeVarnishCacheInvalidation("inv-statusfail", "default", "uid-statusfail", "purge", "example.com",
		[]string{"/foo"}, "my-gw", "default")
	dynClient := newMemoryDynamicClient(obj)
	dynClient.updateStatusErr = fmt.Errorf("apiserver unavailable")
	w := newTestWatcherWithK8s(addr, "my-gw", "default", "pod-0")
	w.dynClient = dynClient

	// First call: purge sent, but status write fails -> must NOT be processed.
	w.handleInvalidation(context.Background(), obj)
	w.mu.Lock()
	_, processed := w.processed["uid-statusfail"]
	w.mu.Unlock()
	if processed {
		t.Fatal("invalidation must not be marked processed when the status update fails")
	}

	// Second call: reprocessing must happen (idempotent purge re-sent).
	w.handleInvalidation(context.Background(), obj)
	mu.Lock()
	count := requestCount
	mu.Unlock()
	if count != 2 {
		t.Fatalf("expected reprocessing to re-send purge, request count = %d, want 2", count)
	}

	// Once the status write succeeds, it is marked processed and not retried again.
	dynClient.mu.Lock()
	dynClient.updateStatusErr = nil
	dynClient.mu.Unlock()
	w.handleInvalidation(context.Background(), obj)
	w.mu.Lock()
	_, processed = w.processed["uid-statusfail"]
	w.mu.Unlock()
	if !processed {
		t.Fatal("invalidation should be marked processed after a successful status write")
	}
}

func TestHandleInvalidation_MarkedProcessedAfterSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	obj := makeVarnishCacheInvalidation("inv-ok-proc", "default", "uid-ok-proc", "purge", "example.com",
		[]string{"/foo"}, "my-gw", "default")
	dynClient := newMemoryDynamicClient(obj)
	w := newTestWatcherWithK8s(addr, "my-gw", "default", "pod-0")
	w.dynClient = dynClient

	w.handleInvalidation(context.Background(), obj)

	w.mu.Lock()
	_, processed := w.processed["uid-ok-proc"]
	w.mu.Unlock()
	if !processed {
		t.Fatal("invalidation should be marked processed after successful status write")
	}
}

// makeVarnishCacheInvalidation is a helper to build an unstructured VarnishCacheInvalidation object.
func makeVarnishCacheInvalidation(name, ns, uid, invType, hostname string, paths []string, gwName, gwNS string) *unstructured.Unstructured {
	// Convert []string to []any for unstructured
	pathsAny := make([]any, len(paths))
	for i, p := range paths {
		pathsAny[i] = p
	}
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
				"paths":    pathsAny,
				"gatewayRef": map[string]any{
					"name":      gwName,
					"namespace": gwNS,
				},
			},
		},
	}
}

func getStoredInvalidation(t *testing.T, dynClient *memoryDynamicClient, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := dynClient.Resource(varnishCacheInvalidationGVR).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get stored invalidation %s/%s: %v", namespace, name, err)
	}
	return obj
}

func requireNestedMap(t *testing.T, obj map[string]any, fields ...string) map[string]any {
	t.Helper()
	m, found, err := unstructured.NestedMap(obj, fields...)
	if err != nil {
		t.Fatalf("NestedMap(%v): %v", fields, err)
	}
	if !found {
		t.Fatalf("NestedMap(%v) not found", fields)
	}
	return m
}

func requireNestedSlice(t *testing.T, obj map[string]any, fields ...string) []any {
	t.Helper()
	s, found, err := unstructured.NestedSlice(obj, fields...)
	if err != nil {
		t.Fatalf("NestedSlice(%v): %v", fields, err)
	}
	if !found {
		t.Fatalf("NestedSlice(%v) not found", fields)
	}
	return s
}

type memoryDynamicClient struct {
	mu                sync.Mutex
	objects           map[string]*unstructured.Unstructured
	updateStatusCount int
	updateStatusErr   error // when set, UpdateStatus fails with this error
}

var _ dynamic.Interface = (*memoryDynamicClient)(nil)

func newMemoryDynamicClient(objs ...*unstructured.Unstructured) *memoryDynamicClient {
	client := &memoryDynamicClient{objects: make(map[string]*unstructured.Unstructured)}
	for _, obj := range objs {
		client.objects[objectKey(obj.GetNamespace(), obj.GetName())] = obj.DeepCopy()
	}
	return client
}

func (c *memoryDynamicClient) Resource(resource schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &memoryDynamicResource{client: c, resource: resource}
}

type memoryDynamicResource struct {
	client    *memoryDynamicClient
	resource  schema.GroupVersionResource
	namespace string
}

var _ dynamic.NamespaceableResourceInterface = (*memoryDynamicResource)(nil)

func (r *memoryDynamicResource) Namespace(namespace string) dynamic.ResourceInterface {
	return &memoryDynamicResource{
		client:    r.client,
		resource:  r.resource,
		namespace: namespace,
	}
}

func (r *memoryDynamicResource) Create(ctx context.Context, obj *unstructured.Unstructured, options metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	return nil, fmt.Errorf("Create not implemented")
}

func (r *memoryDynamicResource) Update(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	return nil, fmt.Errorf("Update not implemented")
}

func (r *memoryDynamicResource) UpdateStatus(ctx context.Context, obj *unstructured.Unstructured, options metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	if err := r.checkResource(); err != nil {
		return nil, err
	}
	r.client.mu.Lock()
	defer r.client.mu.Unlock()
	if r.client.updateStatusErr != nil {
		return nil, r.client.updateStatusErr
	}
	key := objectKey(r.namespace, obj.GetName())
	if _, ok := r.client.objects[key]; !ok {
		return nil, fmt.Errorf("%s not found", key)
	}
	stored := obj.DeepCopy()
	stored.SetNamespace(r.namespace)
	r.client.objects[key] = stored
	r.client.updateStatusCount++
	return stored.DeepCopy(), nil
}

func (r *memoryDynamicResource) Delete(ctx context.Context, name string, options metav1.DeleteOptions, subresources ...string) error {
	return fmt.Errorf("Delete not implemented")
}

func (r *memoryDynamicResource) DeleteCollection(ctx context.Context, options metav1.DeleteOptions, listOptions metav1.ListOptions) error {
	return fmt.Errorf("DeleteCollection not implemented")
}

func (r *memoryDynamicResource) Get(ctx context.Context, name string, options metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if err := r.checkResource(); err != nil {
		return nil, err
	}
	r.client.mu.Lock()
	defer r.client.mu.Unlock()
	obj, ok := r.client.objects[objectKey(r.namespace, name)]
	if !ok {
		return nil, fmt.Errorf("%s/%s not found", r.namespace, name)
	}
	return obj.DeepCopy(), nil
}

func (r *memoryDynamicResource) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	if err := r.checkResource(); err != nil {
		return nil, err
	}
	r.client.mu.Lock()
	defer r.client.mu.Unlock()
	list := &unstructured.UnstructuredList{}
	for _, obj := range r.client.objects {
		if r.namespace != "" && obj.GetNamespace() != r.namespace {
			continue
		}
		list.Items = append(list.Items, *obj.DeepCopy())
	}
	return list, nil
}

func (r *memoryDynamicResource) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return nil, fmt.Errorf("Watch not implemented")
}

func (r *memoryDynamicResource) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	return nil, fmt.Errorf("Patch not implemented")
}

func (r *memoryDynamicResource) Apply(ctx context.Context, name string, obj *unstructured.Unstructured, options metav1.ApplyOptions, subresources ...string) (*unstructured.Unstructured, error) {
	return nil, fmt.Errorf("Apply not implemented")
}

func (r *memoryDynamicResource) ApplyStatus(ctx context.Context, name string, obj *unstructured.Unstructured, options metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	return nil, fmt.Errorf("ApplyStatus not implemented")
}

func (r *memoryDynamicResource) checkResource() error {
	if r.resource != varnishCacheInvalidationGVR {
		return fmt.Errorf("unexpected resource %s, want %s", r.resource, varnishCacheInvalidationGVR)
	}
	return nil
}

func objectKey(namespace, name string) string {
	return namespace + "/" + name
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
