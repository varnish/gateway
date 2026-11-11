package vcl

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testVVP(namespace, name string, podResults ...any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.varnish-software.com/v1alpha1",
		"kind":       "VarnishVCLPolicy",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       map[string]any{},
	}}
	if len(podResults) > 0 {
		_ = unstructured.SetNestedSlice(u.Object, podResults, "status", "podResults")
	}
	return u
}

func newFakeDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{VarnishVCLPolicyGVR: "VarnishVCLPolicyList"}, objs...)
}

func podResults(t *testing.T, dyn *dynamicfake.FakeDynamicClient, namespace, name string) []any {
	t.Helper()
	u, err := dyn.Resource(VarnishVCLPolicyGVR).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s/%s: %v", namespace, name, err)
	}
	items, _, _ := unstructured.NestedSlice(u.Object, "status", "podResults")
	return items
}

func countUpdates(dyn *dynamicfake.FakeDynamicClient) int {
	n := 0
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "update" && a.GetSubresource() == "status" {
			n++
		}
	}
	return n
}

func TestStatusReporter_MergesPerPod(t *testing.T) {
	other := map[string]any{"podName": "gw-other", "slot": int64(3), "hash": "h", "programmed": true, "lastUpdate": "2026-01-01T00:00:00Z"}
	dyn := newFakeDynamic(testVVP("default", "p3", other))
	s := newStatusReporter(dyn, "gw-abc", testLogger())
	ctx := context.Background()

	res := SlotResult{Policy: "default/p3", Slot: 3, Hash: "aa", Programmed: false, Message: "VCC says no"}
	s.update(map[string]SlotResult{"default/p3": res, "default/missing": {Policy: "default/missing", Slot: 1}, "bad": {Policy: "bad"}})
	if err := s.flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	items := podResults(t, dyn, "default", "p3")
	if len(items) != 2 {
		t.Fatalf("podResults = %v, want the other pod's entry and ours", items)
	}
	mine := items[1].(map[string]any)
	if mine["podName"] != "gw-abc" || mine["programmed"] != false || mine["message"] != "VCC says no" || mine["hash"] != "aa" || mine["lastUpdate"] == "" {
		t.Errorf("entry = %v", mine)
	}
	if items[0].(map[string]any)["podName"] != "gw-other" {
		t.Errorf("other pod's entry clobbered: %v", items[0])
	}

	// Unchanged results: no further writes.
	before := countUpdates(dyn)
	s.update(map[string]SlotResult{"default/p3": res})
	if err := s.flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if countUpdates(dyn) != before {
		t.Error("unchanged result was written again")
	}

	// Changed result replaces our entry in place.
	res.Programmed, res.Message, res.Hash = true, "", "bb"
	s.update(map[string]SlotResult{"default/p3": res})
	if err := s.flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	items = podResults(t, dyn, "default", "p3")
	mine = items[1].(map[string]any)
	if len(items) != 2 || mine["programmed"] != true || mine["hash"] != "bb" {
		t.Errorf("after change: %v", items)
	}
	if _, ok := mine["message"]; ok {
		t.Errorf("message not cleared: %v", mine)
	}
}

func TestStatusReporter_RetriesConflict(t *testing.T) {
	dyn := newFakeDynamic(testVVP("default", "p1"))
	conflicts := 2
	dyn.PrependReactor("update", "varnishvclpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "status" && conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(VarnishVCLPolicyGVR.GroupResource(), "p1", nil)
		}
		return false, nil, nil
	})
	s := newStatusReporter(dyn, "gw-abc", testLogger())
	s.update(map[string]SlotResult{"default/p1": {Policy: "default/p1", Slot: 1, Hash: "aa", Programmed: true}})
	if err := s.flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if items := podResults(t, dyn, "default", "p1"); len(items) != 1 {
		t.Errorf("podResults = %v", items)
	}
}

func TestStatusReporter_FailureIsRetriedOnNextFlush(t *testing.T) {
	dyn := newFakeDynamic(testVVP("default", "p1"))
	fail := true
	dyn.PrependReactor("update", "varnishvclpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if fail {
			return true, nil, apierrors.NewServiceUnavailable("down")
		}
		return false, nil, nil
	})
	s := newStatusReporter(dyn, "gw-abc", testLogger())
	s.update(map[string]SlotResult{"default/p1": {Policy: "default/p1", Slot: 1, Hash: "aa", Programmed: true}})
	if err := s.flush(context.Background()); err == nil {
		t.Fatal("flush did not report the failure")
	}
	fail = false
	if err := s.flush(context.Background()); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if items := podResults(t, dyn, "default", "p1"); len(items) != 1 {
		t.Errorf("podResults = %v", items)
	}
}

func entryFor(t *testing.T, dyn *dynamicfake.FakeDynamicClient, name, pod string) map[string]any {
	t.Helper()
	for _, item := range podResults(t, dyn, "default", name) {
		if m, ok := item.(map[string]any); ok && m["podName"] == pod {
			return m
		}
	}
	return nil
}

// There is no "already written" cache: a policy deleted and recreated under
// the same name gets this pod's entry back within a resync.
func TestStatusReporter_ResyncRestoresEntry(t *testing.T) {
	dyn := newFakeDynamic(testVVP("default", "p1"))
	s := newStatusReporter(dyn, "gw-abc", testLogger())
	s.resync = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.run(ctx)

	s.update(map[string]SlotResult{"default/p1": {Policy: "default/p1", Slot: 1, Hash: "aa", Programmed: true}})
	waitFor(t, func() bool { return entryFor(t, dyn, "p1", "gw-abc") != nil }, "first write")

	client := dyn.Resource(VarnishVCLPolicyGVR).Namespace("default")
	if err := client.Delete(ctx, "p1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Create(ctx, testVVP("default", "p1"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		e := entryFor(t, dyn, "p1", "gw-abc")
		return e != nil && e["programmed"] == true && e["hash"] == "aa"
	}, "entry rewritten on the recreated policy")

	// Same for a status rewritten without podResults.
	u, err := client.Get(ctx, "p1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	unstructured.RemoveNestedField(u.Object, "status", "podResults")
	if _, err := client.UpdateStatus(ctx, u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return entryFor(t, dyn, "p1", "gw-abc") != nil }, "entry rewritten after podResults were dropped")
}

// A policy that leaves this pod's results gets one final entry saying so.
func TestStatusReporter_LeftManifest(t *testing.T) {
	dyn := newFakeDynamic(testVVP("default", "p1"), testVVP("default", "p2"))
	s := newStatusReporter(dyn, "gw-abc", testLogger())
	ctx := context.Background()
	flush := func() {
		t.Helper()
		if err := s.flush(ctx); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}

	p1 := SlotResult{Policy: "default/p1", Slot: 1, Hash: "aa", Programmed: true}
	p2 := SlotResult{Policy: "default/p2", Slot: 2, Hash: "bb", Programmed: true}
	s.update(map[string]SlotResult{"default/p1": p1, "default/p2": p2})
	flush()
	s.update(map[string]SlotResult{"default/p2": p2})
	flush()
	e := entryFor(t, dyn, "p1", "gw-abc")
	if e == nil || e["programmed"] != false || e["hash"] != "" || e["message"] != notInManifestMessage || e["slot"] != int64(1) {
		t.Errorf("final entry = %v", e)
	}
	if e := entryFor(t, dyn, "p2", "gw-abc"); e["programmed"] != true {
		t.Errorf("p2 entry = %v", e)
	}

	// Written once: a later flush does not touch it again.
	before := countUpdates(dyn)
	flush()
	if countUpdates(dyn) != before {
		t.Error("an unchanged flush wrote again")
	}

	// Back in the manifest: a live entry again.
	s.update(map[string]SlotResult{"default/p1": p1, "default/p2": p2})
	flush()
	if e := entryFor(t, dyn, "p1", "gw-abc"); e["programmed"] != true || e["hash"] != "aa" {
		t.Errorf("p1 entry after returning = %v", e)
	}
}

// A result superseded while the flush is busy with earlier policies is not
// written; the next flush writes the newer one.
func TestStatusReporter_SkipsSupersededResult(t *testing.T) {
	dyn := newFakeDynamic(testVVP("default", "p1"), testVVP("default", "p2"))
	s := newStatusReporter(dyn, "gw-abc", testLogger())
	old2 := SlotResult{Policy: "default/p2", Slot: 2, Hash: "old", Programmed: true}
	new2 := SlotResult{Policy: "default/p2", Slot: 2, Hash: "new", Programmed: true}
	p1 := SlotResult{Policy: "default/p1", Slot: 1, Hash: "aa", Programmed: true}
	superseded := false
	dyn.PrependReactor("get", "varnishvclpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.GetAction).GetName() == "p1" && !superseded {
			superseded = true
			s.update(map[string]SlotResult{"default/p1": p1, "default/p2": new2})
		}
		return false, nil, nil
	})
	s.update(map[string]SlotResult{"default/p1": p1, "default/p2": old2})
	if err := s.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e := entryFor(t, dyn, "p2", "gw-abc"); e != nil {
		t.Errorf("superseded result written: %v", e)
	}
	if err := s.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e := entryFor(t, dyn, "p2", "gw-abc"); e == nil || e["hash"] != "new" {
		t.Errorf("p2 entry = %v, want the newer result", e)
	}
}
