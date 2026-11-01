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
	// statusResyncPeriod re-checks every entry this pod should have, so a
	// policy deleted and recreated under the same name, or one whose status
	// was rewritten without podResults, gets its entry back.
	statusResyncPeriod = 60 * time.Second

	// notInManifestMessage is the final entry written for a policy that left
	// this pod's manifest (detached, moved to another policy, route VCL
	// ConfigMap gone), so the operator can tell a stale entry from a live
	// one.
	notInManifestMessage = "not in manifest on this pod"
)

// statusReporter writes this pod's slot results into VarnishVCLPolicy
// status.podResults. It runs on its own goroutine so a slow or failing API
// server never holds up VCL work: the reconcile loop hands it the latest
// results and moves on.
//
// There is no in-memory record of what was written: every flush goes through
// write(), which reads the policy and only updates it when this pod's entry
// differs. Flushes run when the results change, after a failure, and every
// resync period.
type statusReporter struct {
	dyn     dynamic.Interface
	podName string
	logger  *slog.Logger
	resync  time.Duration

	mu      sync.Mutex
	desired map[string]SlotResult // policy -> latest result
	gone    map[string]SlotResult // policy -> final entry still to write
	nudge   chan struct{}
}

func newStatusReporter(dyn dynamic.Interface, podName string, logger *slog.Logger) *statusReporter {
	return &statusReporter{
		dyn:     dyn,
		podName: podName,
		logger:  logger,
		resync:  statusResyncPeriod,
		desired: make(map[string]SlotResult),
		gone:    make(map[string]SlotResult),
		nudge:   make(chan struct{}, 1),
	}
}

// update replaces the desired results. A policy that disappeared gets one
// final entry, programmed false with notInManifestMessage, instead of being
// deleted: replacing the entry goes through the same get/UpdateStatus path
// as every other write, and the operator can still see which pods had it.
func (s *statusReporter) update(results map[string]SlotResult) {
	s.mu.Lock()
	changed := !maps.Equal(s.desired, results)
	for policy, old := range s.desired {
		if _, ok := results[policy]; !ok {
			s.gone[policy] = SlotResult{Policy: policy, Slot: old.Slot, Message: notInManifestMessage}
		}
	}
	for policy := range results {
		delete(s.gone, policy)
	}
	s.desired = maps.Clone(results)
	s.mu.Unlock()
	if !changed {
		return
	}
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// run writes the results on every change and every resync period, retrying
// failures after statusRetryDelay.
func (s *statusReporter) run(ctx context.Context) {
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	resync := time.NewTicker(s.resync)
	defer resync.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.nudge:
		case <-retry.C:
		case <-resync.C:
		}
		if err := s.flush(ctx); err != nil {
			s.logger.Warn("VarnishVCLPolicy status update failed, will retry", "error", err, "retryIn", statusRetryDelay)
			retry.Reset(statusRetryDelay)
		}
	}
}

// current returns the result to write for policy now: its desired result or
// its pending final entry.
func (s *statusReporter) current(policy string) (SlotResult, bool) {
	if res, ok := s.desired[policy]; ok {
		return res, true
	}
	res, ok := s.gone[policy]
	return res, ok
}

// flush makes every policy's entry match the latest results. Each entry is
// re-read under the lock right before its write, so a result superseded
// while earlier writes were in flight is not written (the newer one is, by
// the flush its update triggered).
func (s *statusReporter) flush(ctx context.Context) error {
	s.mu.Lock()
	pending := maps.Clone(s.gone)
	maps.Copy(pending, s.desired)
	s.mu.Unlock()

	var errs []error
	for _, policy := range slices.Sorted(maps.Keys(pending)) {
		res := pending[policy]
		s.mu.Lock()
		cur, ok := s.current(policy)
		s.mu.Unlock()
		if !ok || cur != res {
			continue
		}
		if err := s.write(ctx, res); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", policy, err))
			continue
		}
		s.mu.Lock()
		if g, ok := s.gone[policy]; ok && g == res {
			delete(s.gone, policy)
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
