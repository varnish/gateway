package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
	"github.com/varnish/gateway/internal/status"
)

// vvpRequeueAfter is how long to wait before re-checking a policy whose target
// HTTPRoute or ConfigMap does not exist yet. The watches normally pick up the
// creation first; the requeue is a safety net, as in the VCP controller.
const vvpRequeueAfter = 10 * time.Second

// VarnishVCLPolicyReconciler reconciles VarnishVCLPolicy objects.
//
// It maintains the Accepted condition (validation, references, enablement,
// conflicts), and the Programmed condition and status.slots, derived from
// each parent Gateway's route VCL manifest (written by the Gateway
// controller) and the per-pod results the chaperones write to
// status.podResults. It also garbage-collects podResults of pods that no
// longer exist.
type VarnishVCLPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Logger *slog.Logger
}

// vvpCondition is the computed Accepted condition for a VarnishVCLPolicy.
type vvpCondition struct {
	accepted     bool
	reason       string
	message      string
	requeueAfter time.Duration
}

// Reconcile computes and persists the Accepted condition of a VarnishVCLPolicy.
func (r *VarnishVCLPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Logger.With("vvp", req.NamespacedName)
	log.Debug("reconciling VarnishVCLPolicy")

	var vvp gatewayparamsv1alpha1.VarnishVCLPolicy
	if err := r.Get(ctx, req.NamespacedName, &vvp); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted. If it was the winner of a conflict, the sibling
			// watch (findSiblingVVPs) has already enqueued the losers
			// from the delete event, so there is nothing to do here.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("r.Get(%s): %w", req.NamespacedName, err)
	}

	cond, err := r.computeCondition(ctx, &vvp)
	if err != nil {
		return ctrl.Result{}, err
	}
	prog, err := r.computeProgrammed(ctx, &vvp, cond.accepted)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !vvpStatusMatches(&vvp, cond, prog) {
		// Mutate the fetched object: status.podResults belongs to the
		// gateway pods and must survive this write (apart from the GC of
		// entries for pods that no longer exist).
		setVVPConditions(&vvp, cond, prog)
		vvp.Status.Slots = prog.slots
		vvp.Status.PodResults = gcPodResults(vvp.Status.PodResults, prog.livePods)
		if err := r.Status().Update(ctx, &vvp); err != nil {
			if apierrors.IsConflict(err) {
				// A chaperone wrote podResults meanwhile; recompute.
				log.Debug("conflict writing VarnishVCLPolicy status, requeueing", "error", err)
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, fmt.Errorf("r.Status().Update(%s): %w", req.NamespacedName, err)
		}
	}

	// Creating, retargeting or deleting this policy can change which of its
	// siblings wins a conflict. Those siblings are enqueued by the
	// findSiblingVVPs watch rather than written here, so every policy
	// writes only its own status and failed writes get the normal retry.

	log.Debug("VarnishVCLPolicy reconciliation complete", "accepted", cond.accepted, "reason", cond.reason,
		"programmed", prog.status, "programmedReason", prog.reason)
	return ctrl.Result{RequeueAfter: cond.requeueAfter}, nil
}

// computeCondition determines the Accepted condition for a policy without
// mutating or persisting it. A non-nil error is returned only for unexpected
// API failures that warrant a controller retry.
//
// Checks run in this order, and the first failure decides the reason:
// spec validity, target existence, ConfigMap and document, enablement on the
// route's parent Gateways, and conflicts with sibling policies.
func (r *VarnishVCLPolicyReconciler) computeCondition(ctx context.Context, vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) (vvpCondition, error) {
	if err := validateVVPSpec(&vvp.Spec); err != nil {
		return vvpCondition{reason: gatewayparamsv1alpha1.VCLPolicyReasonInvalid, message: err.Error()}, nil
	}

	routeName := vvp.Spec.TargetRef.Name
	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, types.NamespacedName{Name: routeName, Namespace: vvp.Namespace}, &route); err != nil {
		if apierrors.IsNotFound(err) {
			return vvpCondition{
				reason:       gatewayparamsv1alpha1.VCLPolicyReasonTargetNotFound,
				message:      fmt.Sprintf("HTTPRoute %q not found in namespace %q", routeName, vvp.Namespace),
				requeueAfter: vvpRequeueAfter,
			}, nil
		}
		return vvpCondition{}, fmt.Errorf("r.Get(HTTPRoute %s/%s): %w", vvp.Namespace, routeName, err)
	}

	if cond, ok, err := r.checkConfigMap(ctx, vvp); err != nil || !ok {
		return cond, err
	}

	enabledMsg, disabledMsg, err := r.routeVCLEnablement(ctx, &route)
	if err != nil {
		return vvpCondition{}, err
	}
	if enabledMsg == "" {
		return vvpCondition{reason: gatewayparamsv1alpha1.VCLPolicyReasonDisabled, message: disabledMsg}, nil
	}

	conflict, err := r.checkConflict(ctx, vvp)
	if err != nil {
		return vvpCondition{}, err
	}
	if conflict != "" {
		return vvpCondition{reason: gatewayparamsv1alpha1.VCLPolicyReasonConflicted, message: conflict}, nil
	}

	msg := "Policy accepted"
	if disabledMsg != "" {
		msg += "; route VCL is not enabled for some parent Gateways: " + disabledMsg
	}
	return vvpCondition{accepted: true, reason: gatewayparamsv1alpha1.VCLPolicyReasonAccepted, message: msg}, nil
}

// validateVVPSpec checks the parts of the spec that need no API lookups.
func validateVVPSpec(spec *gatewayparamsv1alpha1.VarnishVCLPolicySpec) error {
	ref := spec.TargetRef
	if ref.Group != gatewayv1.GroupName {
		return fmt.Errorf("targetRef.group must be %s, got %q", gatewayv1.GroupName, ref.Group)
	}
	if ref.Kind != "HTTPRoute" {
		return fmt.Errorf("targetRef.kind must be HTTPRoute, got %q", ref.Kind)
	}
	if ref.Name == "" {
		return fmt.Errorf("targetRef.name must not be empty")
	}
	if ref.SectionName != nil {
		return fmt.Errorf("targetRef.sectionName is not supported; attach the policy to the whole HTTPRoute")
	}
	if spec.VCLConfigMapRef.Name == "" {
		return fmt.Errorf("vclConfigMapRef.name must not be empty")
	}
	return nil
}

// vvpConfigMapKey returns the ConfigMap key the policy reads its VCL from.
func vvpConfigMapKey(vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) string {
	if vvp.Spec.VCLConfigMapRef.Key != "" {
		return vvp.Spec.VCLConfigMapRef.Key
	}
	return gatewayparamsv1alpha1.DefaultRouteVCLKey
}

// checkConfigMap verifies that the referenced ConfigMap and key exist and that
// the document is within MaxRouteVCLDocumentBytes. ok is false when the policy
// must not be accepted, in which case cond holds the reason.
func (r *VarnishVCLPolicyReconciler) checkConfigMap(ctx context.Context, vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) (vvpCondition, bool, error) {
	cmName := vvp.Spec.VCLConfigMapRef.Name
	key := vvpConfigMapKey(vvp)

	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: vvp.Namespace}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return vvpCondition{
				reason:       gatewayparamsv1alpha1.VCLPolicyReasonConfigMapNotFound,
				message:      fmt.Sprintf("ConfigMap %q not found in namespace %q", cmName, vvp.Namespace),
				requeueAfter: vvpRequeueAfter,
			}, false, nil
		}
		return vvpCondition{}, false, fmt.Errorf("r.Get(ConfigMap %s/%s): %w", vvp.Namespace, cmName, err)
	}

	doc, found := cm.Data[key]
	if !found {
		return vvpCondition{
			reason:       gatewayparamsv1alpha1.VCLPolicyReasonConfigMapNotFound,
			message:      fmt.Sprintf("ConfigMap %q has no key %q in data", cmName, key),
			requeueAfter: vvpRequeueAfter,
		}, false, nil
	}
	if len(doc) > gatewayparamsv1alpha1.MaxRouteVCLDocumentBytes {
		return vvpCondition{
			reason: gatewayparamsv1alpha1.VCLPolicyReasonInvalid,
			message: fmt.Sprintf("VCL document in ConfigMap %q key %q is %d bytes, larger than the %d byte limit",
				cmName, key, len(doc), gatewayparamsv1alpha1.MaxRouteVCLDocumentBytes),
		}, false, nil
	}
	return vvpCondition{}, true, nil
}

// routeVCLEnablement reports whether route VCL is enabled for at least one of
// the route's parent Gateways. enabledMsg lists the parents with the feature
// on and is empty when there are none; disabledMsg explains each parent that
// does not have it on. Both are deterministic for a given input so that the
// condition message only changes when the situation does.
func (r *VarnishVCLPolicyReconciler) routeVCLEnablement(ctx context.Context, route *gatewayv1.HTTPRoute) (enabledMsg, disabledMsg string, err error) {
	var enabled, disabled []string
	seen := make(map[types.NamespacedName]bool)

	for _, ref := range route.Spec.ParentRefs {
		if ref.Group != nil && *ref.Group != gatewayv1.GroupName {
			continue
		}
		if ref.Kind != nil && *ref.Kind != "Gateway" {
			continue
		}
		gwKey := types.NamespacedName{Name: string(ref.Name), Namespace: route.Namespace}
		if ref.Namespace != nil {
			gwKey.Namespace = string(*ref.Namespace)
		}
		if seen[gwKey] {
			continue
		}
		seen[gwKey] = true

		on, why, err := r.routeVCLEnabledForGateway(ctx, gwKey)
		if err != nil {
			return "", "", err
		}
		if on {
			enabled = append(enabled, fmt.Sprintf("Gateway %s", gwKey))
		} else {
			disabled = append(disabled, fmt.Sprintf("Gateway %s: %s", gwKey, why))
		}
	}

	if len(seen) == 0 {
		return "", fmt.Sprintf("HTTPRoute %q has no parent Gateway", route.Name), nil
	}
	return strings.Join(enabled, "; "), strings.Join(disabled, "; "), nil
}

// routeVCLEnabledForGateway resolves Gateway -> GatewayClass ->
// GatewayClassParameters and reports whether routeVCL.enabled is true. When it
// is not, why says which link in the chain is missing or which GatewayClass
// has the feature off.
func (r *VarnishVCLPolicyReconciler) routeVCLEnabledForGateway(ctx context.Context, gwKey types.NamespacedName) (bool, string, error) {
	var gw gatewayv1.Gateway
	if err := r.Get(ctx, gwKey, &gw); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "Gateway not found", nil
		}
		return false, "", fmt.Errorf("r.Get(Gateway %s): %w", gwKey, err)
	}

	className := string(gw.Spec.GatewayClassName)
	gc, params, err := resolveGatewayClassParameters(ctx, r.Client, className)
	if err != nil {
		return false, "", err
	}
	if gc == nil {
		return false, fmt.Sprintf("GatewayClass %q not found", className), nil
	}
	if string(gc.Spec.ControllerName) != ControllerName {
		return false, fmt.Sprintf("GatewayClass %q is not managed by %s", className, ControllerName), nil
	}
	if params == nil || params.Spec.RouteVCL == nil || !params.Spec.RouteVCL.Enabled {
		return false, fmt.Sprintf("GatewayClass %q does not set routeVCL.enabled=true in its GatewayClassParameters", className), nil
	}
	return true, "", nil
}

// checkConflict reports whether an older policy in the same namespace targets
// the same HTTPRoute. The oldest policy wins (creation timestamp, then name)
// regardless of its own validity, matching VarnishCachePolicy. It returns an
// empty string when there is no conflict. A List failure is returned as an
// error rather than treated as "no conflict", so a transient API error can
// never promote a Conflicted loser.
func (r *VarnishVCLPolicyReconciler) checkConflict(ctx context.Context, vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) (string, error) {
	var list gatewayparamsv1alpha1.VarnishVCLPolicyList
	if err := r.List(ctx, &list, client.InNamespace(vvp.Namespace)); err != nil {
		return "", fmt.Errorf("r.List(VarnishVCLPolicy, %s): %w", vvp.Namespace, err)
	}

	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == vvp.Name {
			continue
		}
		if !sameVVPTarget(other, vvp) {
			continue
		}
		if vvpIsOlder(other, vvp) {
			return fmt.Sprintf("Conflicts with VarnishVCLPolicy %q (older policy takes precedence)", other.Name), nil
		}
	}
	return "", nil
}

// sameVVPTarget reports whether two policies name the same target object.
func sameVVPTarget(a, b *gatewayparamsv1alpha1.VarnishVCLPolicy) bool {
	return a.Spec.TargetRef.Group == b.Spec.TargetRef.Group &&
		a.Spec.TargetRef.Kind == b.Spec.TargetRef.Kind &&
		a.Spec.TargetRef.Name == b.Spec.TargetRef.Name
}

// vvpIsOlder reports whether a takes precedence over b in a conflict.
func vvpIsOlder(a, b *gatewayparamsv1alpha1.VarnishVCLPolicy) bool {
	if a.CreationTimestamp.Before(&b.CreationTimestamp) {
		return true
	}
	return a.CreationTimestamp.Equal(&b.CreationTimestamp) && a.Name < b.Name
}

// findSiblingVVPs maps a VarnishVCLPolicy event (create, spec change or
// delete) to the other policies in its namespace that name the same target,
// so a conflict winner appearing, retargeting or disappearing re-evaluates
// the losers through their own reconcile (with normal retry on failure).
// On an update controller-runtime calls this for both the old and the new
// object, so a retarget enqueues the siblings of both targets.
func (r *VarnishVCLPolicyReconciler) findSiblingVVPs(ctx context.Context, obj client.Object) []reconcile.Request {
	vvp, ok := obj.(*gatewayparamsv1alpha1.VarnishVCLPolicy)
	if !ok {
		return nil
	}
	var list gatewayparamsv1alpha1.VarnishVCLPolicyList
	if err := r.List(ctx, &list, client.InNamespace(vvp.Namespace)); err != nil {
		r.Logger.Error("failed to list VarnishVCLPolicies for sibling lookup", "vvp", client.ObjectKeyFromObject(vvp), "error", err)
		return nil
	}
	return vvpRequests(list.Items, func(p *gatewayparamsv1alpha1.VarnishVCLPolicy) bool {
		return p.Name != vvp.Name && sameVVPTarget(p, vvp)
	})
}

// vvpConditionMatches reports whether the policy's Accepted condition already
// equals cond. Skipping no-op writes keeps sibling re-evaluation from churning
// status.
func vvpConditionMatches(vvp *gatewayparamsv1alpha1.VarnishVCLPolicy, cond vvpCondition) bool {
	if len(vvp.Status.Ancestors) == 0 {
		return false
	}
	ancestor := vvp.Status.Ancestors[0]
	if ancestor.ControllerName != ControllerName ||
		ancestor.AncestorRef.Kind != vvp.Spec.TargetRef.Kind ||
		ancestor.AncestorRef.Name != vvp.Spec.TargetRef.Name {
		return false
	}
	for _, c := range ancestor.Conditions {
		if c.Type == gatewayparamsv1alpha1.VCLPolicyConditionAccepted {
			return c.Status == status.BoolToStatus(cond.accepted) &&
				c.Reason == cond.reason &&
				c.Message == cond.message &&
				c.ObservedGeneration == vvp.Generation
		}
	}
	return false
}

// setVVPConditions writes the Accepted and Programmed conditions into
// ancestors[0], whose ancestorRef is the policy's target, mirroring
// VarnishCachePolicy. Other condition types already present are preserved,
// and LastTransitionTime only moves when a status flips.
func setVVPConditions(vvp *gatewayparamsv1alpha1.VarnishVCLPolicy, cond vvpCondition, prog vvpProgrammed) {
	var conditions []metav1.Condition
	if len(vvp.Status.Ancestors) > 0 {
		conditions = vvp.Status.Ancestors[0].Conditions
	}
	status.SetCondition(&conditions, status.NewCondition(
		gatewayparamsv1alpha1.VCLPolicyConditionAccepted,
		status.BoolToStatus(cond.accepted),
		vvp.Generation,
		cond.reason,
		cond.message,
	))
	status.SetCondition(&conditions, status.NewCondition(
		gatewayparamsv1alpha1.VCLPolicyConditionProgrammed,
		prog.status,
		vvp.Generation,
		prog.reason,
		prog.message,
	))
	vvp.Status.Ancestors = []gatewayparamsv1alpha1.VarnishVCLPolicyAncestorStatus{{
		AncestorRef:    vvp.Spec.TargetRef,
		ControllerName: ControllerName,
		Conditions:     conditions,
	}}
}

// vvpRequests converts policies into reconcile requests.
func vvpRequests(items []gatewayparamsv1alpha1.VarnishVCLPolicy, keep func(*gatewayparamsv1alpha1.VarnishVCLPolicy) bool) []reconcile.Request {
	var requests []reconcile.Request
	for i := range items {
		if keep != nil && !keep(&items[i]) {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: items[i].Name, Namespace: items[i].Namespace},
		})
	}
	return requests
}

// findVVPsForHTTPRoute maps an HTTPRoute event to the policies targeting it.
func (r *VarnishVCLPolicyReconciler) findVVPsForHTTPRoute(ctx context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}
	var list gatewayparamsv1alpha1.VarnishVCLPolicyList
	if err := r.List(ctx, &list, client.InNamespace(route.Namespace)); err != nil {
		r.Logger.Error("failed to list VarnishVCLPolicies for HTTPRoute", "route", client.ObjectKeyFromObject(route), "error", err)
		return nil
	}
	return vvpRequests(list.Items, func(p *gatewayparamsv1alpha1.VarnishVCLPolicy) bool {
		return p.Spec.TargetRef.Kind == "HTTPRoute" && p.Spec.TargetRef.Name == route.Name
	})
}

// findVVPsForConfigMap maps a ConfigMap event to the policies in the same
// namespace that reference it by name. A gateway's "{gateway}-route-vcl"
// ConfigMap instead maps to the policies listed in its manifest, in any
// namespace (slot assignment drives Programmed and status.slots).
func (r *VarnishVCLPolicyReconciler) findVVPsForConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return nil
	}
	if isRouteVCLConfigMap(cm) {
		return r.findVVPsForRouteVCLConfigMap(cm)
	}
	var list gatewayparamsv1alpha1.VarnishVCLPolicyList
	if err := r.List(ctx, &list, client.InNamespace(cm.Namespace)); err != nil {
		r.Logger.Error("failed to list VarnishVCLPolicies for ConfigMap", "configmap", client.ObjectKeyFromObject(cm), "error", err)
		return nil
	}
	return vvpRequests(list.Items, func(p *gatewayparamsv1alpha1.VarnishVCLPolicy) bool {
		return p.Spec.VCLConfigMapRef.Name == cm.Name
	})
}

// findVVPsForGateway maps a Gateway event to the policies whose target
// HTTPRoute lists that Gateway as a parent. Routes may live in any namespace,
// so all policies are considered.
func (r *VarnishVCLPolicyReconciler) findVVPsForGateway(ctx context.Context, obj client.Object) []reconcile.Request {
	gw, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}
	var list gatewayparamsv1alpha1.VarnishVCLPolicyList
	if err := r.List(ctx, &list); err != nil {
		r.Logger.Error("failed to list VarnishVCLPolicies for Gateway", "gateway", client.ObjectKeyFromObject(gw), "error", err)
		return nil
	}
	return vvpRequests(list.Items, func(p *gatewayparamsv1alpha1.VarnishVCLPolicy) bool {
		var route gatewayv1.HTTPRoute
		if err := r.Get(ctx, types.NamespacedName{Name: p.Spec.TargetRef.Name, Namespace: p.Namespace}, &route); err != nil {
			// A missing route makes the policy TargetNotFound regardless
			// of Gateways; any other error is handled by enqueueing.
			return !apierrors.IsNotFound(err)
		}
		return routeHasParentGateway(&route, gw)
	})
}

// routeHasParentGateway reports whether route lists gw in its parentRefs.
func routeHasParentGateway(route *gatewayv1.HTTPRoute, gw *gatewayv1.Gateway) bool {
	for _, ref := range route.Spec.ParentRefs {
		if ref.Group != nil && *ref.Group != gatewayv1.GroupName {
			continue
		}
		if ref.Kind != nil && *ref.Kind != "Gateway" {
			continue
		}
		ns := route.Namespace
		if ref.Namespace != nil {
			ns = string(*ref.Namespace)
		}
		if string(ref.Name) == gw.Name && ns == gw.Namespace {
			return true
		}
	}
	return false
}

// findAllVVPs maps an event to every policy in the cluster. Used for
// GatewayClass and GatewayClassParameters changes, which can flip enablement
// for routes in any namespace.
func (r *VarnishVCLPolicyReconciler) findAllVVPs(ctx context.Context, _ client.Object) []reconcile.Request {
	var list gatewayparamsv1alpha1.VarnishVCLPolicyList
	if err := r.List(ctx, &list); err != nil {
		r.Logger.Error("failed to list VarnishVCLPolicies", "error", err)
		return nil
	}
	return vvpRequests(list.Items, nil)
}

// SetupWithManager sets up the VarnishVCLPolicy controller with the Manager.
func (r *VarnishVCLPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// The primary resource passes spec changes and status.podResults
	// changes (gateway pods reporting compile results), and ignores our own
	// condition writes, which would otherwise re-trigger reconciles in a
	// loop. On HTTPRoute, Gateway and GatewayClass GenerationChangedPredicate
	// filters out the frequent status-only updates: only spec changes
	// (parentRefs, gatewayClassName, parametersRef), creates and deletes
	// affect a policy's conditions.
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{RateLimiter: defaultRateLimiter()}).
		For(&gatewayparamsv1alpha1.VarnishVCLPolicy{}, builder.WithPredicates(vvpGenerationOrPodResultsChanged())).
		// Second source on the same kind: a policy's create, retarget or
		// delete enqueues its same-target siblings (conflict re-evaluation).
		Watches(
			&gatewayparamsv1alpha1.VarnishVCLPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.findSiblingVVPs),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&gatewayv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(r.findVVPsForHTTPRoute),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.findVVPsForConfigMap),
		).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.findVVPsForGateway),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&gatewayv1.GatewayClass{},
			handler.EnqueueRequestsFromMapFunc(r.findAllVVPs),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&gatewayparamsv1alpha1.GatewayClassParameters{},
			handler.EnqueueRequestsFromMapFunc(r.findAllVVPs),
		).
		// Gateway Deployments: rollouts and pod churn change the set of
		// live pods that must report a policy's VCL before it counts as
		// Programmed. This stands in for a cluster-wide Pod watch; pods
		// themselves are only listed (through a cache restricted to
		// operator-labelled pods).
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(r.findVVPsForDeployment),
		).
		Complete(r)
}
