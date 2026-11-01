package vcl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// VarnishVCLPolicyGVR identifies VarnishVCLPolicy for dynamic clients.
var VarnishVCLPolicyGVR = schema.GroupVersionResource{
	Group:    "gateway.varnish-software.com",
	Version:  "v1alpha1",
	Resource: "varnishvclpolicies",
}

const (
	statusMaxAttempts = 5
	statusRetryDelay  = 10 * time.Second
)

// statusReporter writes this pod's slot results into VarnishVCLPolicy
// status.podResults. It runs on its own goroutine so a slow or failing API
// server never holds up VCL work: the reconcile loop hands it the latest
// results and moves on.
type statusReporter struct {
	dyn     dynamic.Interface
	podName string
	logger  *slog.Logger

	mu      sync.Mutex
	desired map[string]SlotResult // policy -> latest result
	written map[string]SlotResult // policy -> result last written (or found already present)
	nudge   chan struct{}
}

func newStatusReporter(dyn dynamic.Interface, podName string, logger *slog.Logger) *statusReporter {
	return &statusReporter{
		dyn:     dyn,
		podName: podName,
		logger:  logger,
		desired: make(map[string]SlotResult),
		written: make(map[string]SlotResult),
		nudge:   make(chan struct{}, 1),
	}
}

// update replaces the desired results. Policies that disappeared are
// forgotten (their status entry is left for the operator to clean up).
func (s *statusReporter) update(results map[string]SlotResult) {
	s.mu.Lock()
	s.desired = maps.Clone(results)
	for policy := range s.written {
		if _, ok := s.desired[policy]; !ok {
			delete(s.written, policy)
		}
	}
	s.mu.Unlock()
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// run writes pending results on every update, retrying failures after
// statusRetryDelay.
func (s *statusReporter) run(ctx context.Context) {
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.nudge:
		case <-retry.C:
		}
		if err := s.flush(ctx); err != nil {
			s.logger.Warn("VarnishVCLPolicy status update failed, will retry", "error", err, "retryIn", statusRetryDelay)
			retry.Reset(statusRetryDelay)
		}
	}
}

// flush writes every result that differs from what was last written.
func (s *statusReporter) flush(ctx context.Context) error {
	s.mu.Lock()
	pending := make(map[string]SlotResult)
	for policy, res := range s.desired {
		if w, ok := s.written[policy]; !ok || w != res {
			pending[policy] = res
		}
	}
	s.mu.Unlock()

	var errs []error
	for _, policy := range slices.Sorted(maps.Keys(pending)) {
		res := pending[policy]
		if err := s.write(ctx, res); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", policy, err))
			continue
		}
		s.mu.Lock()
		if s.desired[policy] == res {
			s.written[policy] = res
		}
		s.mu.Unlock()
	}
	return errors.Join(errs...)
}

// write merges this pod's entry into the policy's status.podResults with a
// get / modify / UpdateStatus loop that re-reads on conflict. A missing
// policy or an unparsable policy reference is not an error: there is nothing
// to write to.
func (s *statusReporter) write(ctx context.Context, res SlotResult) error {
	namespace, name, ok := strings.Cut(res.Policy, "/")
	if !ok || namespace == "" || name == "" {
		s.logger.Warn("route VCL manifest policy is not <namespace>/<name>, not reporting status", "policy", res.Policy)
		return nil
	}
	client := s.dyn.Resource(VarnishVCLPolicyGVR).Namespace(namespace)
	for attempt := range statusMaxAttempts {
		current, err := client.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			s.logger.Debug("VarnishVCLPolicy not found, not reporting status", "policy", res.Policy)
			return nil
		}
		if err != nil {
			return fmt.Errorf("dynamic.Get(%s): %w", res.Policy, err)
		}

		podResults, _, err := unstructured.NestedSlice(current.Object, "status", "podResults")
		if err != nil {
			podResults = nil
		}
		idx := -1
		for i, item := range podResults {
			if m, ok := item.(map[string]any); ok && m["podName"] == s.podName {
				idx = i
				if sameResult(m, res) {
					return nil
				}
				break
			}
		}
		entry := map[string]any{
			"podName":    s.podName,
			"slot":       int64(res.Slot),
			"hash":       res.Hash,
			"programmed": res.Programmed,
			"lastUpdate": metav1.Now().UTC().Format(time.RFC3339),
		}
		if res.Message != "" {
			entry["message"] = res.Message
		}
		if idx >= 0 {
			podResults[idx] = entry
		} else {
			podResults = append(podResults, entry)
		}
		if err := unstructured.SetNestedSlice(current.Object, podResults, "status", "podResults"); err != nil {
			return fmt.Errorf("unstructured.SetNestedSlice(%s): %w", res.Policy, err)
		}

		_, err = client.UpdateStatus(ctx, current, metav1.UpdateOptions{})
		if err == nil {
			s.logger.Info("VarnishVCLPolicy status updated", "policy", res.Policy, "slot", res.Slot,
				"programmed", res.Programmed, "pod", s.podName)
			return nil
		}
		if !apierrors.IsConflict(err) {
			return fmt.Errorf("dynamic.UpdateStatus(%s): %w", res.Policy, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
		}
	}
	return fmt.Errorf("dynamic.UpdateStatus(%s): conflict after %d attempts", res.Policy, statusMaxAttempts)
}

// sameResult reports whether an existing podResults entry already says res.
func sameResult(m map[string]any, res SlotResult) bool {
	slot, _, _ := unstructured.NestedInt64(m, "slot")
	hash, _, _ := unstructured.NestedString(m, "hash")
	programmed, _, _ := unstructured.NestedBool(m, "programmed")
	message, _, _ := unstructured.NestedString(m, "message")
	return slot == int64(res.Slot) && hash == res.Hash && programmed == res.Programmed && message == res.Message
}
