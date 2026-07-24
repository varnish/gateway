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
	"regexp"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// hostnameRE matches an RFC 1123 hostname (DNS name). It deliberately excludes
// whitespace, quotes and the '&' character so that a hostname can never break
// out of the Varnish ban expression it is concatenated into (see M-21 and the
// BAN handler in internal/vcl/preamble.vcl).
var hostnameRE = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)

// validateInvalidationSpec validates the untrusted hostname and paths from a
// VarnishCacheInvalidation CR before any request is sent to Varnish.
//
// The hostname is set verbatim as the Host header and, via the ban-lurker
// headers, concatenated into a Varnish ban expression. The paths become the
// request URL, which for BAN is used as a regex inside the same expression.
// Since std.ban() takes a single string with no escaping mechanism, strict
// input validation here is the primary defense against ban-expression
// injection: a hostname or path containing whitespace, quotes or '&' could
// otherwise inject additional ban conditions and flush unrelated objects.
func validateInvalidationSpec(hostname string, paths []string) error {
	if hostname == "" {
		return fmt.Errorf("spec.hostname is required")
	}
	if len(hostname) > 253 || !hostnameRE.MatchString(hostname) {
		return fmt.Errorf("spec.hostname %q is not a valid RFC 1123 hostname", hostname)
	}
	if len(paths) == 0 {
		return fmt.Errorf("spec.paths must contain at least one path")
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("spec.paths entry %q must start with '/'", p)
		}
		if strings.ContainsAny(p, " \t\r\n\"") {
			return fmt.Errorf("spec.paths entry %q must not contain whitespace or quotes", p)
		}
	}
	return nil
}

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

	// M-14: enforce same-namespace authorization. The CR must live in the same
	// namespace as the target gateway. The only cluster-level authorization for
	// a VarnishCacheInvalidation is "can create the CR somewhere"; without this
	// check any author could set spec.gatewayRef.namespace to an unrelated
	// gateway and purge/ban (or `type: Ban` with `.*` flush) its entire cache.
	// The chaperone has no controller-runtime client to evaluate a
	// ReferenceGrant, so cross-namespace refs are rejected outright — matching
	// how the rest of the operator treats cross-namespace references (a
	// ReferenceGrant would be the follow-up to relax this).
	if ns != gwNS {
		msg := fmt.Sprintf("cross-namespace gatewayRef not permitted: VarnishCacheInvalidation in namespace %q targets gateway in namespace %q (create the invalidation in %q)", ns, gwNS, gwNS)
		w.logger.Warn("rejecting cross-namespace cache invalidation", "name", name, "namespace", ns, "gatewayNamespace", gwNS)
		if w.updateStatus(ctx, ns, name, false, msg, nil) {
			w.markProcessed(uid)
		}
		return
	}

	// Extract spec fields
	invType, _, _ := unstructured.NestedString(obj.Object, "spec", "type")
	hostname, _, _ := unstructured.NestedString(obj.Object, "spec", "hostname")
	paths, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "paths")

	// M-21: validate untrusted hostname/paths BEFORE issuing any request. An
	// invalid spec is a terminal Failed result, not a transient error.
	if err := validateInvalidationSpec(hostname, paths); err != nil {
		msg := fmt.Sprintf("invalid cache invalidation spec: %v", err)
		w.logger.Warn("rejecting invalid cache invalidation", "name", name, "namespace", ns, "error", err)
		if w.updateStatus(ctx, ns, name, false, msg, nil) {
			w.markProcessed(uid)
		}
		return
	}

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

	// Write per-pod result and compute aggregate phase. Mark the invalidation
	// as processed locally ONLY after the status write succeeds (M-20). If the
	// status update fails (transient API error, exhausted conflict retries) we
	// leave it unprocessed so a later watch event or re-list retries it — the
	// podAlreadyReported guard and the idempotency of purge/ban make
	// reprocessing safe, and this prevents a CR from being wedged in
	// Pending/InProgress forever (where operator GC never reaps it).
	if w.updateStatus(ctx, ns, name, success, message, pathResults) {
		w.markProcessed(uid)
	}
}

// markProcessed records that this pod has finished processing the given UID.
func (w *Watcher) markProcessed(uid string) {
	w.mu.Lock()
	w.processed[uid] = struct{}{}
	w.mu.Unlock()
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

	// Varnish's return(purge) synthesizes 200 on a cache hit and 404 ("Not in
	// cache") on a miss. Purge is an idempotent delete: purging a URL that is
	// not cached still leaves the cache in the desired state (object absent),
	// so 404 is a success, not a failure (M-19). Any other status is an error.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
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
//
// It returns true when the status was durably recorded (either this pod's
// result was written, or this pod was already recorded), and false on any
// failure to persist. Callers use the return value to decide whether to mark
// the invalidation processed (M-20): a false return means "retry later".
func (w *Watcher) updateStatus(ctx context.Context, namespace, name string, success bool, message string, pathResults []any) bool {
	if w.dynClient == nil {
		// No status backend (only happens in tests/standalone). There is
		// nothing to persist, so treat the work as done to avoid reprocessing.
		w.logger.Warn("no dynamic client, skipping status update", "name", name)
		return true
	}
	// Retry loop for conflict resolution (multiple pods updating concurrently)
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Get the latest version
		current, err := w.dynClient.Resource(varnishCacheInvalidationGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			w.logger.Error("failed to get VarnishCacheInvalidation for status update",
				"name", name, "error", err)
			return false
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
					return true // already reported — durably recorded
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
			return false
		}

		_, err = w.dynClient.Resource(varnishCacheInvalidationGVR).Namespace(namespace).UpdateStatus(
			ctx, current, metav1.UpdateOptions{})
		if err != nil {
			if apierrors.IsConflict(err) {
				w.logger.Debug("status update conflict, retrying", "attempt", attempt+1)
				// Bounded backoff before re-reading and retrying.
				select {
				case <-ctx.Done():
					return false
				case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
				}
				continue
			}
			w.logger.Error("failed to update VarnishCacheInvalidation status",
				"name", name, "error", err)
			return false
		}

		w.logger.Info("cache invalidation status updated",
			"name", name,
			"pod", w.podName,
			"phase", phase,
			"podResults", len(existingResults),
		)
		return true
	}

	w.logger.Error("failed to update status after max retries", "name", name)
	return false
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
		"app.kubernetes.io/managed-by":                "varnish-gateway-operator",
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
