package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
	"github.com/varnish/gateway/internal/vcl"
)

// routeVCLConfigMapMaxBytes is the size guard for the "{gateway}-route-vcl"
// ConfigMap. The API server refuses objects over 1 MiB; the guard leaves room
// for metadata and the unassigned list, which are not counted.
const routeVCLConfigMapMaxBytes = 900 * 1024

// routeVCLManifestEntryBytes approximates the JSON of one manifest slot entry
// beyond the policy and route names (slot key, field names, generation and
// the 64 hex digit hash). It only feeds the size guard.
const routeVCLManifestEntryBytes = 160

// routeVCLReleaseQuarantine is the minimum time a released slot stays out of
// the free list. The manifest, routing.json, the chaperone's relabel and
// ghost's reload are independent asynchronous writes; the quarantine (plus
// the podResults check) keeps a new occupant's route from being tagged with
// a slot whose label still points at the previous occupant's VCL.
const routeVCLReleaseQuarantine = 60 * time.Second

// routeVCLReleasePollInterval is how often a Gateway is re-reconciled while
// a released slot is past its quarantine but a live pod still reports the
// previous occupant programmed in it. podResults writes do not trigger the
// Gateway controller (vvpSlotRelevantChange filters them), so this polls.
const routeVCLReleasePollInterval = 10 * time.Second

// routeVCLPoolSize returns the number of route VCL slots for Gateways using
// params: 0 unless routeVCL.enabled, then slotPoolSize (default
// DefaultRouteVCLSlotPoolSize) clamped to MaxRouteVCLSlotPoolSize.
func routeVCLPoolSize(params *gatewayparamsv1alpha1.GatewayClassParameters) int {
	if params == nil || params.Spec.RouteVCL == nil || !params.Spec.RouteVCL.Enabled {
		return 0
	}
	n := gatewayparamsv1alpha1.DefaultRouteVCLSlotPoolSize
	if p := params.Spec.RouteVCL.SlotPoolSize; p != nil {
		n = int(*p)
	}
	if n < 0 {
		n = 0
	}
	if n > gatewayparamsv1alpha1.MaxRouteVCLSlotPoolSize {
		n = gatewayparamsv1alpha1.MaxRouteVCLSlotPoolSize
	}
	return n
}

// routeVCLConfigMapName returns the name of a Gateway's route VCL ConfigMap.
// The chaperone derives the same name from GATEWAY_NAME.
func routeVCLConfigMapName(gatewayName string) string {
	return gatewayName + vcl.RouteVCLConfigMapSuffix
}

// readRouteVCLManifest returns the manifest in a Gateway's route VCL
// ConfigMap. found is false when the ConfigMap does not exist. A manifest
// that does not parse is returned as an error.
func readRouteVCLManifest(ctx context.Context, c client.Reader, gwKey types.NamespacedName) (manifest vcl.RouteVCLManifest, found bool, err error) {
	var cm corev1.ConfigMap
	name := routeVCLConfigMapName(gwKey.Name)
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: gwKey.Namespace}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return vcl.RouteVCLManifest{}, false, nil
		}
		return vcl.RouteVCLManifest{}, false, fmt.Errorf("c.Get(ConfigMap %s/%s): %w", gwKey.Namespace, name, err)
	}
	m, err := vcl.ParseRouteVCLManifest(cm.Data[vcl.ManifestKey])
	if err != nil {
		return vcl.RouteVCLManifest{}, true, fmt.Errorf("vcl.ParseRouteVCLManifest(%s/%s): %w", gwKey.Namespace, name, err)
	}
	return m, true, nil
}

// isRouteVCLConfigMap reports whether obj looks like an operator-managed
// "{gateway}-route-vcl" ConfigMap.
func isRouteVCLConfigMap(obj client.Object) bool {
	return strings.HasSuffix(obj.GetName(), vcl.RouteVCLConfigMapSuffix) &&
		obj.GetLabels()[LabelManagedBy] == ManagedByValue
}

// parseManifestSlot parses a manifest slot key ("7"). Only canonical
// non-negative decimals are accepted, as in the chaperone.
func parseManifestSlot(key string) (int, bool) {
	n, err := strconv.Atoi(key)
	if err != nil || n < 0 || strconv.Itoa(n) != key {
		return 0, false
	}
	return n, true
}

// manifestPolicySlots returns policy -> slot for the valid slot keys of a
// manifest. A corrupt manifest could list a policy twice; the lowest slot is
// kept so the result is deterministic.
func manifestPolicySlots(m vcl.RouteVCLManifest) map[string]int {
	out := make(map[string]int, len(m.Slots))
	for key, s := range m.Slots {
		n, ok := parseManifestSlot(key)
		if !ok {
			continue
		}
		if old, dup := out[s.Policy]; !dup || n < old {
			out[s.Policy] = n
		}
	}
	return out
}

// routeVCLCandidate is a VarnishVCLPolicy eligible for a slot on a Gateway.
type routeVCLCandidate struct {
	policy     string // "<namespace>/<name>"
	route      string // "<namespace>/<name>"
	generation int64
	created    metav1.Time
	document   string // the route VCL as written by the user
	// composed, when non-empty, is an already composed slot document that
	// is used as is instead of composing document: a policy that holds a
	// slot keeps its previous bytes while its source cannot be read.
	composed string
}

// routeVCLRelease is the input for freeing released slots.
type routeVCLRelease struct {
	now time.Time
	// cleared[slot] is true when no live pod of the Gateway still reports
	// the released slot programmed for its previous occupant (or that policy
	// no longer exists). A slot that is not in the map is not cleared.
	cleared map[int]bool
}

// routeVCLAssignment is the computed content of a route VCL ConfigMap.
type routeVCLAssignment struct {
	manifest vcl.RouteVCLManifest
	// data is the full ConfigMap data: the manifest and one document per
	// occupied slot.
	data map[string]string
	// newlyUnassigned are Unassigned entries that were not in the previous
	// manifest with the same reason; they are logged once.
	newlyUnassigned []vcl.RouteVCLUnassigned
	// requeueAfter is when the earliest released slot may become free
	// (0 when nothing is released).
	requeueAfter time.Duration
}

// sortRouteVCLCandidates orders candidates by creation timestamp, then
// "<namespace>/<name>", so slot assignment is deterministic.
func sortRouteVCLCandidates(cands []routeVCLCandidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		if !cands[i].created.Equal(&cands[j].created) {
			return cands[i].created.Before(&cands[j].created)
		}
		return cands[i].policy < cands[j].policy
	})
}

// minPositive returns the smaller of two durations, treating 0 as unset.
func minPositive(a, b time.Duration) time.Duration {
	if a == 0 || (b > 0 && b < a) {
		return b
	}
	return a
}

// assignRouteVCLSlots computes a Gateway's route VCL ConfigMap from the
// previous manifest and the eligible policies.
//
//   - A policy that held slot s in prev keeps it while s < poolSize, even if
//     its document changed. Retained slots are honoured before anything new
//     is placed.
//   - A policy coming back (same policy and route) while its old slot is in
//     prev.Released gets that slot back at once.
//   - A slot whose occupant did not keep it (gone, no longer eligible, moved
//     by a pool shrink, ConfigMapFull) is not free: it goes into
//     manifest.Released with ReleasedAt = now. A released slot becomes free
//     only when routeVCLReleaseQuarantine has passed since ReleasedAt AND
//     rel.cleared says no live pod still runs the previous occupant in it.
//     Until then it is in neither Slots nor the free list, so the chaperone
//     points its label at the placeholder and routing.json drops it.
//   - Everyone else (new policies, and those whose slot fell outside a
//     reduced pool) gets the lowest free slot, in creation-timestamp then
//     namespace/name order.
//   - With no free slot left the policy is Unassigned: SlotOutsidePool if it
//     lost its slot to a pool shrink (now or earlier, the reason is sticky
//     while it waits), NoSlotAvailable otherwise.
//   - A document that would push the ConfigMap past
//     routeVCLConfigMapMaxBytes is Unassigned with ConfigMapFull and does not
//     consume a slot. Each document is checked on its own, so a smaller later
//     one can still fit.
//
// Documents are composed with the gateway user VCL (vcl.GenerateRouteVCL)
// and the manifest hash is vcl.RouteVCLHash of the composed document, which
// is what the chaperone verifies. With poolSize 0 the manifest is empty
// (Released included): the chaperone discards every label.
func assignRouteVCLSlots(prev vcl.RouteVCLManifest, candidates []routeVCLCandidate, poolSize int, gatewayUserVCL string, rel routeVCLRelease) routeVCLAssignment {
	manifest := vcl.RouteVCLManifest{Slots: map[string]vcl.RouteVCLSlot{}}
	data := map[string]string{}
	var requeue time.Duration

	if poolSize > 0 {
		cands := make([]routeVCLCandidate, len(candidates))
		copy(cands, candidates)
		sortRouteVCLCandidates(cands)

		prevSlot := manifestPolicySlots(prev)
		prevReason := make(map[string]string, len(prev.Unassigned))
		for _, u := range prev.Unassigned {
			prevReason[u.Policy] = u.Reason
		}
		occupiedPrev := make(map[int]bool, len(prev.Slots))
		for key := range prev.Slots {
			if n, ok := parseManifestSlot(key); ok {
				occupiedPrev[n] = true
			}
		}
		// Released slots by number. An entry for a slot that is occupied
		// in prev.Slots (corrupt) is ignored; of duplicates the latest
		// release wins (longest quarantine).
		prevReleased := make(map[int]vcl.RouteVCLReleased, len(prev.Released))
		for _, rl := range prev.Released {
			if rl.Slot < 0 || occupiedPrev[rl.Slot] {
				continue
			}
			if old, dup := prevReleased[rl.Slot]; dup && !old.ReleasedAt.Before(&rl.ReleasedAt) {
				continue
			}
			prevReleased[rl.Slot] = rl
		}
		releasedSlots := slices.Sorted(maps.Keys(prevReleased))

		used := make(map[int]bool, poolSize)
		decided := make(map[string]bool, len(cands))
		lostToShrink := make(map[string]bool)
		size := 0

		// place composes c's document and records it in slot, or reports
		// false when it does not fit under the size guard.
		place := func(c routeVCLCandidate, slot int) bool {
			doc := c.composed
			if doc == "" {
				doc = vcl.GenerateRouteVCL(gatewayUserVCL, c.document)
			}
			key := vcl.SlotKey(slot)
			cost := len(key) + len(doc) + len(c.policy) + len(c.route) + routeVCLManifestEntryBytes
			if size+cost > routeVCLConfigMapMaxBytes {
				return false
			}
			size += cost
			used[slot] = true
			data[key] = doc
			manifest.Slots[strconv.Itoa(slot)] = vcl.RouteVCLSlot{
				Policy:     c.policy,
				Route:      c.route,
				Generation: c.generation,
				Hash:       vcl.RouteVCLHash(doc),
			}
			return true
		}
		unassign := func(c routeVCLCandidate, reason string) {
			manifest.Unassigned = append(manifest.Unassigned, vcl.RouteVCLUnassigned{
				Policy: c.policy, Route: c.route, Reason: reason,
			})
		}

		// Pass 1: retained assignments.
		for _, c := range cands {
			s, ok := prevSlot[c.policy]
			if !ok {
				continue
			}
			if s >= poolSize {
				lostToShrink[c.policy] = true
				continue
			}
			if used[s] {
				continue // duplicate claim in a corrupt manifest; place as new
			}
			decided[c.policy] = true
			if !place(c, s) {
				unassign(c, vcl.UnassignedConfigMapFull)
			}
		}

		// Pass 1b: a policy coming back to its own released slot. Nothing
		// else can have run in that slot since, so no quarantine applies.
		reclaimed := make(map[int]bool)
		for _, c := range cands {
			if decided[c.policy] {
				continue
			}
			for _, slot := range releasedSlots {
				rl := prevReleased[slot]
				if rl.Policy != c.policy || rl.Route != c.route || slot >= poolSize || used[slot] {
					continue
				}
				decided[c.policy] = true
				if place(c, slot) {
					reclaimed[slot] = true
				} else {
					unassign(c, vcl.UnassignedConfigMapFull)
				}
				break
			}
		}

		// Release: slots whose previous occupant did not keep them, then
		// earlier releases that may not be freed yet.
		held := make(map[int]bool)
		var released []vcl.RouteVCLReleased
		releasedAt := metav1.NewTime(rel.now).Rfc3339Copy()
		for _, key := range slices.Sorted(maps.Keys(prev.Slots)) {
			n, ok := parseManifestSlot(key)
			if !ok || held[n] {
				continue
			}
			s := prev.Slots[key]
			if cur, ok := manifest.Slots[key]; ok && cur.Policy == s.Policy {
				continue
			}
			held[n] = true
			released = append(released, vcl.RouteVCLReleased{
				Slot: n, Policy: s.Policy, Route: s.Route, Hash: s.Hash, ReleasedAt: releasedAt,
			})
			requeue = minPositive(requeue, routeVCLReleaseQuarantine)
		}
		for _, slot := range releasedSlots {
			if reclaimed[slot] || held[slot] {
				continue
			}
			rl := prevReleased[slot]
			wait := routeVCLReleaseQuarantine - rel.now.Sub(rl.ReleasedAt.Time)
			if wait <= 0 && rel.cleared[slot] {
				continue // free
			}
			held[slot] = true
			released = append(released, rl)
			if wait > 0 {
				requeue = minPositive(requeue, wait)
			} else {
				requeue = minPositive(requeue, routeVCLReleasePollInterval)
			}
		}
		sort.Slice(released, func(i, j int) bool { return released[i].Slot < released[j].Slot })
		manifest.Released = released

		// Pass 2: everyone else, lowest free slot first.
		next := 0
		for _, c := range cands {
			if decided[c.policy] {
				continue
			}
			for next < poolSize && (used[next] || held[next]) {
				next++
			}
			if next >= poolSize {
				if lostToShrink[c.policy] || prevReason[c.policy] == vcl.UnassignedSlotOutsidePool {
					unassign(c, vcl.UnassignedSlotOutsidePool)
				} else {
					unassign(c, vcl.UnassignedNoSlotAvailable)
				}
				continue
			}
			if !place(c, next) {
				unassign(c, vcl.UnassignedConfigMapFull)
			}
		}

		sort.Slice(manifest.Unassigned, func(i, j int) bool {
			return manifest.Unassigned[i].Policy < manifest.Unassigned[j].Policy
		})
	}

	var newly []vcl.RouteVCLUnassigned
	prevUnassigned := make(map[vcl.RouteVCLUnassigned]bool, len(prev.Unassigned))
	for _, u := range prev.Unassigned {
		prevUnassigned[vcl.RouteVCLUnassigned{Policy: u.Policy, Reason: u.Reason}] = true
	}
	for _, u := range manifest.Unassigned {
		if !prevUnassigned[vcl.RouteVCLUnassigned{Policy: u.Policy, Reason: u.Reason}] {
			newly = append(newly, u)
		}
	}

	// encoding/json sorts map keys, so equal manifests marshal identically.
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		// Only strings, integers and times: cannot fail.
		panic(fmt.Sprintf("json.MarshalIndent(RouteVCLManifest): %v", err))
	}
	data[vcl.ManifestKey] = string(raw)

	return routeVCLAssignment{manifest: manifest, data: data, newlyUnassigned: newly, requeueAfter: requeue}
}

// VarnishVCLPolicy acceptance as seen by the slot assignment.
const (
	vvpAcceptanceUnknown  = iota // no current verdict (status lags the spec, or none yet)
	vvpAcceptanceAccepted        // Accepted=True for the current generation
	vvpAcceptanceRejected        // Accepted=False for the current generation
)

// vvpAcceptance reads the policy's Accepted condition as written by this
// controller. Only a verdict for the current generation counts; anything
// else (status not yet rewritten after a spec edit) is Unknown.
func vvpAcceptance(vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) int {
	if len(vvp.Status.Ancestors) == 0 {
		return vvpAcceptanceUnknown
	}
	anc := vvp.Status.Ancestors[0]
	if anc.ControllerName != ControllerName {
		return vvpAcceptanceUnknown
	}
	c := meta.FindStatusCondition(anc.Conditions, gatewayparamsv1alpha1.VCLPolicyConditionAccepted)
	if c == nil || c.ObservedGeneration != vvp.Generation {
		return vvpAcceptanceUnknown
	}
	switch c.Status {
	case metav1.ConditionTrue:
		return vvpAcceptanceAccepted
	case metav1.ConditionFalse:
		return vvpAcceptanceRejected
	}
	return vvpAcceptanceUnknown
}

// vvpAcceptedCurrent reports whether the policy's Accepted condition, as
// written by this controller for its current generation, is True.
func vvpAcceptedCurrent(vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) bool {
	return vvpAcceptance(vvp) == vvpAcceptanceAccepted
}

// vvpKey returns "<namespace>/<name>" for a policy.
func vvpKey(vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) string {
	return vvp.Namespace + "/" + vvp.Name
}

// routeAcceptedByGateway reports whether route is attached to the Gateway
// gwKey: its spec lists the Gateway in parentRefs and its status.parents has
// an entry for it, written by our controller, with Accepted=True. That entry
// is the HTTPRoute controller's verdict (allowedRoutes, listener matching),
// so a route a listener refuses does not count.
func routeAcceptedByGateway(route *gatewayv1.HTTPRoute, gwKey types.NamespacedName) bool {
	if !routeHasParentGatewayKey(route, gwKey) {
		return false
	}
	for _, ps := range route.Status.Parents {
		if string(ps.ControllerName) != ControllerName {
			continue
		}
		pr := ps.ParentRef
		if pr.Group != nil && *pr.Group != gatewayv1.GroupName {
			continue
		}
		if pr.Kind != nil && *pr.Kind != "Gateway" {
			continue
		}
		ns := route.Namespace
		if pr.Namespace != nil {
			ns = string(*pr.Namespace)
		}
		if string(pr.Name) != gwKey.Name || ns != gwKey.Namespace {
			continue
		}
		if meta.IsStatusConditionTrue(ps.Conditions, string(gatewayv1.RouteConditionAccepted)) {
			return true
		}
	}
	return false
}

// vvpDocument returns the policy's VCL document from its ConfigMap. ok is
// false when the ConfigMap or key is missing or the document is over
// MaxRouteVCLDocumentBytes.
func vvpDocument(ctx context.Context, c client.Reader, vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) (doc string, ok bool, err error) {
	cmKey := types.NamespacedName{Name: vvp.Spec.VCLConfigMapRef.Name, Namespace: vvp.Namespace}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, cmKey, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("c.Get(ConfigMap %s): %w", cmKey, err)
	}
	doc, ok = cm.Data[vvpConfigMapKey(vvp)]
	if !ok || len(doc) > gatewayparamsv1alpha1.MaxRouteVCLDocumentBytes {
		return "", false, nil
	}
	return doc, true, nil
}

// routeVCLCandidates lists the VarnishVCLPolicies that should hold a slot on
// gateway, from the live spec of each policy:
//
//   - the target is an HTTPRoute in the policy's namespace that is attached
//     to gateway (routeAcceptedByGateway);
//   - a policy that holds a slot in prev stays eligible unless its Accepted
//     condition is explicitly False for its current generation, so a spec
//     edit does not drop the slot while the VVP controller catches up; any
//     other policy needs Accepted=True for its current generation;
//   - the document is read from the referenced ConfigMap now. For a policy
//     that holds a slot and whose ConfigMap/key is missing (or oversized) at
//     the moment, the slot's previous composed document from prevData is
//     kept rather than freeing the slot; a new policy is left out.
//
// If two policies target the same route, only the older is kept.
func (r *GatewayReconciler) routeVCLCandidates(ctx context.Context, gateway *gatewayv1.Gateway, prev vcl.RouteVCLManifest, prevData map[string]string) ([]routeVCLCandidate, error) {
	var list gatewayparamsv1alpha1.VarnishVCLPolicyList
	if err := r.List(ctx, &list); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil // CRD not installed: no route VCL
		}
		return nil, fmt.Errorf("r.List(VarnishVCLPolicy): %w", err)
	}
	gwKey := client.ObjectKeyFromObject(gateway)
	held := manifestPolicySlots(prev)

	byRoute := make(map[string]*gatewayparamsv1alpha1.VarnishVCLPolicy)
	var order []string
	for i := range list.Items {
		vvp := &list.Items[i]
		if _, ok := held[vvpKey(vvp)]; ok {
			if vvpAcceptance(vvp) == vvpAcceptanceRejected {
				continue
			}
		} else if !vvpAcceptedCurrent(vvp) {
			continue
		}
		if vvp.Spec.TargetRef.Kind != "HTTPRoute" || vvp.Spec.TargetRef.Group != gatewayv1.GroupName {
			continue
		}
		var route gatewayv1.HTTPRoute
		routeKey := types.NamespacedName{Name: vvp.Spec.TargetRef.Name, Namespace: vvp.Namespace}
		if err := r.Get(ctx, routeKey, &route); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("r.Get(HTTPRoute %s): %w", routeKey, err)
		}
		if !routeAcceptedByGateway(&route, gwKey) {
			continue
		}
		rk := routeKey.String()
		if cur, ok := byRoute[rk]; ok {
			if vvpIsOlder(vvp, cur) {
				byRoute[rk] = vvp
			}
			continue
		}
		byRoute[rk] = vvp
		order = append(order, rk)
	}

	var out []routeVCLCandidate
	for _, rk := range order {
		vvp := byRoute[rk]
		policy := vvpKey(vvp)
		c := routeVCLCandidate{
			policy:     policy,
			route:      rk,
			generation: vvp.Generation,
			created:    vvp.CreationTimestamp,
		}
		doc, ok, err := vvpDocument(ctx, r.Client, vvp)
		if err != nil {
			return nil, err
		}
		if ok {
			c.document = doc
			out = append(out, c)
			continue
		}
		slot, isHeld := held[policy]
		if !isHeld {
			continue
		}
		prevDoc, ok := prevData[vcl.SlotKey(slot)]
		if !ok || prevDoc == "" {
			r.Logger.Warn("route VCL document unavailable and no previous slot document to keep; releasing the slot",
				"gateway", gwKey, "policy", policy, "slot", slot)
			continue
		}
		r.Logger.Warn("route VCL document unavailable; keeping the slot's previous document",
			"gateway", gwKey, "policy", policy, "slot", slot,
			"configmap", vvp.Namespace+"/"+vvp.Spec.VCLConfigMapRef.Name, "key", vvpConfigMapKey(vvp))
		c.composed = prevDoc
		c.generation = prev.Slots[strconv.Itoa(slot)].Generation
		out = append(out, c)
	}
	return out, nil
}

// timeNow returns the reconciler's clock (time.Now unless a test set one).
func (r *GatewayReconciler) timeNow() time.Time {
	if r.clock != nil {
		return r.clock()
	}
	return time.Now()
}

// routeVCLReleaseState computes, for every released slot of prev that is past
// its quarantine, whether the previous occupant can still be reached through
// it: cleared unless the policy still exists and one of its podResults
// entries for a live pod of gateway reports that slot programmed. The check
// ignores the hash on purpose: a pod reporting the slot programmed with an
// older hash of the same policy still has the label on that policy's VCL.
// The chaperone writes a final {hash: "", programmed: false} entry when a
// policy leaves its manifest, which clears it.
func (r *GatewayReconciler) routeVCLReleaseState(ctx context.Context, gateway *gatewayv1.Gateway, prev vcl.RouteVCLManifest) (routeVCLRelease, error) {
	rel := routeVCLRelease{now: r.timeNow(), cleared: map[int]bool{}}
	var live map[string]bool
	for _, rl := range prev.Released {
		if rel.now.Sub(rl.ReleasedAt.Time) < routeVCLReleaseQuarantine {
			continue
		}
		if live == nil {
			pods, err := gatewayLivePods(ctx, r.Client, gateway)
			if err != nil {
				return routeVCLRelease{}, err
			}
			live = make(map[string]bool, len(pods))
			for _, p := range pods {
				live[p] = true
			}
		}
		ok, err := r.releasedSlotCleared(ctx, rl, live)
		if err != nil {
			return routeVCLRelease{}, err
		}
		if prevOK, seen := rel.cleared[rl.Slot]; seen {
			ok = ok && prevOK
		}
		rel.cleared[rl.Slot] = ok
	}
	return rel, nil
}

// releasedSlotCleared reports whether no live pod still reports rl's slot
// programmed for rl's policy.
func (r *GatewayReconciler) releasedSlotCleared(ctx context.Context, rl vcl.RouteVCLReleased, live map[string]bool) (bool, error) {
	ns, name, ok := strings.Cut(rl.Policy, "/")
	if !ok || ns == "" || name == "" {
		return true, nil
	}
	var vvp gatewayparamsv1alpha1.VarnishVCLPolicy
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &vvp); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return true, nil
		}
		return false, fmt.Errorf("r.Get(VarnishVCLPolicy %s): %w", rl.Policy, err)
	}
	for _, res := range vvp.Status.PodResults {
		if live[res.PodName] && int(res.Slot) == rl.Slot && res.Programmed {
			return false, nil
		}
	}
	return true, nil
}

// reconcileRouteVCLConfigMap writes the Gateway's "{gateway}-route-vcl"
// ConfigMap: the slot manifest plus one composed route VCL document per
// occupied slot. The previous manifest is read from the same object that is
// updated, so a stale cache read fails the update with a conflict instead of
// overwriting a newer assignment. A previous manifest that does not parse
// fails the reconcile: reassigning from scratch would renumber every slot
// while pods still hold the old labels. The returned duration is when the
// earliest released slot may be freed (0: no requeue needed).
func (r *GatewayReconciler) reconcileRouteVCLConfigMap(ctx context.Context, gateway *gatewayv1.Gateway, poolSize int, userVCL string) (time.Duration, error) {
	name := routeVCLConfigMapName(gateway.Name)
	key := types.NamespacedName{Name: name, Namespace: gateway.Namespace}
	log := r.Logger.With("gateway", client.ObjectKeyFromObject(gateway), "configmap", name)

	var existing corev1.ConfigMap
	found := true
	if err := r.Get(ctx, key, &existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("r.Get(ConfigMap %s): %w", key, err)
		}
		found = false
	}

	var prev vcl.RouteVCLManifest
	if found {
		m, err := vcl.ParseRouteVCLManifest(existing.Data[vcl.ManifestKey])
		if err != nil {
			log.Error("route VCL manifest does not parse; not reassigning slots (delete the ConfigMap to start over)", "error", err)
			return 0, fmt.Errorf("route VCL manifest in ConfigMap %s: %w", key, err)
		}
		prev = m
	}

	var candidates []routeVCLCandidate
	rel := routeVCLRelease{now: r.timeNow()}
	if poolSize > 0 {
		var err error
		if candidates, err = r.routeVCLCandidates(ctx, gateway, prev, existing.Data); err != nil {
			return 0, err
		}
		if rel, err = r.routeVCLReleaseState(ctx, gateway, prev); err != nil {
			return 0, err
		}
	}

	a := assignRouteVCLSlots(prev, candidates, poolSize, userVCL, rel)
	for _, u := range a.newlyUnassigned {
		log.Warn("route VCL policy did not get a slot",
			"policy", u.Policy, "route", u.Route, "reason", u.Reason, "poolSize", poolSize)
	}

	if !found {
		desired := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: gateway.Namespace,
				Labels:    r.buildLabels(gateway),
			},
			Data: a.data,
		}
		if err := controllerutil.SetControllerReference(gateway, desired, r.Scheme); err != nil {
			return 0, fmt.Errorf("controllerutil.SetControllerReference: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return 0, fmt.Errorf("r.Create(ConfigMap %s): %w", key, err)
		}
		log.Info("created route VCL configmap", "slots", len(a.manifest.Slots), "unassigned", len(a.manifest.Unassigned))
		return a.requeueAfter, nil
	}

	if maps.Equal(existing.Data, a.data) {
		return a.requeueAfter, nil
	}
	existing.Data = a.data
	if err := r.Update(ctx, &existing); err != nil {
		return 0, fmt.Errorf("r.Update(ConfigMap %s): %w", key, err)
	}
	log.Info("updated route VCL configmap", "slots", len(a.manifest.Slots),
		"unassigned", len(a.manifest.Unassigned), "released", len(a.manifest.Released))
	return a.requeueAfter, nil
}

// varnishVCLPolicyGK is the GroupKind of VarnishVCLPolicy.
var varnishVCLPolicyGK = schema.GroupKind{Group: gatewayparamsv1alpha1.GroupName, Kind: "VarnishVCLPolicy"}

// varnishVCLPolicyCRDInstalled reports whether the API server serves
// VarnishVCLPolicy. Helm does not upgrade CRDs, so an upgraded operator can
// run without it; watches on it would then fail the manager.
func varnishVCLPolicyCRDInstalled(mapper meta.RESTMapper) (bool, error) {
	if _, err := mapper.RESTMapping(varnishVCLPolicyGK, gatewayparamsv1alpha1.GroupVersion.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, fmt.Errorf("mapper.RESTMapping(%s): %w", varnishVCLPolicyGK, err)
	}
	return true, nil
}

// gatewayRequestsForVVP returns the Gateways (of our classes) that the
// policy's target HTTPRoute lists as parents.
func (r *GatewayReconciler) gatewayRequestsForVVP(ctx context.Context, vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) []ctrl.Request {
	if vvp.Spec.TargetRef.Kind != "HTTPRoute" {
		return nil
	}
	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, types.NamespacedName{Name: vvp.Spec.TargetRef.Name, Namespace: vvp.Namespace}, &route); err != nil {
		return nil
	}
	return r.gatewayRequestsForRoute(ctx, &route)
}

// enqueueGatewaysForVVP maps a VarnishVCLPolicy event to the Gateways of its
// target route. On update both the old and new object are mapped (a
// retargeted policy frees its slot on the old route's Gateways); deletes are
// mapped too.
func (r *GatewayReconciler) enqueueGatewaysForVVP() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		vvp, ok := obj.(*gatewayparamsv1alpha1.VarnishVCLPolicy)
		if !ok {
			return nil
		}
		return r.gatewayRequestsForVVP(ctx, vvp)
	})
}

// vvpSlotRelevantChange passes VarnishVCLPolicy creates, deletes, spec
// changes and changes of the Accepted verdict for the current generation
// (accepted, rejected or none yet: a policy holding a slot keeps it until it
// is explicitly rejected, so the move from "none" to "rejected" matters). It
// filters out the frequent podResults and Programmed writes; released slots
// waiting on podResults are polled instead (routeVCLReleasePollInterval).
func vvpSlotRelevantChange() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldP, ok1 := e.ObjectOld.(*gatewayparamsv1alpha1.VarnishVCLPolicy)
			newP, ok2 := e.ObjectNew.(*gatewayparamsv1alpha1.VarnishVCLPolicy)
			if !ok1 || !ok2 {
				return true
			}
			if oldP.Generation != newP.Generation {
				return true
			}
			return vvpAcceptance(oldP) != vvpAcceptance(newP)
		},
	}
}

// gatewayRequestsForVVPConfigMap maps a ConfigMap to the Gateways of every
// VarnishVCLPolicy in its namespace that reads its VCL from it.
func (r *GatewayReconciler) gatewayRequestsForVVPConfigMap(ctx context.Context, cm *corev1.ConfigMap) []ctrl.Request {
	var list gatewayparamsv1alpha1.VarnishVCLPolicyList
	if err := r.List(ctx, &list, client.InNamespace(cm.Namespace)); err != nil {
		if !meta.IsNoMatchError(err) {
			r.Logger.Error("failed to list VarnishVCLPolicies for ConfigMap", "configmap", client.ObjectKeyFromObject(cm), "error", err)
		}
		return nil
	}
	var requests []ctrl.Request
	seen := make(map[types.NamespacedName]bool)
	for i := range list.Items {
		vvp := &list.Items[i]
		if vvp.Spec.VCLConfigMapRef.Name != cm.Name {
			continue
		}
		for _, req := range r.gatewayRequestsForVVP(ctx, vvp) {
			if !seen[req.NamespacedName] {
				seen[req.NamespacedName] = true
				requests = append(requests, req)
			}
		}
	}
	return requests
}
