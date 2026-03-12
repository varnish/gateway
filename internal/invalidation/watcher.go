// Package invalidation watches VarnishCacheInvalidation custom resources and executes
// purge/ban operations against the local Varnish instance via HTTP.
//
// Each chaperone pod independently watches for VarnishCacheInvalidation resources,
// executes the invalidation against its local Varnish, and writes its result
// to status.podResults[]. The phase transitions to Complete when all gateway
// pods have reported success.
package invalidation

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

var varnishCacheInvalidationGVR = schema.GroupVersionResource{
	Group:    "gateway.varnish-software.com",
	Version:  "v1alpha1",
	Resource: "varnishcacheinvalidations",
}

// Watcher watches VarnishCacheInvalidation resources and executes purge/ban
// operations against the local Varnish instance.
type Watcher struct {
	dynClient   dynamic.Interface
	k8sClient   kubernetes.Interface
	httpClient  *http.Client
	varnishAddr string // localhost varnish address (e.g., "localhost:80")
	gatewayName string // name of the gateway this chaperone serves
	namespace   string // namespace of the gateway
	podName     string // this pod's name (from downward API)
	logger      *slog.Logger

	// Track processed invalidations to avoid reprocessing
	mu        sync.Mutex
	processed map[string]struct{} // UID -> processed
}

// NewWatcher creates a new invalidation watcher.
// podName should come from the POD_NAME environment variable (downward API).
// gatewayName should come from the GATEWAY_NAME environment variable.
func NewWatcher(
	dynClient dynamic.Interface,
	k8sClient kubernetes.Interface,
	varnishAddr string,
	gatewayName string,
	namespace string,
	podName string,
	logger *slog.Logger,
) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Watcher{
		dynClient:   dynClient,
		k8sClient:   k8sClient,
		varnishAddr: varnishAddr,
		gatewayName: gatewayName,
		namespace:   namespace,
		podName:     podName,
		logger:      logger,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		processed: make(map[string]struct{}),
	}
}

// Run starts watching VarnishCacheInvalidation resources.
// It blocks until the context is cancelled.
// If readyCh is non-nil, Run waits for it to close before starting (indicating Varnish is ready).
func (w *Watcher) Run(ctx context.Context, readyCh <-chan struct{}) error {
	w.logger.Info("invalidation watcher starting",
		"gateway", w.gatewayName,
		"namespace", w.namespace,
		"pod", w.podName,
	)

	// Wait for Varnish to be ready
	if readyCh != nil {
		w.logger.Debug("waiting for varnish to be ready")
		select {
		case <-readyCh:
			w.logger.Debug("varnish is ready")
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Process any existing pending invalidations first
	if err := w.processExisting(ctx); err != nil {
		w.logger.Error("failed to process existing invalidations", "error", err)
	}

	// Watch loop with automatic reconnection
	for {
		if err := w.watchLoop(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.logger.Warn("watch connection lost, reconnecting", "error", err)
			time.Sleep(time.Second)
			continue
		}
		return ctx.Err()
	}
}

// processExisting lists and processes any pending VarnishCacheInvalidation resources.
func (w *Watcher) processExisting(ctx context.Context) error {
	list, err := w.dynClient.Resource(varnishCacheInvalidationGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list VarnishCacheInvalidations: %w", err)
	}

	for i := range list.Items {
		w.handleInvalidation(ctx, &list.Items[i])
	}
	return nil
}

// watchLoop runs a single watch session.
func (w *Watcher) watchLoop(ctx context.Context) error {
	watcher, err := w.dynClient.Resource(varnishCacheInvalidationGVR).Namespace("").Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("watch VarnishCacheInvalidations: %w", err)
	}
	defer watcher.Stop()

	w.logger.Info("invalidation watcher ready")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed")
			}
			if event.Type == watch.Added || event.Type == watch.Modified {
				if u, ok := event.Object.(*unstructured.Unstructured); ok {
					w.handleInvalidation(ctx, u)
				}
			}
		}
	}
}

// handleInvalidation processes a single VarnishCacheInvalidation resource.
func (w *Watcher) handleInvalidation(ctx context.Context, obj *unstructured.Unstructured) {
	name := obj.GetName()
	ns := obj.GetNamespace()
	uid := string(obj.GetUID())

	// Check if this pod already processed it
	w.mu.Lock()
	if _, done := w.processed[uid]; done {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

	// Check if this pod already has a result in status.podResults
	if w.podAlreadyReported(obj) {
		w.mu.Lock()
		w.processed[uid] = struct{}{}
		w.mu.Unlock()
		return
	}

	// Check gatewayRef matches our gateway
	gwName, _, _ := unstructured.NestedString(obj.Object, "spec", "gatewayRef", "name")
	gwNS, _, _ := unstructured.NestedString(obj.Object, "spec", "gatewayRef", "namespace")
	if gwNS == "" {
		gwNS = ns
	}
	if gwName != w.gatewayName || gwNS != w.namespace {
		return
	}

	// Extract spec fields
	invType, _, _ := unstructured.NestedString(obj.Object, "spec", "type")
	hostname, _, _ := unstructured.NestedString(obj.Object, "spec", "hostname")
	paths, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "paths")

	w.logger.Info("processing cache invalidation",
		"name", name,
		"namespace", ns,
		"type", invType,
		"hostname", hostname,
		"paths", paths,
	)

	// Execute the invalidation for each path
	var pathResults []any
	failures := 0
	for _, path := range paths {
		var execErr error
		switch strings.ToLower(invType) {
		case "purge":
			execErr = w.executePurge(ctx, hostname, path)
		case "ban":
			execErr = w.executeBan(ctx, hostname, path)
		default:
			execErr = fmt.Errorf("unknown invalidation type: %s", invType)
		}

		pr := map[string]any{
			"path":    path,
			"success": execErr == nil,
		}
		if execErr != nil {
			pr["message"] = execErr.Error()
			failures++
			w.logger.Error("cache invalidation failed",
				"name", name,
				"namespace", ns,
				"path", path,
				"error", execErr,
			)
		} else {
			pr["message"] = fmt.Sprintf("%s applied successfully", invType)
		}
		pathResults = append(pathResults, pr)
	}

	// Build aggregate result
	success := failures == 0
	message := fmt.Sprintf("%d/%d paths succeeded", len(paths)-failures, len(paths))
	if success {
		w.logger.Info("cache invalidation executed",
			"name", name,
			"namespace", ns,
			"paths", len(paths),
		)
	}

	// Mark as processed locally
	w.mu.Lock()
	w.processed[uid] = struct{}{}
	w.mu.Unlock()

	// Write per-pod result and compute aggregate phase
	w.updateStatus(ctx, ns, name, success, message, pathResults)
}

// podAlreadyReported checks if this pod already has a result in status.podResults.
func (w *Watcher) podAlreadyReported(obj *unstructured.Unstructured) bool {
	results, found, _ := unstructured.NestedSlice(obj.Object, "status", "podResults")
	if !found {
		return false
	}
	for _, r := range results {
		if m, ok := r.(map[string]any); ok {
			if podName, _, _ := unstructured.NestedString(m, "podName"); podName == w.podName {
				return true
			}
		}
	}
	return false
}

// executePurge sends a PURGE request to the local Varnish instance.
func (w *Watcher) executePurge(ctx context.Context, hostname, path string) error {
	url := fmt.Sprintf("http://%s%s", w.varnishAddr, path)

	req, err := http.NewRequestWithContext(ctx, "PURGE", url, nil)
	if err != nil {
		return fmt.Errorf("http.NewRequestWithContext: %w", err)
	}
	req.Host = hostname

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("PURGE request failed: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	// Varnish returns 200 on successful purge (both cache hit and miss)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PURGE returned HTTP %d", resp.StatusCode)
	}

	return nil
}

// executeBan sends a BAN request to the local Varnish instance.
// The URL path is the regex pattern to ban, and Host header scopes it to a hostname.
func (w *Watcher) executeBan(ctx context.Context, hostname, path string) error {
	url := fmt.Sprintf("http://%s%s", w.varnishAddr, path)

	req, err := http.NewRequestWithContext(ctx, "BAN", url, nil)
	if err != nil {
		return fmt.Errorf("http.NewRequestWithContext: %w", err)
	}
	req.Host = hostname

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("BAN request failed: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("BAN returned HTTP %d", resp.StatusCode)
	}

	return nil
}

// updateStatus appends this pod's result to status.podResults and computes the
// aggregate phase. Uses optimistic concurrency (resourceVersion) to handle
// concurrent updates from multiple pods.
func (w *Watcher) updateStatus(ctx context.Context, namespace, name string, success bool, message string, pathResults []any) {
	if w.dynClient == nil {
		w.logger.Warn("no dynamic client, skipping status update", "name", name)
		return
	}
	// Retry loop for conflict resolution (multiple pods updating concurrently)
	for attempt := 0; attempt < 5; attempt++ {
		// Get the latest version
		current, err := w.dynClient.Resource(varnishCacheInvalidationGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			w.logger.Error("failed to get VarnishCacheInvalidation for status update",
				"name", name, "error", err)
			return
		}

		// Build this pod's result
		now := metav1.Now().Format(time.RFC3339)
		podResult := map[string]any{
			"podName":     w.podName,
			"success":     success,
			"message":     message,
			"completedAt": now,
			"pathResults": pathResults,
		}

		// Get existing podResults, append ours
		existingResults, _, _ := unstructured.NestedSlice(current.Object, "status", "podResults")

		// Check if we already reported (race with another update cycle)
		for _, r := range existingResults {
			if m, ok := r.(map[string]any); ok {
				if pn, _, _ := unstructured.NestedString(m, "podName"); pn == w.podName {
					return // already reported
				}
			}
		}

		existingResults = append(existingResults, podResult)

		// Compute aggregate phase
		phase := w.computePhase(ctx, existingResults)

		// Build status
		status := map[string]any{
			"phase":      string(phase),
			"podResults": existingResults,
		}
		if phase == "Complete" || phase == "Failed" {
			status["completedAt"] = now
		}

		if err := unstructured.SetNestedField(current.Object, status, "status"); err != nil {
			w.logger.Error("failed to set status fields", "error", err)
			return
		}

		_, err = w.dynClient.Resource(varnishCacheInvalidationGVR).Namespace(namespace).UpdateStatus(
			ctx, current, metav1.UpdateOptions{})
		if err != nil {
			if strings.Contains(err.Error(), "the object has been modified") {
				w.logger.Debug("status update conflict, retrying", "attempt", attempt+1)
				continue
			}
			w.logger.Error("failed to update VarnishCacheInvalidation status",
				"name", name, "error", err)
			return
		}

		w.logger.Info("cache invalidation status updated",
			"name", name,
			"pod", w.podName,
			"phase", phase,
			"podResults", len(existingResults),
		)
		return
	}

	w.logger.Error("failed to update status after max retries", "name", name)
}

// computePhase determines the aggregate phase based on podResults and expected pod count.
func (w *Watcher) computePhase(ctx context.Context, podResults []any) string {
	// Count successes and failures
	successes := 0
	failures := 0
	for _, r := range podResults {
		if m, ok := r.(map[string]any); ok {
			if s, ok := m["success"].(bool); ok && s {
				successes++
			} else {
				failures++
			}
		}
	}

	totalReported := successes + failures

	// Get expected pod count from the gateway's pods
	expectedPods := w.getExpectedPodCount(ctx)

	if failures > 0 && totalReported >= expectedPods {
		return "Failed"
	}
	if successes >= expectedPods {
		return "Complete"
	}
	return "InProgress"
}

// getExpectedPodCount returns the number of running pods for this gateway.
// Falls back to 1 if the count cannot be determined.
func (w *Watcher) getExpectedPodCount(ctx context.Context) int {
	// Use the same labels the operator sets on gateway pods
	selector := labels.Set{
		"app.kubernetes.io/managed-by":          "varnish-gateway-operator",
		"gateway.networking.k8s.io/gateway-name":      w.gatewayName,
		"gateway.networking.k8s.io/gateway-namespace": w.namespace,
	}.AsSelector()

	pods, err := w.k8sClient.CoreV1().Pods(w.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		w.logger.Warn("failed to list gateway pods, assuming 1", "error", err)
		return 1
	}

	// Count running pods
	count := 0
	for _, pod := range pods.Items {
		if pod.Status.Phase == "Running" {
			count++
		}
	}
	if count == 0 {
		return 1
	}
	return count
}
