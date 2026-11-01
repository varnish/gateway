package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
	"github.com/varnish/gateway/internal/vcl"
)

// maxVVPConditionMessage caps the Programmed message. Compiler output can be
// long; the API server limit for a condition message is 32768 bytes.
const maxVVPConditionMessage = 8192

// vvpProgrammed is the computed Programmed condition and slot list of a
// VarnishVCLPolicy, plus the live gateway pods used for podResults GC.
type vvpProgrammed struct {
	status  metav1.ConditionStatus
	reason  string
	message string
	slots   []gatewayparamsv1alpha1.VarnishVCLPolicySlot
	// livePods are the running pods of every parent Gateway of the target
	// route (managed by us, route VCL enabled or not).
	livePods map[string]bool
}

// vvpParentGateway is one parent Gateway of a policy's target route.
type vvpParentGateway struct {
	key      types.NamespacedName
	enabled  bool // route VCL pool size > 0
	livePods []string
}

// gatewayLivePods lists the pods of a Gateway's Deployment (selected by the
// operator's labels) that are not terminating or finished.
func gatewayLivePods(ctx context.Context, c client.Reader, gw *gatewayv1.Gateway) ([]string, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(gw.Namespace), client.MatchingLabels(gatewayLabels(gw))); err != nil {
		return nil, fmt.Errorf("c.List(Pods %s/%s): %w", gw.Namespace, gw.Name, err)
	}
	var out []string
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out, nil
}

// vvpParentGateways resolves the parent Gateways (of our classes) of the
// policy's target HTTPRoute, sorted by namespace/name. A missing route or
// Gateway contributes nothing.
func (r *VarnishVCLPolicyReconciler) vvpParentGateways(ctx context.Context, vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) ([]vvpParentGateway, error) {
	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, types.NamespacedName{Name: vvp.Spec.TargetRef.Name, Namespace: vvp.Namespace}, &route); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("r.Get(HTTPRoute %s/%s): %w", vvp.Namespace, vvp.Spec.TargetRef.Name, err)
	}

	seen := make(map[types.NamespacedName]bool)
	var out []vvpParentGateway
	for _, ref := range route.Spec.ParentRefs {
		if ref.Group != nil && *ref.Group != gatewayv1.GroupName {
			continue
		}
		if ref.Kind != nil && *ref.Kind != "Gateway" {
			continue
		}
		key := types.NamespacedName{Name: string(ref.Name), Namespace: route.Namespace}
		if ref.Namespace != nil {
			key.Namespace = string(*ref.Namespace)
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		var gw gatewayv1.Gateway
		if err := r.Get(ctx, key, &gw); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("r.Get(Gateway %s): %w", key, err)
		}
		gc, params, err := resolveGatewayClassParameters(ctx, r.Client, string(gw.Spec.GatewayClassName))
		if err != nil {
			return nil, err
		}
		if gc == nil || string(gc.Spec.ControllerName) != ControllerName {
			continue
		}
		pods, err := gatewayLivePods(ctx, r.Client, &gw)
		if err != nil {
			return nil, err
		}
		out = append(out, vvpParentGateway{key: key, enabled: routeVCLPoolSize(params) > 0, livePods: pods})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key.String() < out[j].key.String() })
	return out, nil
}

// Per-Gateway programming states, in aggregation precedence order (lower
// wins when not every Gateway is programmed).
const (
	gwStateCompileFailed = iota
	gwStateNoSlot
	gwStatePending
	gwStateNotListed
	gwStateProgrammed
)

// gatewayProgramState is one parent Gateway's view of the policy.
type gatewayProgramState struct {
	state   int
	message string
}

// programStateForGateway derives one Gateway's state from its manifest and
// the policy's podResults.
func programStateForGateway(gw vvpParentGateway, policy string, manifest vcl.RouteVCLManifest, results map[string]gatewayparamsv1alpha1.VarnishVCLPolicyPodResult) (gatewayProgramState, *gatewayparamsv1alpha1.VarnishVCLPolicySlot) {
	prefix := "Gateway " + gw.key.String() + ": "

	var slotKey string
	var entry vcl.RouteVCLSlot
	found := false
	// Deterministic choice if a corrupt manifest lists the policy twice.
	for _, k := range slices.Sorted(mapKeys(manifest.Slots)) {
		if manifest.Slots[k].Policy == policy {
			slotKey, entry, found = k, manifest.Slots[k], true
			break
		}
	}
	if !found {
		for _, u := range manifest.Unassigned {
			if u.Policy == policy {
				return gatewayProgramState{gwStateNoSlot, prefix + "no slot assigned: " + u.Reason}, nil
			}
		}
		return gatewayProgramState{gwStateNotListed, prefix + "not in the route VCL manifest yet"}, nil
	}
	n, ok := parseManifestSlot(slotKey)
	if !ok {
		return gatewayProgramState{gwStateNotListed, prefix + fmt.Sprintf("manifest slot %q is not a number", slotKey)}, nil
	}
	slot := &gatewayparamsv1alpha1.VarnishVCLPolicySlot{Gateway: gw.key.String(), Slot: int32(n)}

	if len(gw.livePods) == 0 {
		return gatewayProgramState{gwStatePending, prefix + fmt.Sprintf("slot %d assigned, no running pods", n)}, slot
	}
	programmed := 0
	for _, pod := range gw.livePods {
		res, ok := results[pod]
		// Slot and hash must both match: after a slot move the same
		// document (same hash) reported for the old slot says nothing
		// about the label of the new one.
		if !ok || int(res.Slot) != n || res.Hash != entry.Hash {
			continue
		}
		if !res.Programmed {
			msg := res.Message
			if msg == "" {
				msg = "not programmed"
			}
			return gatewayProgramState{gwStateCompileFailed, prefix + fmt.Sprintf("slot %d, pod %s: %s", n, pod, msg)}, slot
		}
		programmed++
	}
	if programmed == len(gw.livePods) {
		return gatewayProgramState{gwStateProgrammed, prefix + fmt.Sprintf("slot %d programmed on %d pod(s)", n, programmed)}, slot
	}
	return gatewayProgramState{gwStatePending, prefix + fmt.Sprintf("slot %d programmed on %d of %d pod(s)", n, programmed, len(gw.livePods))}, slot
}

// mapKeys yields the keys of a map (for slices.Sorted).
func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// computeProgrammed derives the Programmed condition and status.slots.
//
// For every parent Gateway with route VCL enabled, the Gateway's route VCL
// manifest decides: a slot means the policy is Programmed there once every
// live pod of the Gateway reports that slot number and hash with
// programmed=true; CompileFailed when a live pod reports the current slot
// and hash with programmed=false; Pending otherwise. Unassigned means NoSlotAvailable; no
// entry at all means NotProgrammed. Across Gateways: all programmed is True,
// else CompileFailed, NoSlotAvailable, Pending in that order; NotProgrammed
// (Unknown) only when no Gateway has done anything with the policy yet.
func (r *VarnishVCLPolicyReconciler) computeProgrammed(ctx context.Context, vvp *gatewayparamsv1alpha1.VarnishVCLPolicy, accepted bool) (vvpProgrammed, error) {
	parents, err := r.vvpParentGateways(ctx, vvp)
	if err != nil {
		return vvpProgrammed{}, err
	}
	live := make(map[string]bool)
	parentSet := make(map[types.NamespacedName]bool, len(parents))
	for _, p := range parents {
		parentSet[p.key] = true
		for _, pod := range p.livePods {
			live[pod] = true
		}
	}
	// Gateways that still list the policy (typically a released slot after
	// a retarget) are no longer parents, but their pods' entries are what
	// the Gateway controller checks before freeing the slot: keep them.
	holders, err := r.manifestHolderGateways(ctx, vvpKey(vvp))
	if err != nil {
		return vvpProgrammed{}, err
	}
	for _, gk := range holders {
		if parentSet[gk] {
			continue
		}
		pods, err := gatewayLivePods(ctx, r.Client, &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: gk.Name, Namespace: gk.Namespace}})
		if err != nil {
			return vvpProgrammed{}, err
		}
		for _, pod := range pods {
			live[pod] = true
		}
	}

	if !accepted {
		return vvpProgrammed{
			status:   metav1.ConditionFalse,
			reason:   gatewayparamsv1alpha1.VCLPolicyReasonNotAccepted,
			message:  "Policy is not accepted",
			livePods: live,
		}, nil
	}

	results := make(map[string]gatewayparamsv1alpha1.VarnishVCLPolicyPodResult, len(vvp.Status.PodResults))
	for _, res := range vvp.Status.PodResults {
		results[res.PodName] = res
	}

	policy := vvpKey(vvp)
	var states []gatewayProgramState
	var slots []gatewayparamsv1alpha1.VarnishVCLPolicySlot
	for _, p := range parents {
		if !p.enabled {
			continue
		}
		manifest, _, err := readRouteVCLManifest(ctx, r.Client, p.key)
		if err != nil {
			return vvpProgrammed{}, err
		}
		st, slot := programStateForGateway(p, policy, manifest, results)
		states = append(states, st)
		if slot != nil {
			slots = append(slots, *slot)
		}
	}

	out := vvpProgrammed{slots: slots, livePods: live}
	if len(states) == 0 {
		out.status = metav1.ConditionUnknown
		out.reason = gatewayparamsv1alpha1.VCLPolicyReasonNotProgrammed
		out.message = "No parent Gateway with route VCL enabled"
		return out, nil
	}

	best := gwStateProgrammed
	anyProgrammed := false
	for _, st := range states {
		if st.state < best {
			best = st.state
		}
		if st.state == gwStateProgrammed {
			anyProgrammed = true
		}
	}
	if best == gwStateNotListed && anyProgrammed {
		// Some Gateways are done, others have not listed the policy yet.
		best = gwStatePending
	}

	var msgs []string
	for _, st := range states {
		if st.state == best || (best == gwStatePending && st.state == gwStateNotListed) {
			msgs = append(msgs, st.message)
		}
	}
	out.message = truncateMessage(strings.Join(msgs, "; "), maxVVPConditionMessage)

	switch best {
	case gwStateProgrammed:
		out.status, out.reason = metav1.ConditionTrue, gatewayparamsv1alpha1.VCLPolicyReasonProgrammed
	case gwStateCompileFailed:
		out.status, out.reason = metav1.ConditionFalse, gatewayparamsv1alpha1.VCLPolicyReasonCompileFailed
	case gwStateNoSlot:
		out.status, out.reason = metav1.ConditionFalse, gatewayparamsv1alpha1.VCLPolicyReasonNoSlotAvailable
	case gwStatePending:
		out.status, out.reason = metav1.ConditionFalse, gatewayparamsv1alpha1.VCLPolicyReasonPending
	default:
		out.status, out.reason = metav1.ConditionUnknown, gatewayparamsv1alpha1.VCLPolicyReasonNotProgrammed
	}
	return out, nil
}

// truncateMessage cuts s to at most limit bytes, on a rune boundary.
func truncateMessage(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const tail = " ...(truncated)"
	cut := limit - len(tail)
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + tail
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// gcPodResults drops entries for pods that are not live pods of any parent
// Gateway. Entries for live pods are kept even when stale: the chaperone only
// rewrites an entry when its result changes, so deleting a live pod's entry
// could leave it missing for good.
func gcPodResults(results []gatewayparamsv1alpha1.VarnishVCLPolicyPodResult, live map[string]bool) []gatewayparamsv1alpha1.VarnishVCLPolicyPodResult {
	var out []gatewayparamsv1alpha1.VarnishVCLPolicyPodResult
	for _, res := range results {
		if live[res.PodName] {
			out = append(out, res)
		}
	}
	return out
}

// vvpConditionEqual reports whether conditions holds a condition of type t
// with the given status, reason and message for generation.
func vvpConditionEqual(conditions []metav1.Condition, t string, s metav1.ConditionStatus, reason, message string, generation int64) bool {
	for _, c := range conditions {
		if c.Type == t {
			return c.Status == s && c.Reason == reason && c.Message == message && c.ObservedGeneration == generation
		}
	}
	return false
}

// vvpStatusMatches reports whether the stored status already reflects cond,
// prog and the podResults GC, so the write can be skipped.
func vvpStatusMatches(vvp *gatewayparamsv1alpha1.VarnishVCLPolicy, cond vvpCondition, prog vvpProgrammed) bool {
	if !vvpConditionMatches(vvp, cond) {
		return false
	}
	conds := vvp.Status.Ancestors[0].Conditions
	if !vvpConditionEqual(conds, gatewayparamsv1alpha1.VCLPolicyConditionProgrammed, prog.status, prog.reason, prog.message, vvp.Generation) {
		return false
	}
	if !equality.Semantic.DeepEqual(normalizeSlots(vvp.Status.Slots), normalizeSlots(prog.slots)) {
		return false
	}
	return len(gcPodResults(vvp.Status.PodResults, prog.livePods)) == len(vvp.Status.PodResults)
}

// normalizeSlots maps nil to empty so a nil and an empty list compare equal.
func normalizeSlots(s []gatewayparamsv1alpha1.VarnishVCLPolicySlot) []gatewayparamsv1alpha1.VarnishVCLPolicySlot {
	if s == nil {
		return []gatewayparamsv1alpha1.VarnishVCLPolicySlot{}
	}
	return s
}

// vvpGenerationOrPodResultsChanged passes creates, deletes, spec changes and
// changes to status.podResults (written by gateway pods), and filters out
// everything else, notably this controller's own condition writes.
func vvpGenerationOrPodResultsChanged() predicate.Predicate {
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
			return !equality.Semantic.DeepEqual(oldP.Status.PodResults, newP.Status.PodResults)
		},
	}
}

// routeGenerationOrParentsChanged passes HTTPRoute creates, deletes, spec
// changes and status.parents changes. Attachment (status.parents Accepted)
// is part of a policy's Accepted condition, and it is written by the
// HTTPRoute controller without a generation bump.
func routeGenerationOrParentsChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldR, ok1 := e.ObjectOld.(*gatewayv1.HTTPRoute)
			newR, ok2 := e.ObjectNew.(*gatewayv1.HTTPRoute)
			if !ok1 || !ok2 {
				return true
			}
			if oldR.Generation != newR.Generation {
				return true
			}
			return !equality.Semantic.DeepEqual(oldR.Status.Parents, newR.Status.Parents)
		},
	}
}

// vvpRequestsForPolicyKeys converts "<namespace>/<name>" strings into
// deduplicated reconcile requests.
func vvpRequestsForPolicyKeys(keys []string) []reconcile.Request {
	seen := make(map[types.NamespacedName]bool)
	var out []reconcile.Request
	for _, k := range keys {
		ns, name, ok := strings.Cut(k, "/")
		if !ok || ns == "" || name == "" {
			continue
		}
		nn := types.NamespacedName{Namespace: ns, Name: name}
		if seen[nn] {
			continue
		}
		seen[nn] = true
		out = append(out, reconcile.Request{NamespacedName: nn})
	}
	return out
}

// manifestPolicyKeys returns every policy named in a manifest: slotted,
// unassigned or holding a released slot.
func manifestPolicyKeys(m vcl.RouteVCLManifest) []string {
	var keys []string
	for _, k := range slices.Sorted(mapKeys(m.Slots)) {
		keys = append(keys, m.Slots[k].Policy)
	}
	for _, u := range m.Unassigned {
		keys = append(keys, u.Policy)
	}
	for _, rl := range m.Released {
		keys = append(keys, rl.Policy)
	}
	return keys
}

// manifestHolderGateways returns the Gateways whose route VCL manifest lists
// policy in Slots or Released. Manifests that do not parse are skipped.
func (r *VarnishVCLPolicyReconciler) manifestHolderGateways(ctx context.Context, policy string) ([]types.NamespacedName, error) {
	var cms corev1.ConfigMapList
	if err := r.List(ctx, &cms, client.MatchingLabels{LabelManagedBy: ManagedByValue}); err != nil {
		return nil, fmt.Errorf("r.List(ConfigMaps managed-by=%s): %w", ManagedByValue, err)
	}
	var out []types.NamespacedName
	for i := range cms.Items {
		cm := &cms.Items[i]
		if !isRouteVCLConfigMap(cm) {
			continue
		}
		m, err := vcl.ParseRouteVCLManifest(cm.Data[vcl.ManifestKey])
		if err != nil {
			continue
		}
		if slices.Contains(manifestPolicyHolders(m), policy) {
			out = append(out, types.NamespacedName{
				Namespace: cm.Namespace,
				Name:      strings.TrimSuffix(cm.Name, vcl.RouteVCLConfigMapSuffix),
			})
		}
	}
	return out, nil
}

// manifestPolicyHolders returns the policies that hold or held a slot in a
// manifest (Slots and Released; not Unassigned).
func manifestPolicyHolders(m vcl.RouteVCLManifest) []string {
	var keys []string
	for _, s := range m.Slots {
		keys = append(keys, s.Policy)
	}
	for _, rl := range m.Released {
		keys = append(keys, rl.Policy)
	}
	return keys
}

// findVVPsForRouteVCLConfigMap maps a "{gateway}-route-vcl" ConfigMap to the
// policies in its manifest (any namespace). Old and new objects are both
// mapped on update, so a policy that left the manifest is re-evaluated.
func (r *VarnishVCLPolicyReconciler) findVVPsForRouteVCLConfigMap(cm *corev1.ConfigMap) []reconcile.Request {
	m, err := vcl.ParseRouteVCLManifest(cm.Data[vcl.ManifestKey])
	if err != nil {
		r.Logger.Warn("route VCL manifest does not parse", "configmap", client.ObjectKeyFromObject(cm), "error", err)
		return nil
	}
	return vvpRequestsForPolicyKeys(manifestPolicyKeys(m))
}

// findVVPsForDeployment maps a gateway Deployment event (rollouts, scaling,
// pod churn reflected in its status) to the policies in that Gateway's route
// VCL manifest, so Programmed is re-derived against the current pod set.
// Deployments not created by this operator for a Gateway are ignored.
func (r *VarnishVCLPolicyReconciler) findVVPsForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok {
		return nil
	}
	labels := dep.GetLabels()
	if labels[LabelManagedBy] != ManagedByValue || labels[LabelGatewayName] == "" {
		return nil
	}
	gwKey := types.NamespacedName{Name: labels[LabelGatewayName], Namespace: dep.Namespace}
	m, found, err := readRouteVCLManifest(ctx, r.Client, gwKey)
	if err != nil {
		r.Logger.Warn("failed to read route VCL manifest for Deployment", "deployment", client.ObjectKeyFromObject(dep), "error", err)
		return nil
	}
	if !found {
		return nil
	}
	return vvpRequestsForPolicyKeys(manifestPolicyKeys(m))
}
