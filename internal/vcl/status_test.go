package vcl

import (
	"context"
	"testing"

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
