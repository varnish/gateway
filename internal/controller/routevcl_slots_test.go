package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/event"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
	"github.com/varnish/gateway/internal/vcl"
)

// --- pure slot assignment ---

var slotTestEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// cand builds a candidate "default/<name>" targeting route "default/r-<name>",
// created minute minutes after slotTestEpoch.
func cand(name string, minute int, doc string) routeVCLCandidate {
	return routeVCLCandidate{
		policy:     "default/" + name,
		route:      "default/r-" + name,
		generation: 1,
		created:    metav1.NewTime(slotTestEpoch.Add(time.Duration(minute) * time.Minute)),
		document:   doc,
	}
}

// slotsOf returns policy -> slot for a manifest.
func slotsOf(t *testing.T, m vcl.RouteVCLManifest) map[string]int {
	t.Helper()
	out := map[string]int{}
	for k, s := range m.Slots {
		n, ok := parseManifestSlot(k)
		if !ok {
			t.Fatalf("bad slot key %q", k)
		}
		out[s.Policy] = n
	}
	return out
}

func unassignedOf(m vcl.RouteVCLManifest) map[string]string {
	out := map[string]string{}
	for _, u := range m.Unassigned {
		out[u.Policy] = u.Reason
	}
	return out
}

func assertSlots(t *testing.T, m vcl.RouteVCLManifest, want map[string]int) {
	t.Helper()
	got := slotsOf(t, m)
	if len(got) != len(want) {
		t.Fatalf("slots = %v, want %v", got, want)
	}
	for p, s := range want {
		if got[p] != s {
			t.Errorf("slot of %s = %d, want %d (all: %v)", p, got[p], s, got)
		}
	}
}

func TestRouteVCLPoolSize(t *testing.T) {
	params := func(rv *gatewayparamsv1alpha1.RouteVCLConfig) *gatewayparamsv1alpha1.GatewayClassParameters {
		return &gatewayparamsv1alpha1.GatewayClassParameters{Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{RouteVCL: rv}}
	}
	cases := []struct {
		name string
		p    *gatewayparamsv1alpha1.GatewayClassParameters
		want int
	}{
		{"no params", nil, 0},
		{"no routeVCL", params(nil), 0},
		{"disabled with size", params(&gatewayparamsv1alpha1.RouteVCLConfig{SlotPoolSize: ptr.To(int32(8))}), 0},
		{"enabled default", params(&gatewayparamsv1alpha1.RouteVCLConfig{Enabled: true}), gatewayparamsv1alpha1.DefaultRouteVCLSlotPoolSize},
		{"enabled explicit", params(&gatewayparamsv1alpha1.RouteVCLConfig{Enabled: true, SlotPoolSize: ptr.To(int32(8))}), 8},
		{"clamped", params(&gatewayparamsv1alpha1.RouteVCLConfig{Enabled: true, SlotPoolSize: ptr.To(int32(10000))}), gatewayparamsv1alpha1.MaxRouteVCLSlotPoolSize},
	}
	for _, tc := range cases {
		if got := routeVCLPoolSize(tc.p); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestAssignRouteVCLSlots_StableAcrossReconcilesAndDocumentChanges(t *testing.T) {
	cands := []routeVCLCandidate{cand("c", 3, "c1"), cand("a", 1, "a1"), cand("b", 2, "b1")}
	first := assignRouteVCLSlots(vcl.RouteVCLManifest{}, cands, 8, "")
	assertSlots(t, first.manifest, map[string]int{"default/a": 0, "default/b": 1, "default/c": 2})

	// Same input, previous manifest from the first round: identical data.
	second := assignRouteVCLSlots(first.manifest, cands, 8, "")
	if fmt.Sprint(second.data) != fmt.Sprint(first.data) {
		t.Errorf("second reconcile changed the ConfigMap data")
	}

	// "a" changes its document and generation: same slot, new hash.
	changed := []routeVCLCandidate{cand("c", 3, "c1"), cand("a", 1, "a2"), cand("b", 2, "b1")}
	changed[1].generation = 2
	third := assignRouteVCLSlots(second.manifest, changed, 8, "")
	assertSlots(t, third.manifest, map[string]int{"default/a": 0, "default/b": 1, "default/c": 2})
	if third.manifest.Slots["0"].Hash == first.manifest.Slots["0"].Hash {
		t.Error("hash of slot 0 did not change with the document")
	}
	if third.manifest.Slots["0"].Generation != 2 {
		t.Errorf("generation = %d, want 2", third.manifest.Slots["0"].Generation)
	}

	// Without the previous manifest, a creation order that differs from
	// the old assignment would renumber; with it, nothing moves.
	reordered := []routeVCLCandidate{cand("c", 0, "c1"), cand("a", 1, "a1"), cand("b", 2, "b1")}
	kept := assignRouteVCLSlots(first.manifest, reordered, 8, "")
	assertSlots(t, kept.manifest, map[string]int{"default/a": 0, "default/b": 1, "default/c": 2})
}

func TestAssignRouteVCLSlots_ReleaseAndReuse(t *testing.T) {
	prev := assignRouteVCLSlots(vcl.RouteVCLManifest{},
		[]routeVCLCandidate{cand("a", 1, "a"), cand("b", 2, "b"), cand("c", 3, "c")}, 8, "").manifest

	// "a" (slot 0) goes away; "d" is new and also older than everyone.
	// Retained slots are honoured first, then "d" reuses slot 0.
	next := assignRouteVCLSlots(prev, []routeVCLCandidate{cand("b", 2, "b"), cand("c", 3, "c"), cand("d", 0, "d")}, 8, "")
	assertSlots(t, next.manifest, map[string]int{"default/b": 1, "default/c": 2, "default/d": 0})
	if _, ok := next.data[vcl.SlotKey(0)]; !ok {
		t.Error("slot 0 document missing")
	}

	// A new policy older than a retained one cannot take its slot.
	prev2 := assignRouteVCLSlots(vcl.RouteVCLManifest{}, []routeVCLCandidate{cand("b", 2, "b")}, 8, "").manifest
	next2 := assignRouteVCLSlots(prev2, []routeVCLCandidate{cand("a", 1, "a"), cand("b", 2, "b")}, 8, "")
	assertSlots(t, next2.manifest, map[string]int{"default/a": 1, "default/b": 0})
}

func TestAssignRouteVCLSlots_Exhaustion(t *testing.T) {
	cands := []routeVCLCandidate{cand("a", 1, "a"), cand("b", 2, "b"), cand("c", 3, "c")}
	first := assignRouteVCLSlots(vcl.RouteVCLManifest{}, cands, 2, "")
	assertSlots(t, first.manifest, map[string]int{"default/a": 0, "default/b": 1})
	if got := unassignedOf(first.manifest); got["default/c"] != vcl.UnassignedNoSlotAvailable || len(got) != 1 {
		t.Errorf("unassigned = %v", got)
	}
	if first.manifest.Unassigned[0].Route != "default/r-c" {
		t.Errorf("unassigned route = %q", first.manifest.Unassigned[0].Route)
	}
	if len(first.newlyUnassigned) != 1 {
		t.Errorf("newlyUnassigned = %v, want the one new entry (logged once)", first.newlyUnassigned)
	}
	// Second round: same result, nothing new to log.
	second := assignRouteVCLSlots(first.manifest, cands, 2, "")
	if len(second.newlyUnassigned) != 0 {
		t.Errorf("newlyUnassigned on the second round = %v, want none", second.newlyUnassigned)
	}
	if fmt.Sprint(second.data) != fmt.Sprint(first.data) {
		t.Error("second round changed the data")
	}
	// A slot frees up: the waiting policy gets it.
	third := assignRouteVCLSlots(second.manifest, []routeVCLCandidate{cand("b", 2, "b"), cand("c", 3, "c")}, 2, "")
	assertSlots(t, third.manifest, map[string]int{"default/b": 1, "default/c": 0})
	if len(third.manifest.Unassigned) != 0 {
		t.Errorf("unassigned = %v", third.manifest.Unassigned)
	}
}

func TestAssignRouteVCLSlots_ShrinkBelowAssignedSlot(t *testing.T) {
	cands := []routeVCLCandidate{cand("a", 1, "a"), cand("b", 2, "b"), cand("c", 3, "c"), cand("d", 4, "d")}
	full := assignRouteVCLSlots(vcl.RouteVCLManifest{}, cands, 4, "").manifest
	assertSlots(t, full, map[string]int{"default/a": 0, "default/b": 1, "default/c": 2, "default/d": 3})

	// Pool 4 -> 2 with every slot in use: c and d lose their slots, a and b
	// keep their numbers.
	shrunk := assignRouteVCLSlots(full, cands, 2, "")
	assertSlots(t, shrunk.manifest, map[string]int{"default/a": 0, "default/b": 1})
	un := unassignedOf(shrunk.manifest)
	if un["default/c"] != vcl.UnassignedSlotOutsidePool || un["default/d"] != vcl.UnassignedSlotOutsidePool {
		t.Errorf("unassigned = %v, want c and d SlotOutsidePool", un)
	}
	for _, k := range []string{vcl.SlotKey(2), vcl.SlotKey(3)} {
		if _, ok := shrunk.data[k]; ok {
			t.Errorf("stale key %s kept", k)
		}
	}
	// The reason is sticky and the result stable on the next round.
	again := assignRouteVCLSlots(shrunk.manifest, cands, 2, "")
	if fmt.Sprint(again.data) != fmt.Sprint(shrunk.data) {
		t.Error("result not stable after shrink")
	}

	// Pool 4 -> 2 with a free slot inside the new pool: the policy that
	// lost slot 3 is re-placed in slot 1; slot 0 does not move.
	sparse := assignRouteVCLSlots(vcl.RouteVCLManifest{Slots: map[string]vcl.RouteVCLSlot{
		"0": {Policy: "default/a"}, "3": {Policy: "default/d"},
	}}, []routeVCLCandidate{cand("a", 1, "a"), cand("d", 4, "d")}, 2, "")
	assertSlots(t, sparse.manifest, map[string]int{"default/a": 0, "default/d": 1})
}

func TestAssignRouteVCLSlots_ComposedDocumentsAndHash(t *testing.T) {
	userVCL := "vcl 4.1;\nsub vcl_deliver { set resp.http.X-Gateway-User = \"1\"; }\n"
	routeVCL := "vcl 4.1;\nsub vcl_recv { set req.http.X-Route = \"1\"; }\n"
	a := assignRouteVCLSlots(vcl.RouteVCLManifest{}, []routeVCLCandidate{cand("a", 1, routeVCL)}, 4, userVCL)

	doc := a.data[vcl.SlotKey(0)]
	if doc != vcl.GenerateRouteVCL(userVCL, routeVCL) {
		t.Error("slot document is not GenerateRouteVCL(userVCL, routeVCL)")
	}
	if !strings.Contains(doc, "X-Gateway-User") || !strings.Contains(doc, "X-Route") {
		t.Error("document does not contain both the gateway and the route VCL")
	}
	s := a.manifest.Slots["0"]
	if s.Hash != vcl.RouteVCLHash(doc) {
		t.Errorf("hash = %s, want RouteVCLHash(doc)", s.Hash)
	}
	if s.Policy != "default/a" || s.Route != "default/r-a" || s.Generation != 1 {
		t.Errorf("slot = %+v", s)
	}
	// The manifest key round-trips through the chaperone's parser.
	m, err := vcl.ParseRouteVCLManifest(a.data[vcl.ManifestKey])
	if err != nil || m.Slots["0"] != s {
		t.Errorf("ParseRouteVCLManifest = %+v, %v", m, err)
	}

	// A gateway user VCL change changes every hash.
	b := assignRouteVCLSlots(a.manifest, []routeVCLCandidate{cand("a", 1, routeVCL)}, 4, userVCL+"\n# changed\n")
	if b.manifest.Slots["0"].Hash == s.Hash {
		t.Error("hash did not change with the gateway user VCL")
	}
}

func TestAssignRouteVCLSlots_PoolZero(t *testing.T) {
	prev := assignRouteVCLSlots(vcl.RouteVCLManifest{}, []routeVCLCandidate{cand("a", 1, "a")}, 4, "").manifest
	a := assignRouteVCLSlots(prev, []routeVCLCandidate{cand("a", 1, "a")}, 0, "")
	if len(a.manifest.Slots) != 0 || len(a.manifest.Unassigned) != 0 {
		t.Errorf("manifest = %+v, want empty", a.manifest)
	}
	if len(a.data) != 1 || a.data[vcl.ManifestKey] == "" {
		t.Errorf("data = %v, want only the manifest", a.data)
	}
}

func TestAssignRouteVCLSlots_ConfigMapFull(t *testing.T) {
	big := strings.Repeat("#", 250*1024)
	cands := []routeVCLCandidate{
		cand("a", 1, big), cand("b", 2, big), cand("c", 3, big),
		cand("d", 4, big),     // would cross 900 KiB
		cand("e", 5, "small"), // still fits
	}
	a := assignRouteVCLSlots(vcl.RouteVCLManifest{}, cands, 8, "")
	assertSlots(t, a.manifest, map[string]int{"default/a": 0, "default/b": 1, "default/c": 2, "default/e": 3})
	if un := unassignedOf(a.manifest); un["default/d"] != vcl.UnassignedConfigMapFull || len(un) != 1 {
		t.Errorf("unassigned = %v", un)
	}
	total := 0
	for k, v := range a.data {
		total += len(k) + len(v)
	}
	if total > 1024*1024 {
		t.Errorf("ConfigMap data is %d bytes, over 1 MiB", total)
	}
}

// --- Gateway reconcile with a fake client ---

// routeVCLWorld returns a Gateway "gw" (class "varnish", params "params" with
// route VCL pool poolSize; disabled when poolSize is 0), a user VCL ConfigMap,
// an HTTPRoute "r1" attached to gw, and an Accepted VVP "p1" whose document
// lives in ConfigMap "vcl1".
func routeVCLWorld(poolSize int32) []runtime.Object {
	rv := &gatewayparamsv1alpha1.RouteVCLConfig{Enabled: poolSize > 0, SlotPoolSize: ptr.To(max(poolSize, 1))}
	params := vvpParams("params", rv)
	params.Spec.UserVCLConfigMapRef = &gatewayparamsv1alpha1.ConfigMapReference{Name: "user-vcl", Namespace: vvpTestNS}
	return []runtime.Object{
		vvpGatewayClass("varnish", "params"),
		params,
		vvpConfigMap("user-vcl", map[string]string{"user.vcl": testUserVCL}),
		vvpGateway("gw", "varnish"),
		vvpRoute("r1", "gw"),
		vvpConfigMap("vcl1", map[string]string{"route.vcl": testRouteVCL}),
		acceptedVVP("p1", "r1", "vcl1", slotTestEpoch),
	}
}

const (
	testUserVCL  = "vcl 4.1;\nsub vcl_deliver { set resp.http.X-User = \"1\"; }\n"
	testRouteVCL = "vcl 4.1;\nsub vcl_recv { set req.http.X-Route = \"1\"; }\n"
)

// acceptedVVP returns a policy at generation 1 with Accepted=True for it.
func acceptedVVP(name, route, cm string, created time.Time) *gatewayparamsv1alpha1.VarnishVCLPolicy {
	vvp := newVVP(name, route, cm, created)
	vvp.Generation = 1
	vvp.Status.Ancestors = []gatewayparamsv1alpha1.VarnishVCLPolicyAncestorStatus{{
		AncestorRef:    vvp.Spec.TargetRef,
		ControllerName: ControllerName,
		Conditions: []metav1.Condition{{
			Type: gatewayparamsv1alpha1.VCLPolicyConditionAccepted, Status: metav1.ConditionTrue,
			Reason: gatewayparamsv1alpha1.VCLPolicyReasonAccepted, ObservedGeneration: 1,
			LastTransitionTime: metav1.Now(),
		}},
	}}
	return vvp
}

func getCM(t *testing.T, r *GatewayReconciler, name string) *corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	if err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: vvpTestNS}, &cm); err != nil {
		t.Fatalf("Get(ConfigMap %s): %v", name, err)
	}
	return &cm
}

func reconcileGatewayResources(t *testing.T, r *GatewayReconciler) {
	t.Helper()
	var gw gatewayv1.Gateway
	if err := r.Get(context.Background(), types.NamespacedName{Name: "gw", Namespace: vvpTestNS}, &gw); err != nil {
		t.Fatalf("Get(gw): %v", err)
	}
	if err := r.reconcileResources(context.Background(), &gw); err != nil {
		t.Fatalf("reconcileResources: %v", err)
	}
}

func TestGatewayReconcile_RouteVCLEnabled(t *testing.T) {
	r := newTestReconciler(newTestScheme(), routeVCLWorld(4)...)
	reconcileGatewayResources(t, r)

	vclCM := getCM(t, r, "gw-vcl")
	if got := vclCM.Data[vcl.PoolSizeKey]; got != "4" {
		t.Errorf("pool size = %q, want 4", got)
	}
	if vclCM.Data[vcl.MainVCLKey] != vcl.GenerateMaster(4, testUserVCL) {
		t.Error("main.vcl is not GenerateMaster(4, userVCL)")
	}
	if !strings.Contains(vclCM.Data[vcl.MainVCLKey], "return (vcl(gwvcl-3))") {
		t.Error("main.vcl has no dispatch for slot 3")
	}

	routeCM := getCM(t, r, "gw-route-vcl")
	if ref := metav1.GetControllerOf(routeCM); ref == nil || ref.Kind != "Gateway" || ref.Name != "gw" {
		t.Errorf("controller reference = %+v, want Gateway gw", ref)
	}
	if routeCM.Labels[LabelManagedBy] != ManagedByValue {
		t.Errorf("labels = %v", routeCM.Labels)
	}
	m, err := vcl.ParseRouteVCLManifest(routeCM.Data[vcl.ManifestKey])
	if err != nil {
		t.Fatal(err)
	}
	s, ok := m.Slots["0"]
	if !ok || s.Policy != "default/p1" || s.Route != "default/r1" || s.Generation != 1 {
		t.Fatalf("manifest = %+v", m)
	}
	doc := routeCM.Data[vcl.SlotKey(0)]
	if doc != vcl.GenerateRouteVCL(testUserVCL, testRouteVCL) || s.Hash != vcl.RouteVCLHash(doc) {
		t.Error("slot 0 document/hash mismatch")
	}

	// A second reconcile writes nothing.
	rv1, rv2 := vclCM.ResourceVersion, routeCM.ResourceVersion
	reconcileGatewayResources(t, r)
	if getCM(t, r, "gw-vcl").ResourceVersion != rv1 || getCM(t, r, "gw-route-vcl").ResourceVersion != rv2 {
		t.Error("no-op reconcile rewrote a ConfigMap")
	}

	// A policy document change keeps slot 0 and changes the hash.
	var cm corev1.ConfigMap
	if err := r.Get(context.Background(), types.NamespacedName{Name: "vcl1", Namespace: vvpTestNS}, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["route.vcl"] = testRouteVCL + "# v2\n"
	if err := r.Update(context.Background(), &cm); err != nil {
		t.Fatal(err)
	}
	reconcileGatewayResources(t, r)
	m2, _ := vcl.ParseRouteVCLManifest(getCM(t, r, "gw-route-vcl").Data[vcl.ManifestKey])
	if m2.Slots["0"].Policy != "default/p1" || m2.Slots["0"].Hash == s.Hash {
		t.Errorf("after document change: %+v", m2)
	}
}

func TestGatewayReconcile_RouteVCLDisabled(t *testing.T) {
	// Feature off, yet a (stale) Accepted policy exists: no slots, and
	// main.vcl is exactly what it was before route VCL existed.
	r := newTestReconciler(newTestScheme(), routeVCLWorld(0)...)
	reconcileGatewayResources(t, r)

	vclCM := getCM(t, r, "gw-vcl")
	if vclCM.Data[vcl.MainVCLKey] != vcl.Merge(vcl.Generate(), testUserVCL) {
		t.Error("main.vcl with route VCL off differs from Merge(Generate(), userVCL)")
	}
	if got, ok := vclCM.Data[vcl.PoolSizeKey]; !ok || got != "0" {
		t.Errorf("pool size = %q (present %v), want \"0\"", got, ok)
	}
	routeCM := getCM(t, r, "gw-route-vcl")
	if len(routeCM.Data) != 1 {
		t.Errorf("route VCL ConfigMap data = %v, want only the manifest", routeCM.Data)
	}
	m, err := vcl.ParseRouteVCLManifest(routeCM.Data[vcl.ManifestKey])
	if err != nil || len(m.Slots) != 0 || len(m.Unassigned) != 0 {
		t.Errorf("manifest = %+v, %v", m, err)
	}
}

func TestGatewayReconcile_PoolResizeWritesMasterAndPoolTogether(t *testing.T) {
	scheme := newTestScheme()
	r := newTestReconciler(scheme, routeVCLWorld(4)...)
	reconcileGatewayResources(t, r)

	var params gatewayparamsv1alpha1.GatewayClassParameters
	if err := r.Get(context.Background(), types.NamespacedName{Name: "params"}, &params); err != nil {
		t.Fatal(err)
	}
	params.Spec.RouteVCL.SlotPoolSize = ptr.To(int32(16))
	if err := r.Update(context.Background(), &params); err != nil {
		t.Fatal(err)
	}
	before := getCM(t, r, "gw-vcl").ResourceVersion
	reconcileGatewayResources(t, r)
	after := getCM(t, r, "gw-vcl")
	if after.Data[vcl.PoolSizeKey] != "16" || after.Data[vcl.MainVCLKey] != vcl.GenerateMaster(16, testUserVCL) {
		t.Error("pool size and master not both updated")
	}
	if after.ResourceVersion == before {
		t.Error("ConfigMap not updated")
	}
	// The master and pool size went out in one update: exactly one write.
	if n := mustAtoi(t, after.ResourceVersion) - mustAtoi(t, before); n != 1 {
		t.Errorf("gw-vcl updated %d times, want 1", n)
	}
	if !strings.Contains(after.Data["routing.json"], "vhosts") {
		t.Error("routing.json lost")
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("resourceVersion %q: %v", s, err)
	}
	return n
}

func TestGatewayReconcile_RouteVCLEligibility(t *testing.T) {
	objs := routeVCLWorld(4)
	// p2: not accepted for its current generation (stale observedGeneration).
	p2 := acceptedVVP("p2", "r2", "vcl1", slotTestEpoch.Add(time.Minute))
	p2.Generation = 2
	// p3: target route not attached to gw.
	p3 := acceptedVVP("p3", "r3", "vcl1", slotTestEpoch.Add(2*time.Minute))
	// p4: ConfigMap missing.
	p4 := acceptedVVP("p4", "r4", "missing", slotTestEpoch.Add(3*time.Minute))
	// p5: same route as p1, newer (stale status during a conflict).
	p5 := acceptedVVP("p5", "r1", "vcl1", slotTestEpoch.Add(4*time.Minute))
	// p6: accepted, in another namespace, route attached cross-namespace.
	p6 := acceptedVVP("p6", "r6", "vcl6", slotTestEpoch.Add(5*time.Minute))
	p6.Namespace = "team"
	r6 := vvpRoute("r6")
	r6.Namespace = "team"
	r6.Spec.ParentRefs = []gatewayv1.ParentReference{{Name: "gw", Namespace: ptr.To(gatewayv1.Namespace(vvpTestNS))}}
	vcl6 := vvpConfigMap("vcl6", map[string]string{"route.vcl": testRouteVCL})
	vcl6.Namespace = "team"
	objs = append(objs, p2, vvpRoute("r2", "gw"), p3, vvpRoute("r3", "other"), p4, vvpRoute("r4", "gw"), p5, p6, r6, vcl6)

	r := newTestReconciler(newTestScheme(), objs...)
	reconcileGatewayResources(t, r)
	m, err := vcl.ParseRouteVCLManifest(getCM(t, r, "gw-route-vcl").Data[vcl.ManifestKey])
	if err != nil {
		t.Fatal(err)
	}
	assertSlots(t, m, map[string]int{"default/p1": 0, "team/p6": 1})
	if m.Slots["1"].Route != "team/r6" {
		t.Errorf("cross-namespace route = %q", m.Slots["1"].Route)
	}
}

func TestGatewayReconcile_RemovesStaleSlotKeys(t *testing.T) {
	stale := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-route-vcl", Namespace: vvpTestNS, Labels: map[string]string{LabelManagedBy: ManagedByValue}},
		Data: map[string]string{
			vcl.ManifestKey: `{"slots":{"5":{"policy":"default/gone","route":"default/gone","generation":1,"hash":"x"}}}`,
			vcl.SlotKey(5):  "old",
		},
	}
	r := newTestReconciler(newTestScheme(), append(routeVCLWorld(8), stale)...)
	reconcileGatewayResources(t, r)
	cm := getCM(t, r, "gw-route-vcl")
	if _, ok := cm.Data[vcl.SlotKey(5)]; ok {
		t.Error("stale slot-5.vcl kept")
	}
	m, _ := vcl.ParseRouteVCLManifest(cm.Data[vcl.ManifestKey])
	assertSlots(t, m, map[string]int{"default/p1": 0})
}

func TestEnqueueGatewaysForConfigMap_VVPDocument(t *testing.T) {
	r := newTestReconciler(newTestScheme(), routeVCLWorld(4)...)
	ctx := context.Background()

	reqs := r.gatewayRequestsForVVPConfigMap(ctx, vvpConfigMap("vcl1", nil))
	if len(reqs) != 1 || reqs[0].Name != "gw" {
		t.Errorf("requests for vcl1 = %v, want gw", reqs)
	}
	if reqs := r.gatewayRequestsForVVPConfigMap(ctx, vvpConfigMap("unrelated", nil)); len(reqs) != 0 {
		t.Errorf("requests for unrelated = %v", reqs)
	}
	other := vvpConfigMap("vcl1", nil)
	other.Namespace = "elsewhere"
	if reqs := r.gatewayRequestsForVVPConfigMap(ctx, other); len(reqs) != 0 {
		t.Errorf("requests for a same-named ConfigMap in another namespace = %v", reqs)
	}
}

func TestGatewayRequestsForVVP(t *testing.T) {
	r := newTestReconciler(newTestScheme(), routeVCLWorld(4)...)
	reqs := r.gatewayRequestsForVVP(context.Background(), acceptedVVP("p1", "r1", "vcl1", slotTestEpoch))
	if len(reqs) != 1 || reqs[0].Name != "gw" || reqs[0].Namespace != vvpTestNS {
		t.Errorf("requests = %v", reqs)
	}
	if reqs := r.gatewayRequestsForVVP(context.Background(), acceptedVVP("px", "nope", "vcl1", slotTestEpoch)); len(reqs) != 0 {
		t.Errorf("requests for a missing route = %v", reqs)
	}
}

func TestVVPSlotRelevantChange(t *testing.T) {
	p := vvpSlotRelevantChange()
	base := acceptedVVP("p", "r", "c", slotTestEpoch)

	podResults := base.DeepCopy()
	podResults.Status.PodResults = []gatewayparamsv1alpha1.VarnishVCLPolicyPodResult{{PodName: "x", Hash: "h"}}
	if p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: podResults}) {
		t.Error("podResults change passed")
	}
	spec := base.DeepCopy()
	spec.Generation = 2
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: spec}) {
		t.Error("generation change filtered")
	}
	flipped := base.DeepCopy()
	flipped.Status.Ancestors[0].Conditions[0].Status = metav1.ConditionFalse
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: flipped}) {
		t.Error("Accepted flip filtered")
	}
}
