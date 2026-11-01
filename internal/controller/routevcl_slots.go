package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"

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

// routeVCLCandidate is a VarnishVCLPolicy eligible for a slot on a Gateway.
type routeVCLCandidate struct {
	policy     string // "<namespace>/<name>"
	route      string // "<namespace>/<name>"
	generation int64
	created    metav1.Time
	document   string // the route VCL as written by the user
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

// assignRouteVCLSlots computes a Gateway's route VCL ConfigMap from the
// previous manifest and the eligible policies.
//
//   - A policy that held slot s in prev keeps it while s < poolSize, even if
//     its document changed. Retained slots are honoured before anything new
//     is placed.
//   - Everyone else (new policies, and those whose slot fell outside a
//     reduced pool) gets the lowest free slot, in creation-timestamp then
//     namespace/name order. Slots of policies that are no longer eligible are
//     free for this.
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
// is what the chaperone verifies.
func assignRouteVCLSlots(prev vcl.RouteVCLManifest, candidates []routeVCLCandidate, poolSize int, gatewayUserVCL string) routeVCLAssignment {
	manifest := vcl.RouteVCLManifest{Slots: map[string]vcl.RouteVCLSlot{}}
	data := map[string]string{}

	if poolSize > 0 && len(candidates) > 0 {
		cands := make([]routeVCLCandidate, len(candidates))
		copy(cands, candidates)
		sortRouteVCLCandidates(cands)

		prevSlot := make(map[string]int, len(prev.Slots))
		for key, s := range prev.Slots {
			n, ok := parseManifestSlot(key)
			if !ok {
				continue
			}
			// A corrupt manifest could list a policy twice; keep the
			// lowest slot so the result is deterministic.
			if old, dup := prevSlot[s.Policy]; !dup || n < old {
				prevSlot[s.Policy] = n
			}
		}
		prevReason := make(map[string]string, len(prev.Unassigned))
		for _, u := range prev.Unassigned {
			prevReason[u.Policy] = u.Reason
		}

		used := make(map[int]bool, poolSize)
		decided := make(map[string]bool, len(cands))
		lostToShrink := make(map[string]bool)
		size := 0

		// place composes c's document and records it in slot, or reports
		// false when it does not fit under the size guard.
		place := func(c routeVCLCandidate, slot int) bool {
			doc := vcl.GenerateRouteVCL(gatewayUserVCL, c.document)
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

		// Pass 2: everyone else, lowest free slot first.
		next := 0
		for _, c := range cands {
			if decided[c.policy] {
				continue
			}
			for next < poolSize && used[next] {
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
		// Only strings and integers: cannot fail.
		panic(fmt.Sprintf("json.MarshalIndent(RouteVCLManifest): %v", err))
	}
	data[vcl.ManifestKey] = string(raw)

	return routeVCLAssignment{manifest: manifest, data: data, newlyUnassigned: newly}
}

// vvpAcceptedCurrent reports whether the policy's Accepted condition, as
// written by this controller for its current generation, is True.
func vvpAcceptedCurrent(vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) bool {
	if len(vvp.Status.Ancestors) == 0 {
		return false
	}
	anc := vvp.Status.Ancestors[0]
	if anc.ControllerName != ControllerName {
		return false
	}
	c := meta.FindStatusCondition(anc.Conditions, gatewayparamsv1alpha1.VCLPolicyConditionAccepted)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == vvp.Generation
}

// vvpKey returns "<namespace>/<name>" for a policy.
func vvpKey(vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) string {
	return vvp.Namespace + "/" + vvp.Name
}

// routeVCLCandidates lists the VarnishVCLPolicies that should hold a slot on
// gateway: Accepted for their current generation, targeting an HTTPRoute (in
// the policy's namespace) that lists gateway in spec.parentRefs, with the
// document present in the referenced ConfigMap right now. A policy whose
// ConfigMap or key vanished since it was accepted is left out this round;
// the VVP controller will flip Accepted and that re-triggers this reconcile.
// If two policies target the same route (a stale status during a conflict),
// only the older is kept.
func (r *GatewayReconciler) routeVCLCandidates(ctx context.Context, gateway *gatewayv1.Gateway) ([]routeVCLCandidate, error) {
	var list gatewayparamsv1alpha1.VarnishVCLPolicyList
	if err := r.List(ctx, &list); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil // CRD not installed: no route VCL
		}
		return nil, fmt.Errorf("r.List(VarnishVCLPolicy): %w", err)
	}

	byRoute := make(map[string]*gatewayparamsv1alpha1.VarnishVCLPolicy)
	var order []string
	for i := range list.Items {
		vvp := &list.Items[i]
		if !vvpAcceptedCurrent(vvp) {
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
		if !routeHasParentGateway(&route, gateway) {
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
		cmKey := types.NamespacedName{Name: vvp.Spec.VCLConfigMapRef.Name, Namespace: vvp.Namespace}
		var cm corev1.ConfigMap
		if err := r.Get(ctx, cmKey, &cm); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("r.Get(ConfigMap %s): %w", cmKey, err)
		}
		doc, ok := cm.Data[vvpConfigMapKey(vvp)]
		if !ok || len(doc) > gatewayparamsv1alpha1.MaxRouteVCLDocumentBytes {
			continue
		}
		out = append(out, routeVCLCandidate{
			policy:     vvpKey(vvp),
			route:      rk,
			generation: vvp.Generation,
			created:    vvp.CreationTimestamp,
			document:   doc,
		})
	}
	return out, nil
}

// reconcileRouteVCLConfigMap writes the Gateway's "{gateway}-route-vcl"
// ConfigMap: the slot manifest plus one composed route VCL document per
// occupied slot. The previous manifest is read from the same object that is
// updated, so a stale cache read fails the update with a conflict instead of
// overwriting a newer assignment.
func (r *GatewayReconciler) reconcileRouteVCLConfigMap(ctx context.Context, gateway *gatewayv1.Gateway, poolSize int, userVCL string) error {
	name := routeVCLConfigMapName(gateway.Name)
	key := types.NamespacedName{Name: name, Namespace: gateway.Namespace}
	log := r.Logger.With("gateway", client.ObjectKeyFromObject(gateway), "configmap", name)

	var existing corev1.ConfigMap
	found := true
	if err := r.Get(ctx, key, &existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("r.Get(ConfigMap %s): %w", key, err)
		}
		found = false
	}

	var prev vcl.RouteVCLManifest
	if found {
		m, err := vcl.ParseRouteVCLManifest(existing.Data[vcl.ManifestKey])
		if err != nil {
			// Start over rather than wedge: slots are reassigned from scratch.
			log.Warn("route VCL manifest does not parse, reassigning all slots", "error", err)
		} else {
			prev = m
		}
	}

	var candidates []routeVCLCandidate
	if poolSize > 0 {
		var err error
		if candidates, err = r.routeVCLCandidates(ctx, gateway); err != nil {
			return err
		}
	}

	a := assignRouteVCLSlots(prev, candidates, poolSize, userVCL)
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
			return fmt.Errorf("controllerutil.SetControllerReference: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("r.Create(ConfigMap %s): %w", key, err)
		}
		log.Info("created route VCL configmap", "slots", len(a.manifest.Slots), "unassigned", len(a.manifest.Unassigned))
		return nil
	}

	if maps.Equal(existing.Data, a.data) {
		return nil
	}
	existing.Data = a.data
	if err := r.Update(ctx, &existing); err != nil {
		return fmt.Errorf("r.Update(ConfigMap %s): %w", key, err)
	}
	log.Info("updated route VCL configmap", "slots", len(a.manifest.Slots), "unassigned", len(a.manifest.Unassigned))
	return nil
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
// changes and changes of the Accepted condition. It filters out the frequent
// podResults and Programmed writes, which do not affect slot assignment.
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
			return vvpAcceptedCurrent(oldP) != vvpAcceptedCurrent(newP)
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
