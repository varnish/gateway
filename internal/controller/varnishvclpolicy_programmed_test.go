package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
	"github.com/varnish/gateway/internal/vcl"
)

const progHash = "1111111111111111111111111111111111111111111111111111111111111111"

// gatewayPod returns a running pod of Gateway gw (default namespace).
func gatewayPod(gw, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: vvpTestNS,
			Labels: gatewayLabels(&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: gw, Namespace: vvpTestNS}}),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// slotManifestCM returns gw's route VCL ConfigMap with policy "default/p" in
// slot 2 (hash progHash), or listed as unassigned when reason is non-empty.
func slotManifestCM(gw, reason string) *corev1.ConfigMap {
	m := `{"slots":{"2":{"policy":"default/p","route":"default/my-route","generation":1,"hash":"` + progHash + `"}}}`
	if reason != "" {
		m = `{"slots":{},"unassigned":[{"policy":"default/p","route":"default/my-route","reason":"` + reason + `"}]}`
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: gw + vcl.RouteVCLConfigMapSuffix, Namespace: vvpTestNS,
			Labels: map[string]string{LabelManagedBy: ManagedByValue, LabelGatewayName: gw},
		},
		Data: map[string]string{vcl.ManifestKey: m},
	}
}

func podResult(pod, hash string, programmed bool, msg string) gatewayparamsv1alpha1.VarnishVCLPolicyPodResult {
	return gatewayparamsv1alpha1.VarnishVCLPolicyPodResult{
		PodName: pod, Slot: 2, Hash: hash, Programmed: programmed, Message: msg, LastUpdate: metav1.Now(),
	}
}

func podResultSlot(pod string, slot int32, hash string, programmed bool, msg string) gatewayparamsv1alpha1.VarnishVCLPolicyPodResult {
	res := podResult(pod, hash, programmed, msg)
	res.Slot = slot
	return res
}

func vvpWithResults(results ...gatewayparamsv1alpha1.VarnishVCLPolicyPodResult) *gatewayparamsv1alpha1.VarnishVCLPolicy {
	vvp := newVVP("p", "my-route", "vcl", time.Now())
	vvp.Generation = 1
	vvp.Status.PodResults = results
	return vvp
}

func getVVP(t *testing.T, r *VarnishVCLPolicyReconciler, name string) *gatewayparamsv1alpha1.VarnishVCLPolicy {
	t.Helper()
	var vvp gatewayparamsv1alpha1.VarnishVCLPolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: vvpTestNS}, &vvp); err != nil {
		t.Fatalf("Get(%s): %v", name, err)
	}
	return &vvp
}

func programmedOf(t *testing.T, vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) metav1.Condition {
	t.Helper()
	if len(vvp.Status.Ancestors) == 0 {
		t.Fatal("no ancestors")
	}
	c := meta.FindStatusCondition(vvp.Status.Ancestors[0].Conditions, gatewayparamsv1alpha1.VCLPolicyConditionProgrammed)
	if c == nil {
		t.Fatal("no Programmed condition")
	}
	return *c
}

func TestVVPProgrammed(t *testing.T) {
	pods := []runtime.Object{gatewayPod("gw", "gw-a"), gatewayPod("gw", "gw-b")}
	cases := []struct {
		name       string
		extra      []runtime.Object
		results    []gatewayparamsv1alpha1.VarnishVCLPolicyPodResult
		status     metav1.ConditionStatus
		reason     string
		msgHas     string
		wantSlots  string
		wantPodRes []string
	}{
		{
			name:   "no manifest yet",
			status: metav1.ConditionUnknown, reason: gatewayparamsv1alpha1.VCLPolicyReasonNotProgrammed,
			msgHas: "not in the route VCL manifest", wantSlots: "[]",
		},
		{
			name:   "slot, no pod has reported",
			extra:  []runtime.Object{slotManifestCM("gw", "")},
			status: metav1.ConditionFalse, reason: gatewayparamsv1alpha1.VCLPolicyReasonPending,
			msgHas: "programmed on 0 of 2", wantSlots: "[default/gw=2]",
		},
		{
			name:    "one pod on a stale hash",
			extra:   []runtime.Object{slotManifestCM("gw", "")},
			results: []gatewayparamsv1alpha1.VarnishVCLPolicyPodResult{podResult("gw-a", progHash, true, ""), podResult("gw-b", "old", true, "")},
			status:  metav1.ConditionFalse, reason: gatewayparamsv1alpha1.VCLPolicyReasonPending,
			msgHas: "programmed on 1 of 2", wantSlots: "[default/gw=2]", wantPodRes: []string{"gw-a", "gw-b"},
		},
		{
			name:    "every live pod programmed",
			extra:   []runtime.Object{slotManifestCM("gw", "")},
			results: []gatewayparamsv1alpha1.VarnishVCLPolicyPodResult{podResult("gw-a", progHash, true, ""), podResult("gw-b", progHash, true, "")},
			status:  metav1.ConditionTrue, reason: gatewayparamsv1alpha1.VCLPolicyReasonProgrammed,
			msgHas: "slot 2 programmed on 2 pod(s)", wantSlots: "[default/gw=2]", wantPodRes: []string{"gw-a", "gw-b"},
		},
		{
			name:  "compile failure on one pod",
			extra: []runtime.Object{slotManifestCM("gw", "")},
			results: []gatewayparamsv1alpha1.VarnishVCLPolicyPodResult{
				podResult("gw-a", progHash, true, ""),
				podResult("gw-b", progHash, false, "VCL compilation failed: Unknown variable 'req.htp'"),
			},
			status: metav1.ConditionFalse, reason: gatewayparamsv1alpha1.VCLPolicyReasonCompileFailed,
			msgHas: "pod gw-b: VCL compilation failed: Unknown variable 'req.htp'", wantSlots: "[default/gw=2]",
			wantPodRes: []string{"gw-a", "gw-b"},
		},
		{
			// The slot moved (2 -> now) while the document stayed the
			// same: gw-b still reports the same hash for its old slot 5,
			// which says nothing about label gwvcl-2. Neither its
			// "programmed" nor its failure counts.
			name:  "same hash reported for an old slot",
			extra: []runtime.Object{slotManifestCM("gw", "")},
			results: []gatewayparamsv1alpha1.VarnishVCLPolicyPodResult{
				podResult("gw-a", progHash, true, ""),
				podResultSlot("gw-b", 5, progHash, true, ""),
			},
			status: metav1.ConditionFalse, reason: gatewayparamsv1alpha1.VCLPolicyReasonPending,
			msgHas: "programmed on 1 of 2", wantSlots: "[default/gw=2]", wantPodRes: []string{"gw-a", "gw-b"},
		},
		{
			name:  "failure reported for an old slot is not CompileFailed",
			extra: []runtime.Object{slotManifestCM("gw", "")},
			results: []gatewayparamsv1alpha1.VarnishVCLPolicyPodResult{
				podResult("gw-a", progHash, true, ""),
				podResultSlot("gw-b", 5, progHash, false, "label refused"),
			},
			status: metav1.ConditionFalse, reason: gatewayparamsv1alpha1.VCLPolicyReasonPending,
			msgHas: "programmed on 1 of 2", wantSlots: "[default/gw=2]", wantPodRes: []string{"gw-a", "gw-b"},
		},
		{
			name:   "pool exhausted",
			extra:  []runtime.Object{slotManifestCM("gw", vcl.UnassignedNoSlotAvailable)},
			status: metav1.ConditionFalse, reason: gatewayparamsv1alpha1.VCLPolicyReasonNoSlotAvailable,
			msgHas: "no slot assigned: NoSlotAvailable", wantSlots: "[]",
		},
		{
			name:  "results of gone pods are GCed, live stale ones kept",
			extra: []runtime.Object{slotManifestCM("gw", "")},
			results: []gatewayparamsv1alpha1.VarnishVCLPolicyPodResult{
				podResult("gw-a", progHash, true, ""), podResult("gw-b", "stale", true, ""), podResult("gw-gone", progHash, true, ""),
			},
			status: metav1.ConditionFalse, reason: gatewayparamsv1alpha1.VCLPolicyReasonPending,
			wantSlots: "[default/gw=2]", wantPodRes: []string{"gw-a", "gw-b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := append(vvpEnabledWorld(), pods...)
			objs = append(objs, tc.extra...)
			objs = append(objs, vvpWithResults(tc.results...))
			r := newVVPTestReconciler(objs...)
			reconcileVVP(t, r, "p")

			vvp := getVVP(t, r, "p")
			c := programmedOf(t, vvp)
			if c.Status != tc.status || c.Reason != tc.reason || !strings.Contains(c.Message, tc.msgHas) {
				t.Errorf("Programmed = %s/%s %q, want %s/%s containing %q", c.Status, c.Reason, c.Message, tc.status, tc.reason, tc.msgHas)
			}
			if c.ObservedGeneration != 1 {
				t.Errorf("observedGeneration = %d", c.ObservedGeneration)
			}
			var slots []string
			for _, s := range vvp.Status.Slots {
				slots = append(slots, fmt.Sprintf("%s=%d", s.Gateway, s.Slot))
			}
			if got := "[" + strings.Join(slots, " ") + "]"; got != tc.wantSlots {
				t.Errorf("slots = %s, want %s", got, tc.wantSlots)
			}
			var pr []string
			for _, res := range vvp.Status.PodResults {
				pr = append(pr, res.PodName)
			}
			if fmt.Sprint(pr) != fmt.Sprint(tc.wantPodRes) {
				t.Errorf("podResults = %v, want %v", pr, tc.wantPodRes)
			}

			// A second reconcile with nothing changed writes nothing.
			rv := vvp.ResourceVersion
			reconcileVVP(t, r, "p")
			if got := getVVP(t, r, "p").ResourceVersion; got != rv {
				t.Errorf("no-op reconcile wrote status (%s -> %s)", rv, got)
			}
		})
	}
}

func TestVVPProgrammed_NotAccepted(t *testing.T) {
	objs := []runtime.Object{
		vvpGatewayClass("varnish", "params"),
		vvpParams("params", &gatewayparamsv1alpha1.RouteVCLConfig{Enabled: true}),
		vvpGateway("gw", "varnish"), vvpRoute("my-route", "gw"),
		gatewayPod("gw", "gw-a"), slotManifestCM("gw", ""),
		// No "vcl" ConfigMap: ConfigMapNotFound.
		vvpWithResults(podResult("gw-a", progHash, true, ""), podResult("gw-gone", progHash, true, "")),
	}
	r := newVVPTestReconciler(objs...)
	reconcileVVP(t, r, "p")
	vvp := getVVP(t, r, "p")
	c := programmedOf(t, vvp)
	if c.Status != metav1.ConditionFalse || c.Reason != gatewayparamsv1alpha1.VCLPolicyReasonNotAccepted {
		t.Errorf("Programmed = %s/%s, want False/NotAccepted", c.Status, c.Reason)
	}
	if len(vvp.Status.Slots) != 0 {
		t.Errorf("slots = %v, want none", vvp.Status.Slots)
	}
	if len(vvp.Status.PodResults) != 1 || vvp.Status.PodResults[0].PodName != "gw-a" {
		t.Errorf("podResults = %+v, want only the live pod", vvp.Status.PodResults)
	}
}

func TestVVPProgrammed_TerminatingPodIsNotLive(t *testing.T) {
	terminating := gatewayPod("gw", "gw-old")
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"test/keep"} // the fake client refuses a deletionTimestamp without one
	failed := gatewayPod("gw", "gw-failed")
	failed.Status.Phase = corev1.PodFailed
	otherGW := gatewayPod("other", "other-a")

	objs := append(vvpEnabledWorld(), gatewayPod("gw", "gw-a"), terminating, failed, otherGW, slotManifestCM("gw", ""),
		vvpWithResults(podResult("gw-a", progHash, true, "")))
	r := newVVPTestReconciler(objs...)
	reconcileVVP(t, r, "p")
	if c := programmedOf(t, getVVP(t, r, "p")); c.Status != metav1.ConditionTrue {
		t.Errorf("Programmed = %s/%s %q, want True (terminating, failed and other-gateway pods do not count)", c.Status, c.Reason, c.Message)
	}
}

func TestVVPProgrammed_MultipleGateways(t *testing.T) {
	base := func(results ...gatewayparamsv1alpha1.VarnishVCLPolicyPodResult) []runtime.Object {
		return []runtime.Object{
			vvpGatewayClass("varnish", "params"),
			vvpParams("params", &gatewayparamsv1alpha1.RouteVCLConfig{Enabled: true}),
			vvpGateway("gw1", "varnish"), vvpGateway("gw2", "varnish"),
			vvpRoute("my-route", "gw1", "gw2"),
			vvpConfigMap("vcl", map[string]string{"route.vcl": "x"}),
			gatewayPod("gw1", "gw1-a"), gatewayPod("gw2", "gw2-a"),
			vvpWithResults(results...),
		}
	}
	cases := []struct {
		name    string
		objs    []runtime.Object
		reason  string
		msgHas  string
		slotLen int
	}{
		{
			name:    "both programmed",
			objs:    append(base(podResult("gw1-a", progHash, true, ""), podResult("gw2-a", progHash, true, "")), slotManifestCM("gw1", ""), slotManifestCM("gw2", "")),
			reason:  gatewayparamsv1alpha1.VCLPolicyReasonProgrammed,
			slotLen: 2,
		},
		{
			name:    "one compile failure wins",
			objs:    append(base(podResult("gw1-a", progHash, true, ""), podResult("gw2-a", progHash, false, "boom")), slotManifestCM("gw1", ""), slotManifestCM("gw2", "")),
			reason:  gatewayparamsv1alpha1.VCLPolicyReasonCompileFailed,
			msgHas:  "Gateway default/gw2: slot 2, pod gw2-a: boom",
			slotLen: 2,
		},
		{
			name:    "one done, one not listed yet",
			objs:    append(base(podResult("gw1-a", progHash, true, "")), slotManifestCM("gw1", "")),
			reason:  gatewayparamsv1alpha1.VCLPolicyReasonPending,
			msgHas:  "Gateway default/gw2: not in the route VCL manifest yet",
			slotLen: 1,
		},
		{
			name:    "one done, one out of slots",
			objs:    append(base(podResult("gw1-a", progHash, true, "")), slotManifestCM("gw1", ""), slotManifestCM("gw2", vcl.UnassignedSlotOutsidePool)),
			reason:  gatewayparamsv1alpha1.VCLPolicyReasonNoSlotAvailable,
			msgHas:  "SlotOutsidePool",
			slotLen: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newVVPTestReconciler(tc.objs...)
			reconcileVVP(t, r, "p")
			vvp := getVVP(t, r, "p")
			c := programmedOf(t, vvp)
			if c.Reason != tc.reason || !strings.Contains(c.Message, tc.msgHas) {
				t.Errorf("Programmed = %s/%s %q, want reason %s containing %q", c.Status, c.Reason, c.Message, tc.reason, tc.msgHas)
			}
			if len(vvp.Status.Slots) != tc.slotLen {
				t.Errorf("slots = %v, want %d", vvp.Status.Slots, tc.slotLen)
			}
		})
	}
}

func TestVVPProgrammed_DisabledGatewayIgnored(t *testing.T) {
	// gw1 has route VCL on, gw2's class has it off: only gw1 counts, and
	// gw2's pods are still live for podResults GC.
	objs := []runtime.Object{
		vvpGatewayClass("varnish", "params"),
		vvpParams("params", &gatewayparamsv1alpha1.RouteVCLConfig{Enabled: true}),
		vvpGatewayClass("plain", ""),
		vvpGateway("gw1", "varnish"), vvpGateway("gw2", "plain"),
		vvpRoute("my-route", "gw1", "gw2"),
		vvpConfigMap("vcl", map[string]string{"route.vcl": "x"}),
		gatewayPod("gw1", "gw1-a"), gatewayPod("gw2", "gw2-a"), slotManifestCM("gw1", ""),
		vvpWithResults(podResult("gw1-a", progHash, true, ""), podResult("gw2-a", "x", false, "stale")),
	}
	r := newVVPTestReconciler(objs...)
	reconcileVVP(t, r, "p")
	vvp := getVVP(t, r, "p")
	if c := programmedOf(t, vvp); c.Status != metav1.ConditionTrue {
		t.Errorf("Programmed = %s/%s %q, want True", c.Status, c.Reason, c.Message)
	}
	if len(vvp.Status.PodResults) != 2 {
		t.Errorf("podResults = %v, want both kept", vvp.Status.PodResults)
	}
}

func TestVVPProgrammed_ReleasedSlotHolderPodsKeptForGC(t *testing.T) {
	// The policy was retargeted away from "old-gw", which still holds its
	// slot 3 in Released. old-gw's pod entry is what the Gateway controller
	// checks before freeing the slot, so GC must keep it although old-gw is
	// no longer a parent. A pod of an unrelated gateway is dropped.
	old := slotManifestCM("old-gw", "")
	old.Data[vcl.ManifestKey] = `{"slots":{},"released":[{"slot":3,"policy":"default/p","route":"default/was","hash":"h","releasedAt":"2026-01-01T00:00:00Z"}]}`
	unrelated := slotManifestCM("x-gw", "")
	unrelated.Data[vcl.ManifestKey] = `{"slots":{}}`
	objs := append(vvpEnabledWorld(),
		gatewayPod("gw", "gw-a"), slotManifestCM("gw", ""),
		old, gatewayPod("old-gw", "old-a"),
		unrelated, gatewayPod("x-gw", "x-a"),
		vvpWithResults(
			podResult("gw-a", progHash, true, ""),
			podResultSlot("old-a", 3, "h", true, ""),
			podResultSlot("x-a", 1, "h", true, ""),
		),
	)
	r := newVVPTestReconciler(objs...)
	reconcileVVP(t, r, "p")
	var got []string
	for _, res := range getVVP(t, r, "p").Status.PodResults {
		got = append(got, res.PodName)
	}
	if fmt.Sprint(got) != "[gw-a old-a]" {
		t.Errorf("podResults = %v, want [gw-a old-a]", got)
	}
}

func TestRouteGenerationOrParentsChanged(t *testing.T) {
	p := routeGenerationOrParentsChanged()
	base := vvpRoute("r", "gw")
	base.Status.Parents = nil

	attached := base.DeepCopy()
	markRouteAttached(attached)
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: attached}) {
		t.Error("status.parents change filtered")
	}
	same := base.DeepCopy()
	same.ResourceVersion = "2"
	if p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: same}) {
		t.Error("no-op update passed")
	}
	gen := base.DeepCopy()
	gen.Generation = 5
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: gen}) {
		t.Error("generation change filtered")
	}
}

func TestVVPGenerationOrPodResultsChanged(t *testing.T) {
	p := vvpGenerationOrPodResultsChanged()
	base := vvpWithResults(podResult("a", progHash, false, ""))
	base.Status.Ancestors = []gatewayparamsv1alpha1.VarnishVCLPolicyAncestorStatus{{ControllerName: ControllerName}}

	conds := base.DeepCopy()
	conds.Status.Ancestors[0].Conditions = []metav1.Condition{{Type: "Programmed", Status: metav1.ConditionTrue}}
	conds.Status.Slots = []gatewayparamsv1alpha1.VarnishVCLPolicySlot{{Gateway: "default/gw", Slot: 1}}
	if p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: conds}) {
		t.Error("our own condition/slots write passed the predicate")
	}
	res := base.DeepCopy()
	res.Status.PodResults[0].Programmed = true
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: res}) {
		t.Error("podResults change filtered")
	}
	added := base.DeepCopy()
	added.Status.PodResults = append(added.Status.PodResults, podResult("b", progHash, true, ""))
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: added}) {
		t.Error("new podResults entry filtered")
	}
	gen := base.DeepCopy()
	gen.Generation = 2
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: gen}) {
		t.Error("generation change filtered")
	}
	if !p.Create(event.CreateEvent{Object: base}) || !p.Delete(event.DeleteEvent{Object: base}) {
		t.Error("create/delete filtered")
	}
}

func TestFindVVPsForConfigMap_RouteVCLManifest(t *testing.T) {
	r := newVVPTestReconciler(append(vvpEnabledWorld(), newVVP("p", "my-route", "vcl", time.Now()))...)
	cm := slotManifestCM("gw", "")
	cm.Data[vcl.ManifestKey] = `{"slots":{"0":{"policy":"team/a","route":"team/r"},"1":{"policy":"default/b","route":"default/r"}},` +
		`"unassigned":[{"policy":"other/c","route":"other/r","reason":"NoSlotAvailable"}],` +
		`"released":[{"slot":2,"policy":"old/d","route":"old/r","hash":"h","releasedAt":"2026-01-01T00:00:00Z"}]}`
	got := requestNames(t, r.findVVPsForConfigMap(context.Background(), cm))
	sort.Strings(got)
	want := []string{"default/b", "old/d", "other/c", "team/a"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("requests = %v, want %v", got, want)
	}

	// A plain ConfigMap still maps to same-namespace referrers.
	if got := requestNames(t, r.findVVPsForConfigMap(context.Background(), vvpConfigMap("vcl", nil))); fmt.Sprint(got) != "[default/p]" {
		t.Errorf("plain ConfigMap requests = %v", got)
	}
}

func TestFindVVPsForDeployment(t *testing.T) {
	gw := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: vvpTestNS}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: vvpTestNS, Labels: gatewayLabels(gw)}}
	r := newVVPTestReconciler(slotManifestCM("gw", ""))
	if got := requestNames(t, r.findVVPsForDeployment(context.Background(), dep)); fmt.Sprint(got) != "[default/p]" {
		t.Errorf("requests = %v, want [default/p]", got)
	}
	foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: vvpTestNS, Labels: map[string]string{"app": "x"}}}
	if got := r.findVVPsForDeployment(context.Background(), foreign); len(got) != 0 {
		t.Errorf("foreign Deployment mapped to %v", got)
	}
	noManifest := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: vvpTestNS,
		Labels: gatewayLabels(&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: vvpTestNS}})}}
	if got := r.findVVPsForDeployment(context.Background(), noManifest); len(got) != 0 {
		t.Errorf("Deployment without a manifest mapped to %v", got)
	}
}

func TestTruncateMessage(t *testing.T) {
	if got := truncateMessage("short", 100); got != "short" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", 100) // 2 bytes per rune
	got := truncateMessage(long, 51)
	if len(got) > 51 || !strings.HasSuffix(got, "(truncated)") {
		t.Errorf("got %q (%d bytes)", got, len(got))
	}
	if !strings.HasPrefix(got, "é") || strings.ContainsRune(got, '�') {
		t.Errorf("cut inside a rune: %q", got)
	}
}
