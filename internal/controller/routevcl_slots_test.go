package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
	"github.com/varnish/gateway/internal/vcl"
)

// --- pure slot assignment ---

var slotTestEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// assignAt runs assignRouteVCLSlots at slotTestEpoch with no released slot
// cleared.
func assignAt(prev vcl.RouteVCLManifest, cands []routeVCLCandidate, poolSize int, userVCL string) routeVCLAssignment {
	return assignRouteVCLSlots(prev, cands, poolSize, userVCL, routeVCLRelease{now: slotTestEpoch})
}

// releasedOf returns slot -> policy for a manifest's Released list.
func releasedOf(m vcl.RouteVCLManifest) map[int]string {
	out := map[int]string{}
	for _, rl := range m.Released {
		out[rl.Slot] = rl.Policy
	}
	return out
}

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
	first := assignAt(vcl.RouteVCLManifest{}, cands, 8, "")
	assertSlots(t, first.manifest, map[string]int{"default/a": 0, "default/b": 1, "default/c": 2})

	// Same input, previous manifest from the first round: identical data.
	second := assignAt(first.manifest, cands, 8, "")
	if fmt.Sprint(second.data) != fmt.Sprint(first.data) {
		t.Errorf("second reconcile changed the ConfigMap data")
	}

	// "a" changes its document and generation: same slot, new hash.
	changed := []routeVCLCandidate{cand("c", 3, "c1"), cand("a", 1, "a2"), cand("b", 2, "b1")}
	changed[1].generation = 2
	third := assignAt(second.manifest, changed, 8, "")
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
	kept := assignAt(first.manifest, reordered, 8, "")
	assertSlots(t, kept.manifest, map[string]int{"default/a": 0, "default/b": 1, "default/c": 2})
}

// releaseRound runs assignRouteVCLSlots at slotTestEpoch+after with the given
// slots cleared (no live pod still runs their previous occupant).
func releaseRound(prev vcl.RouteVCLManifest, cands []routeVCLCandidate, poolSize int, after time.Duration, cleared ...int) routeVCLAssignment {
	rel := routeVCLRelease{now: slotTestEpoch.Add(after), cleared: map[int]bool{}}
	for _, s := range cleared {
		rel.cleared[s] = true
	}
	return assignRouteVCLSlots(prev, cands, poolSize, "", rel)
}

func TestAssignRouteVCLSlots_ReleasedSlotNotReusedInSameRound(t *testing.T) {
	prev := assignAt(vcl.RouteVCLManifest{},
		[]routeVCLCandidate{cand("a", 1, "a"), cand("b", 2, "b"), cand("c", 3, "c")}, 8, "").manifest

	// "a" (slot 0) goes away; "d" is new and older than everyone. Slot 0
	// is released, not reused: "d" gets the lowest slot that was never
	// occupied.
	next := assignAt(prev, []routeVCLCandidate{cand("b", 2, "b"), cand("c", 3, "c"), cand("d", 0, "d")}, 8, "")
	assertSlots(t, next.manifest, map[string]int{"default/b": 1, "default/c": 2, "default/d": 3})
	if got := releasedOf(next.manifest); len(got) != 1 || got[0] != "default/a" {
		t.Fatalf("released = %v, want slot 0 held for default/a", got)
	}
	rl := next.manifest.Released[0]
	if rl.Route != "default/r-a" || rl.Hash != prev.Slots["0"].Hash || !rl.ReleasedAt.Time.Equal(slotTestEpoch) {
		t.Errorf("released entry = %+v", rl)
	}
	if _, ok := next.data[vcl.SlotKey(0)]; ok {
		t.Error("released slot 0 still has a document")
	}
	if next.requeueAfter != routeVCLReleaseQuarantine {
		t.Errorf("requeueAfter = %v, want the quarantine %v", next.requeueAfter, routeVCLReleaseQuarantine)
	}
	// The released list round-trips through the chaperone's parser.
	m, err := vcl.ParseRouteVCLManifest(next.data[vcl.ManifestKey])
	if err != nil || len(m.Released) != 1 || !m.Released[0].ReleasedAt.Equal(&rl.ReleasedAt) {
		t.Errorf("ParseRouteVCLManifest = %+v, %v", m.Released, err)
	}

	// A new policy older than a retained one cannot take its slot.
	prev2 := assignAt(vcl.RouteVCLManifest{}, []routeVCLCandidate{cand("b", 2, "b")}, 8, "").manifest
	next2 := assignAt(prev2, []routeVCLCandidate{cand("a", 1, "a"), cand("b", 2, "b")}, 8, "")
	assertSlots(t, next2.manifest, map[string]int{"default/a": 1, "default/b": 0})
}

func TestAssignRouteVCLSlots_ReleasedSlotReusedAfterQuarantine(t *testing.T) {
	// Pool of 1: the waiting policy can only ever get slot 0.
	prev := assignAt(vcl.RouteVCLManifest{}, []routeVCLCandidate{cand("a", 1, "a")}, 1, "").manifest
	waiting := []routeVCLCandidate{cand("b", 2, "b")}

	released := releaseRound(prev, waiting, 1, 0)
	if len(released.manifest.Slots) != 0 || releasedOf(released.manifest)[0] != "default/a" {
		t.Fatalf("manifest = %+v, want slot 0 released and empty", released.manifest)
	}
	if un := unassignedOf(released.manifest); un["default/b"] != vcl.UnassignedNoSlotAvailable {
		t.Errorf("unassigned = %v", un)
	}

	// Cleared (old policy gone) but inside the quarantine: still held, and
	// the requeue points at the end of the quarantine.
	early := releaseRound(released.manifest, waiting, 1, 20*time.Second, 0)
	if len(early.manifest.Slots) != 0 || len(early.manifest.Released) != 1 {
		t.Fatalf("slot reused inside the quarantine: %+v", early.manifest)
	}
	if early.requeueAfter != routeVCLReleaseQuarantine-20*time.Second {
		t.Errorf("requeueAfter = %v, want %v", early.requeueAfter, routeVCLReleaseQuarantine-20*time.Second)
	}
	if !early.manifest.Released[0].ReleasedAt.Time.Equal(slotTestEpoch) {
		t.Error("ReleasedAt moved on a later round")
	}

	// Quarantine over and cleared: free, and "b" gets it in the same round.
	after := releaseRound(early.manifest, waiting, 1, routeVCLReleaseQuarantine, 0)
	assertSlots(t, after.manifest, map[string]int{"default/b": 0})
	if len(after.manifest.Released) != 0 || len(after.manifest.Unassigned) != 0 || after.requeueAfter != 0 {
		t.Errorf("manifest = %+v, requeue %v", after.manifest, after.requeueAfter)
	}
}

func TestAssignRouteVCLSlots_ReleasedSlotHeldWhilePodStillProgrammed(t *testing.T) {
	prev := assignAt(vcl.RouteVCLManifest{}, []routeVCLCandidate{cand("a", 1, "a")}, 1, "").manifest
	waiting := []routeVCLCandidate{cand("b", 2, "b")}
	released := releaseRound(prev, waiting, 1, 0).manifest

	// Quarantine long over, but not cleared (a live pod still reports
	// slot 0 programmed for "a"): held, polled.
	held := releaseRound(released, waiting, 1, 10*routeVCLReleaseQuarantine)
	if len(held.manifest.Slots) != 0 || releasedOf(held.manifest)[0] != "default/a" {
		t.Fatalf("slot reused while a pod still runs the old VCL: %+v", held.manifest)
	}
	if held.requeueAfter != routeVCLReleasePollInterval {
		t.Errorf("requeueAfter = %v, want the poll interval", held.requeueAfter)
	}
}

func TestGatewayReconcile_ReleasedSlotFollowsPodResults(t *testing.T) {
	// Pool 1: p1 holds slot 0; it is deleted and p2 waits for the slot.
	// p1's pods keep reporting slot 0 programmed until the chaperone writes
	// its final "not in manifest" entry.
	objs := routeVCLWorld(1)
	objs = append(objs,
		vvpRoute("r2", "gw"),
		acceptedVVP("p2", "r2", "vcl1", slotTestEpoch.Add(time.Minute)),
		gatewayPod("gw", "gw-pod-a"),
	)
	r := newTestReconciler(newTestScheme(), objs...)
	now := slotTestEpoch
	r.clock = func() time.Time { return now }
	reconcileGatewayResources(t, r)
	m := routeVCLManifestOf(t, r)
	assertSlots(t, m, map[string]int{"default/p1": 0})
	hash := m.Slots["0"].Hash

	// p1 is no longer accepted (explicitly rejected at its generation)
	// and its pod reports slot 0 programmed.
	p1 := getGatewayTestVVP(t, r, "p1")
	p1.Status.Ancestors[0].Conditions[0].Status = metav1.ConditionFalse
	p1.Status.PodResults = []gatewayparamsv1alpha1.VarnishVCLPolicyPodResult{
		{PodName: "gw-pod-a", Slot: 0, Hash: hash, Programmed: true, LastUpdate: metav1.Now()},
	}
	if err := r.Update(context.Background(), p1); err != nil {
		t.Fatal(err)
	}
	if after := reconcileGatewayResourcesRequeue(t, r); after != routeVCLReleaseQuarantine {
		t.Errorf("requeue = %v, want the quarantine", after)
	}
	m = routeVCLManifestOf(t, r)
	if len(m.Slots) != 0 || releasedOf(m)[0] != "default/p1" {
		t.Fatalf("manifest after release = %+v", m)
	}

	// After the quarantine the pod still reports slot 0 programmed: held.
	now = slotTestEpoch.Add(2 * routeVCLReleaseQuarantine)
	if after := reconcileGatewayResourcesRequeue(t, r); after != routeVCLReleasePollInterval {
		t.Errorf("requeue = %v, want the poll interval", after)
	}
	if m = routeVCLManifestOf(t, r); len(m.Slots) != 0 {
		t.Fatalf("slot reused while a live pod reports it programmed: %+v", m)
	}

	// An entry from a pod that is not live does not hold the slot, nor
	// does one for another slot; the live pod's final entry clears it.
	p1 = getGatewayTestVVP(t, r, "p1")
	p1.Status.PodResults = []gatewayparamsv1alpha1.VarnishVCLPolicyPodResult{
		{PodName: "gw-pod-a", Slot: 0, Hash: "", Programmed: false, Message: "not in manifest on this pod", LastUpdate: metav1.Now()},
		{PodName: "gw-pod-gone", Slot: 0, Hash: hash, Programmed: true, LastUpdate: metav1.Now()},
	}
	if err := r.Update(context.Background(), p1); err != nil {
		t.Fatal(err)
	}
	if after := reconcileGatewayResourcesRequeue(t, r); after != 0 {
		t.Errorf("requeue = %v, want none", after)
	}
	m = routeVCLManifestOf(t, r)
	assertSlots(t, m, map[string]int{"default/p2": 0})
	if len(m.Released) != 0 {
		t.Errorf("released = %+v", m.Released)
	}
}

func TestGatewayReconcile_ReleasedSlotOfDeletedPolicy(t *testing.T) {
	objs := routeVCLWorld(1)
	objs = append(objs, vvpRoute("r2", "gw"), acceptedVVP("p2", "r2", "vcl1", slotTestEpoch.Add(time.Minute)))
	r := newTestReconciler(newTestScheme(), objs...)
	now := slotTestEpoch
	r.clock = func() time.Time { return now }
	reconcileGatewayResources(t, r)

	if err := r.Delete(context.Background(), getGatewayTestVVP(t, r, "p1")); err != nil {
		t.Fatal(err)
	}
	reconcileGatewayResources(t, r)
	if m := routeVCLManifestOf(t, r); len(m.Slots) != 0 || len(m.Released) != 1 {
		t.Fatalf("manifest = %+v, want slot 0 released", m)
	}
	// The policy is gone, so only the quarantine applies.
	now = slotTestEpoch.Add(routeVCLReleaseQuarantine)
	reconcileGatewayResources(t, r)
	assertSlots(t, routeVCLManifestOf(t, r), map[string]int{"default/p2": 0})
}

func TestAssignRouteVCLSlots_ReturningPolicyGetsItsReleasedSlot(t *testing.T) {
	prev := assignAt(vcl.RouteVCLManifest{},
		[]routeVCLCandidate{cand("a", 1, "a"), cand("b", 2, "b")}, 4, "").manifest

	// "a" drops out (e.g. briefly not accepted): slot 0 released.
	gone := releaseRound(prev, []routeVCLCandidate{cand("b", 2, "b")}, 4, 0).manifest
	if releasedOf(gone)[0] != "default/a" {
		t.Fatalf("released = %v", releasedOf(gone))
	}

	// "a" comes back inside the quarantine, uncleared, with a changed
	// document; an older new policy "z" is also waiting. "a" gets slot 0
	// back at once; "z" does not get it.
	a2 := cand("a", 1, "a-v2")
	back := releaseRound(gone, []routeVCLCandidate{cand("z", 0, "z"), a2, cand("b", 2, "b")}, 4, time.Second)
	assertSlots(t, back.manifest, map[string]int{"default/a": 0, "default/b": 1, "default/z": 2})
	if len(back.manifest.Released) != 0 {
		t.Errorf("released = %+v, want none", back.manifest.Released)
	}

	// Same policy on a different route is not a return: the slot stays
	// in quarantine.
	moved := cand("a", 1, "a")
	moved.route = "default/other"
	notBack := releaseRound(gone, []routeVCLCandidate{moved, cand("b", 2, "b")}, 4, time.Second)
	assertSlots(t, notBack.manifest, map[string]int{"default/a": 2, "default/b": 1})
	if releasedOf(notBack.manifest)[0] != "default/a" {
		t.Errorf("released = %v, want slot 0 still held", releasedOf(notBack.manifest))
	}
}

func TestAssignRouteVCLSlots_Exhaustion(t *testing.T) {
	cands := []routeVCLCandidate{cand("a", 1, "a"), cand("b", 2, "b"), cand("c", 3, "c")}
	first := assignAt(vcl.RouteVCLManifest{}, cands, 2, "")
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
	second := assignAt(first.manifest, cands, 2, "")
	if len(second.newlyUnassigned) != 0 {
		t.Errorf("newlyUnassigned on the second round = %v, want none", second.newlyUnassigned)
	}
	if fmt.Sprint(second.data) != fmt.Sprint(first.data) {
		t.Error("second round changed the data")
	}
	// A slot is released: the waiting policy keeps waiting (same reason,
	// not logged again) until the release is over, then gets it.
	waiting := []routeVCLCandidate{cand("b", 2, "b"), cand("c", 3, "c")}
	third := assignAt(second.manifest, waiting, 2, "")
	assertSlots(t, third.manifest, map[string]int{"default/b": 1})
	if len(third.newlyUnassigned) != 0 {
		t.Errorf("newlyUnassigned = %v", third.newlyUnassigned)
	}
	fourth := releaseRound(third.manifest, waiting, 2, routeVCLReleaseQuarantine, 0)
	assertSlots(t, fourth.manifest, map[string]int{"default/b": 1, "default/c": 0})
	if len(fourth.manifest.Unassigned) != 0 {
		t.Errorf("unassigned = %v", fourth.manifest.Unassigned)
	}
}

func TestAssignRouteVCLSlots_ShrinkBelowAssignedSlot(t *testing.T) {
	cands := []routeVCLCandidate{cand("a", 1, "a"), cand("b", 2, "b"), cand("c", 3, "c"), cand("d", 4, "d")}
	full := assignAt(vcl.RouteVCLManifest{}, cands, 4, "").manifest
	assertSlots(t, full, map[string]int{"default/a": 0, "default/b": 1, "default/c": 2, "default/d": 3})

	// Pool 4 -> 2 with every slot in use: c and d lose their slots, a and b
	// keep their numbers.
	shrunk := assignAt(full, cands, 2, "")
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
	again := assignAt(shrunk.manifest, cands, 2, "")
	if fmt.Sprint(again.data) != fmt.Sprint(shrunk.data) {
		t.Error("result not stable after shrink")
	}

	// Pool 4 -> 2 with a free slot inside the new pool: the policy that
	// lost slot 3 is re-placed in slot 1; slot 0 does not move.
	sparse := assignAt(vcl.RouteVCLManifest{Slots: map[string]vcl.RouteVCLSlot{
		"0": {Policy: "default/a"}, "3": {Policy: "default/d"},
	}}, []routeVCLCandidate{cand("a", 1, "a"), cand("d", 4, "d")}, 2, "")
	assertSlots(t, sparse.manifest, map[string]int{"default/a": 0, "default/d": 1})
}

func TestAssignRouteVCLSlots_ComposedDocumentsAndHash(t *testing.T) {
	userVCL := "vcl 4.1;\nsub vcl_deliver { set resp.http.X-Gateway-User = \"1\"; }\n"
	routeVCL := "vcl 4.1;\nsub vcl_recv { set req.http.X-Route = \"1\"; }\n"
	a := assignAt(vcl.RouteVCLManifest{}, []routeVCLCandidate{cand("a", 1, routeVCL)}, 4, userVCL)

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
	b := assignAt(a.manifest, []routeVCLCandidate{cand("a", 1, routeVCL)}, 4, userVCL+"\n# changed\n")
	if b.manifest.Slots["0"].Hash == s.Hash {
		t.Error("hash did not change with the gateway user VCL")
	}
}

func TestAssignRouteVCLSlots_PoolZero(t *testing.T) {
	prev := assignAt(vcl.RouteVCLManifest{}, []routeVCLCandidate{cand("a", 1, "a")}, 4, "").manifest
	a := assignAt(prev, []routeVCLCandidate{cand("a", 1, "a")}, 0, "")
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
	a := assignAt(vcl.RouteVCLManifest{}, cands, 8, "")
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
	reconcileGatewayResourcesRequeue(t, r)
}

// reconcileGatewayResourcesRequeue reconciles gw and returns the requested
// requeue delay.
func reconcileGatewayResourcesRequeue(t *testing.T, r *GatewayReconciler) time.Duration {
	t.Helper()
	after, err := reconcileGatewayResourcesErr(r)
	if err != nil {
		t.Fatalf("reconcileResources: %v", err)
	}
	return after
}

func reconcileGatewayResourcesErr(r *GatewayReconciler) (time.Duration, error) {
	var gw gatewayv1.Gateway
	if err := r.Get(context.Background(), types.NamespacedName{Name: "gw", Namespace: vvpTestNS}, &gw); err != nil {
		return 0, err
	}
	return r.reconcileResources(context.Background(), &gw)
}

// routeVCLManifestOf returns the manifest in gw-route-vcl.
func routeVCLManifestOf(t *testing.T, r *GatewayReconciler) vcl.RouteVCLManifest {
	t.Helper()
	m, err := vcl.ParseRouteVCLManifest(getCM(t, r, "gw-route-vcl").Data[vcl.ManifestKey])
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func getGatewayTestVVP(t *testing.T, r *GatewayReconciler, name string) *gatewayparamsv1alpha1.VarnishVCLPolicy {
	t.Helper()
	var vvp gatewayparamsv1alpha1.VarnishVCLPolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: vvpTestNS}, &vvp); err != nil {
		t.Fatalf("Get(VarnishVCLPolicy %s): %v", name, err)
	}
	return &vvp
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
	markRouteAttached(r6)
	vcl6 := vvpConfigMap("vcl6", map[string]string{"route.vcl": testRouteVCL})
	vcl6.Namespace = "team"
	objs = append(objs, p2, vvpRoute("r2", "gw"), p3, vvpRoute("r3", "other"), p4, vvpRoute("r4", "gw"), p5, p6, r6, vcl6)
	// p7: route lists gw but has no parent status (never attached, e.g.
	// allowedRoutes refuse it). p8: route refused by gw's listeners.
	p7 := acceptedVVP("p7", "r7", "vcl1", slotTestEpoch.Add(6*time.Minute))
	r7 := vvpRoute("r7", "gw")
	r7.Status.Parents = nil
	p8 := acceptedVVP("p8", "r8", "vcl1", slotTestEpoch.Add(7*time.Minute))
	r8 := vvpRoute("r8", "gw")
	r8.Status.Parents[0].Conditions[0].Status = metav1.ConditionFalse
	objs = append(objs, p7, r7, p8, r8)

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
	// After a spec edit: no verdict for the new generation yet, then a
	// rejection. Neither is "accepted current", but a slot holder loses
	// its slot only on the rejection, so the change must pass.
	lagging := base.DeepCopy()
	lagging.Generation = 2
	rejected := lagging.DeepCopy()
	rejected.Status.Ancestors[0].Conditions[0].Status = metav1.ConditionFalse
	rejected.Status.Ancestors[0].Conditions[0].ObservedGeneration = 2
	if !p.Update(event.UpdateEvent{ObjectOld: lagging, ObjectNew: rejected}) {
		t.Error("unknown -> rejected filtered")
	}
}

func TestGatewayReconcile_SlotHolderSurvivesStatusLag(t *testing.T) {
	r := newTestReconciler(newTestScheme(), routeVCLWorld(4)...)
	reconcileGatewayResources(t, r)
	before := routeVCLManifestOf(t, r).Slots["0"]

	// Spec edit: generation 2, status still says Accepted for 1. The new
	// document is in place; the holder keeps slot 0 with the new bytes.
	var cm corev1.ConfigMap
	if err := r.Get(context.Background(), types.NamespacedName{Name: "vcl1", Namespace: vvpTestNS}, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["route.vcl"] = testRouteVCL + "# v2\n"
	if err := r.Update(context.Background(), &cm); err != nil {
		t.Fatal(err)
	}
	p1 := getGatewayTestVVP(t, r, "p1")
	p1.Generation = 2
	if err := r.Update(context.Background(), p1); err != nil {
		t.Fatal(err)
	}
	if got := getGatewayTestVVP(t, r, "p1"); got.Generation != 2 {
		t.Fatalf("fake client did not keep generation 2 (got %d)", got.Generation)
	}
	reconcileGatewayResources(t, r)
	m := routeVCLManifestOf(t, r)
	assertSlots(t, m, map[string]int{"default/p1": 0})
	if m.Slots["0"].Hash == before.Hash || m.Slots["0"].Generation != 2 {
		t.Errorf("slot 0 = %+v, want the new document at generation 2", m.Slots["0"])
	}
	if len(m.Released) != 0 {
		t.Errorf("released = %+v", m.Released)
	}

	// The VVP controller then rejects generation 2: the slot is released.
	p1 = getGatewayTestVVP(t, r, "p1")
	p1.Status.Ancestors[0].Conditions[0].Status = metav1.ConditionFalse
	p1.Status.Ancestors[0].Conditions[0].ObservedGeneration = 2
	if err := r.Update(context.Background(), p1); err != nil {
		t.Fatal(err)
	}
	reconcileGatewayResources(t, r)
	m = routeVCLManifestOf(t, r)
	if len(m.Slots) != 0 || releasedOf(m)[0] != "default/p1" {
		t.Errorf("manifest after rejection = %+v", m)
	}
}

func TestGatewayReconcile_SlotHolderKeepsDocumentWhileConfigMapMissing(t *testing.T) {
	objs := routeVCLWorld(4)
	r := newTestReconciler(newTestScheme(), objs...)
	reconcileGatewayResources(t, r)
	before := getCM(t, r, "gw-route-vcl")
	prevDoc := before.Data[vcl.SlotKey(0)]

	// The policy's ConfigMap vanishes (status not updated yet). A new
	// accepted policy pointing at the same missing ConfigMap is left out.
	if err := r.Delete(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "vcl1", Namespace: vvpTestNS}}); err != nil {
		t.Fatal(err)
	}
	for _, o := range []runtime.Object{vvpRoute("r2", "gw"), acceptedVVP("p2", "r2", "vcl1", slotTestEpoch.Add(time.Minute))} {
		if err := r.Create(context.Background(), o.(client.Object)); err != nil {
			t.Fatal(err)
		}
	}
	reconcileGatewayResources(t, r)
	after := getCM(t, r, "gw-route-vcl")
	m := routeVCLManifestOf(t, r)
	assertSlots(t, m, map[string]int{"default/p1": 0})
	if after.Data[vcl.SlotKey(0)] != prevDoc || m.Slots["0"].Hash != vcl.RouteVCLHash(prevDoc) {
		t.Error("previous slot document not kept")
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Error("ConfigMap rewritten although nothing changed")
	}
}

func TestGatewayReconcile_CorruptManifestFailsReconcile(t *testing.T) {
	corrupt := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-route-vcl", Namespace: vvpTestNS, Labels: map[string]string{LabelManagedBy: ManagedByValue}},
		Data: map[string]string{
			vcl.ManifestKey: `{"slots":{"3":{"policy":"default/p1"`,
			vcl.SlotKey(3):  "old",
		},
	}
	r := newTestReconciler(newTestScheme(), append(routeVCLWorld(4), corrupt)...)
	if _, err := reconcileGatewayResourcesErr(r); err == nil || !strings.Contains(err.Error(), "route VCL manifest") {
		t.Fatalf("reconcile error = %v, want a manifest parse error", err)
	}
	cm := getCM(t, r, "gw-route-vcl")
	if cm.Data[vcl.ManifestKey] != corrupt.Data[vcl.ManifestKey] || cm.Data[vcl.SlotKey(3)] != "old" {
		t.Errorf("corrupt manifest was overwritten: %v", cm.Data)
	}

	// Empty manifest key: start fresh.
	cm.Data = map[string]string{vcl.ManifestKey: ""}
	if err := r.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	reconcileGatewayResources(t, r)
	assertSlots(t, routeVCLManifestOf(t, r), map[string]int{"default/p1": 0})
}

func TestGatewayReconcile_UserVCLReadErrorFailsReconcile(t *testing.T) {
	scheme := newTestScheme()
	base := newTestReconciler(scheme, routeVCLWorld(4)...)
	reconcileGatewayResources(t, base)
	vclBefore := getCM(t, base, "gw-vcl").Data[vcl.MainVCLKey]
	routeBefore := getCM(t, base, "gw-route-vcl").Data

	failing := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.ConfigMap); ok && key.Name == "user-vcl" {
				return apierrors.NewServiceUnavailable("etcd is sad")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	r := &GatewayReconciler{
		Client: interceptor.NewClient(base.Client.(client.WithWatch), failing),
		Scheme: scheme, Config: base.Config, Logger: base.Logger,
	}
	if _, err := reconcileGatewayResourcesErr(r); err == nil || !strings.Contains(err.Error(), "user VCL") {
		t.Fatalf("reconcile error = %v, want the user VCL read error", err)
	}
	if getCM(t, base, "gw-vcl").Data[vcl.MainVCLKey] != vclBefore {
		t.Error("main.vcl rewritten without the user VCL")
	}
	if fmt.Sprint(getCM(t, base, "gw-route-vcl").Data) != fmt.Sprint(routeBefore) {
		t.Error("route VCL documents rewritten without the user VCL")
	}

	// A missing user VCL ConfigMap is a user action: "" and no error.
	if err := base.Delete(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "user-vcl", Namespace: vvpTestNS}}); err != nil {
		t.Fatal(err)
	}
	reconcileGatewayResources(t, base)
	if got := getCM(t, base, "gw-vcl").Data[vcl.MainVCLKey]; got != vcl.GenerateMaster(4, "") {
		t.Error("main.vcl with the user VCL ConfigMap gone is not GenerateMaster(4, \"\")")
	}
}
