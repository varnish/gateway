package controller

import (
	"context"
	"encoding/pem"
	"fmt"
	"log/slog"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/varnish/gateway/internal/status"
)

const (
	// caCertKey is the key in a ConfigMap containing the CA certificate PEM data.
	caCertKey = "ca.crt"
)

// serviceNamesFromPolicy extracts target Service names from a BackendTLSPolicy.
func serviceNamesFromPolicy(policy *gatewayv1.BackendTLSPolicy) map[string]struct{} {
	names := make(map[string]struct{})
	for _, ref := range policy.Spec.TargetRefs {
		if ref.Group != "" || ref.Kind != "Service" {
			continue
		}
		names[string(ref.Name)] = struct{}{}
	}
	return names
}

// BackendTLSPolicyReconciler validates CA certificate references, detects conflicts,
// and sets Accepted/ResolvedRefs status conditions on BackendTLSPolicy resources.
type BackendTLSPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Logger *slog.Logger
}
func (r *BackendTLSPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Logger.With("backendtlspolicy", req.NamespacedName)
	log.Debug("reconciling BackendTLSPolicy")

	var policy gatewayv1.BackendTLSPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("r.Get(%s): %w", req.NamespacedName, err)
	}

	// Find ancestor Gateways for this policy.
	// A BackendTLSPolicy targets Services; Gateways are ancestors via HTTPRoutes.
	gateways, err := r.findAncestorGateways(ctx, &policy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("findAncestorGateways: %w", err)
	}

	if len(gateways) == 0 {
		log.Debug("no ancestor Gateways found for BackendTLSPolicy")
		// Clear our status entries if no gateways reference this policy
		r.clearOurStatus(&policy)
		if err := r.Status().Update(ctx, &policy); err != nil {
			return ctrl.Result{}, fmt.Errorf("r.Status().Update (clear): %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Validate CA certificate references
	resolvedRefs, resolvedReason, resolvedMessage := r.validateCACertificateRefs(ctx, &policy)

	// Detect conflicts with other policies targeting the same service+sectionName
	conflicted := r.isConflicted(ctx, &policy)

	// Determine Accepted condition
	var acceptedStatus metav1.ConditionStatus
	var acceptedReason, acceptedMessage string

	switch {
	case conflicted:
		acceptedStatus = metav1.ConditionFalse
		acceptedReason = string(gatewayv1.PolicyReasonConflicted)
		acceptedMessage = "Another BackendTLSPolicy targeting the same Service has higher precedence"
	case !resolvedRefs:
		acceptedStatus = metav1.ConditionFalse
		acceptedReason = string(gatewayv1.BackendTLSPolicyReasonNoValidCACertificate)
		acceptedMessage = resolvedMessage
	default:
		acceptedStatus = metav1.ConditionTrue
		acceptedReason = string(gatewayv1.PolicyReasonAccepted)
		acceptedMessage = "Policy accepted"
	}

	// Set status for each ancestor Gateway
	for _, gw := range gateways {
		r.setAncestorStatus(&policy, gw,
			acceptedStatus, acceptedReason, acceptedMessage,
			resolvedRefs, resolvedReason, resolvedMessage)
	}

	// Update status
	if err := r.Status().Update(ctx, &policy); err != nil {
		return ctrl.Result{}, fmt.Errorf("r.Status().Update: %w", err)
	}

	log.Debug("BackendTLSPolicy reconciliation complete",
		"gateways", len(gateways),
		"accepted", acceptedStatus == metav1.ConditionTrue)
	return ctrl.Result{}, nil
}

// validateCACertificateRefs checks all CA certificate references in the policy.
// Returns (resolved, reason, message).
func (r *BackendTLSPolicyReconciler) validateCACertificateRefs(ctx context.Context, policy *gatewayv1.BackendTLSPolicy) (bool, string, string) {
	// WellKnownCACertificates: System is always valid
	if policy.Spec.Validation.WellKnownCACertificates != nil &&
		*policy.Spec.Validation.WellKnownCACertificates == gatewayv1.WellKnownCACertificatesSystem {
		return true, string(gatewayv1.BackendTLSPolicyReasonResolvedRefs), "References resolved"
	}

	if len(policy.Spec.Validation.CACertificateRefs) == 0 {
		return false, string(gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef),
			"No CA certificate references specified"
	}

	for _, ref := range policy.Spec.Validation.CACertificateRefs {
		// Only ConfigMap with core group is supported
		if ref.Group != "" || ref.Kind != "ConfigMap" {
			return false, string(gatewayv1.BackendTLSPolicyReasonInvalidKind),
				fmt.Sprintf("Unsupported CA certificate ref kind: %s.%s", ref.Kind, ref.Group)
		}

		// Check that the ConfigMap exists and contains a ca.crt key
		var cm corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{
			Name:      string(ref.Name),
			Namespace: policy.Namespace,
		}, &cm); err != nil {
			if apierrors.IsNotFound(err) {
				return false, string(gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef),
					fmt.Sprintf("ConfigMap %q not found", ref.Name)
			}
			return false, string(gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef),
				fmt.Sprintf("Failed to get ConfigMap %q: %v", ref.Name, err)
		}

		caCert, ok := cm.Data[caCertKey]
		if !ok {
			return false, string(gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef),
				fmt.Sprintf("ConfigMap %q does not contain key %q", ref.Name, caCertKey)
		}

		block, _ := pem.Decode([]byte(caCert))
		if block == nil {
			return false, string(gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef),
				fmt.Sprintf("ConfigMap %q key \"ca.crt\" does not contain valid PEM data", ref.Name)
		}
	}

	return true, string(gatewayv1.BackendTLSPolicyReasonResolvedRefs), "References resolved"
}

// isConflicted checks if this policy conflicts with another policy targeting
// the same Service+sectionName. Per GEP-713, the oldest policy wins (by
// creation timestamp, then alphabetical by namespace/name).
func (r *BackendTLSPolicyReconciler) isConflicted(ctx context.Context, policy *gatewayv1.BackendTLSPolicy) bool {
	// List all BackendTLSPolicies in the same namespace
	var policyList gatewayv1.BackendTLSPolicyList
	if err := r.List(ctx, &policyList, client.InNamespace(policy.Namespace)); err != nil {
		r.Logger.Error("failed to list BackendTLSPolicies for conflict detection", "error", err)
		return false
	}

	// For each targetRef in this policy, check if another policy targets
	// the same Service+sectionName with higher precedence
	for _, targetRef := range policy.Spec.TargetRefs {
		if targetRef.Group != "" || targetRef.Kind != "Service" {
			continue
		}

		for i := range policyList.Items {
			other := &policyList.Items[i]
			if other.Name == policy.Name && other.Namespace == policy.Namespace {
				continue
			}

			for _, otherTargetRef := range other.Spec.TargetRefs {
				if otherTargetRef.Group != "" || otherTargetRef.Kind != "Service" {
					continue
				}

				if !targetRefsConflict(targetRef, otherTargetRef) {
					continue
				}

				// Conflict detected — check precedence
				if policyHasPrecedence(other, policy) {
					return true
				}
			}
		}
	}

	return false
}

// targetRefsConflict checks if two target refs conflict (same service, overlapping sectionName).
func targetRefsConflict(a, b gatewayv1.LocalPolicyTargetReferenceWithSectionName) bool {
	if string(a.Name) != string(b.Name) {
		return false
	}

	// Both have sectionName — must match to conflict
	if a.SectionName != nil && b.SectionName != nil {
		return string(*a.SectionName) == string(*b.SectionName)
	}

	// Both have no sectionName — conflict (both target entire service)
	if a.SectionName == nil && b.SectionName == nil {
		return true
	}

	// One has sectionName, other doesn't — no conflict per spec
	// (they target different scopes)
	return false
}

// policyHasPrecedence returns true if policy a has precedence over policy b.
// Oldest creation timestamp wins; ties broken by alphabetical namespace/name.
func policyHasPrecedence(a, b *gatewayv1.BackendTLSPolicy) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	// Same timestamp: alphabetical by namespace/name
	aKey := a.Namespace + "/" + a.Name
	bKey := b.Namespace + "/" + b.Name
	return aKey < bKey
}

// findAncestorGateways finds all Gateways that are ancestors of the policy's
// targeted Services (via HTTPRoutes).
func (r *BackendTLSPolicyReconciler) findAncestorGateways(ctx context.Context, policy *gatewayv1.BackendTLSPolicy) ([]*gatewayv1.Gateway, error) {
	serviceNames := serviceNamesFromPolicy(policy)
	if len(serviceNames) == 0 {
		return nil, nil
	}

	// Find HTTPRoutes that reference these services
	var routeList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routeList); err != nil {
		return nil, fmt.Errorf("List(HTTPRouteList): %w", err)
	}

	// Collect unique Gateways referenced by these routes
	gwMap := make(map[types.NamespacedName]*gatewayv1.Gateway)
	for _, route := range routeList.Items {
		// Check if this route references any of the policy's target services
		referencesTarget := false
		for svcName := range serviceNames {
			if routeReferencesService(&route, svcName, policy.Namespace) {
				referencesTarget = true
				break
			}
		}
		if !referencesTarget {
			continue
		}

		// Collect Gateways from parentRefs
		for _, parentRef := range route.Spec.ParentRefs {
			if parentRef.Kind != nil && *parentRef.Kind != "Gateway" {
				continue
			}
			if parentRef.Group != nil && *parentRef.Group != gatewayv1.Group(gatewayv1.GroupName) {
				continue
			}

			ns := route.Namespace
			if parentRef.Namespace != nil {
				ns = string(*parentRef.Namespace)
			}
			gwNN := types.NamespacedName{Name: string(parentRef.Name), Namespace: ns}

			if _, exists := gwMap[gwNN]; exists {
				continue
			}

			var gw gatewayv1.Gateway
			if err := r.Get(ctx, gwNN, &gw); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, fmt.Errorf("Get(Gateway %s): %w", gwNN, err)
			}

			// Only include Gateways managed by us
			if !isOurGatewayClass(ctx, r.Client, string(gw.Spec.GatewayClassName)) {
				continue
			}

			gwMap[gwNN] = gw.DeepCopy()
		}
	}

	gateways := make([]*gatewayv1.Gateway, 0, len(gwMap))
	for _, gw := range gwMap {
		gateways = append(gateways, gw)
	}

	// Sort for deterministic status ordering
	sort.Slice(gateways, func(i, j int) bool {
		ki := gateways[i].Namespace + "/" + gateways[i].Name
		kj := gateways[j].Namespace + "/" + gateways[j].Name
		return ki < kj
	})

	return gateways, nil
}

// setAncestorStatus sets the policy status conditions for a specific ancestor Gateway.
func (r *BackendTLSPolicyReconciler) setAncestorStatus(
	policy *gatewayv1.BackendTLSPolicy,
	gw *gatewayv1.Gateway,
	acceptedStatus metav1.ConditionStatus, acceptedReason, acceptedMessage string,
	resolvedRefs bool, resolvedReason, resolvedMessage string,
) {
	gwNamespace := gatewayv1.Namespace(gw.Namespace)
	gwGroup := gatewayv1.Group(gatewayv1.GroupName)
	gwKind := gatewayv1.Kind("Gateway")
	ancestorRef := gatewayv1.ParentReference{
		Group:     &gwGroup,
		Kind:      &gwKind,
		Namespace: &gwNamespace,
		Name:      gatewayv1.ObjectName(gw.Name),
	}

	// Find or create ancestor status entry
	var ancestor *gatewayv1.PolicyAncestorStatus
	for i := range policy.Status.Ancestors {
		a := &policy.Status.Ancestors[i]
		if string(a.ControllerName) == ControllerName &&
			a.AncestorRef.Name == ancestorRef.Name &&
			(a.AncestorRef.Namespace == nil || string(*a.AncestorRef.Namespace) == gw.Namespace) {
			ancestor = a
			break
		}
	}
	if ancestor == nil {
		policy.Status.Ancestors = append(policy.Status.Ancestors, gatewayv1.PolicyAncestorStatus{
			AncestorRef:    ancestorRef,
			ControllerName: gatewayv1.GatewayController(ControllerName),
			Conditions:     []metav1.Condition{},
		})
		ancestor = &policy.Status.Ancestors[len(policy.Status.Ancestors)-1]
	}

	acceptedCond := status.NewCondition(
		string(gatewayv1.PolicyConditionAccepted), acceptedStatus,
		policy.Generation, acceptedReason, acceptedMessage)
	status.SetCondition(&ancestor.Conditions, acceptedCond)

	resolvedRefsCond := status.NewCondition(
		string(gatewayv1.BackendTLSPolicyConditionResolvedRefs), status.BoolToStatus(resolvedRefs),
		policy.Generation, resolvedReason, resolvedMessage)
	status.SetCondition(&ancestor.Conditions, resolvedRefsCond)
}

// clearOurStatus removes status entries owned by our controller.
func (r *BackendTLSPolicyReconciler) clearOurStatus(policy *gatewayv1.BackendTLSPolicy) {
	var kept []gatewayv1.PolicyAncestorStatus
	for _, a := range policy.Status.Ancestors {
		if string(a.ControllerName) != ControllerName {
			kept = append(kept, a)
		}
	}
	policy.Status.Ancestors = kept
}

// findBackendTLSPoliciesForHTTPRoute maps HTTPRoute changes to BackendTLSPolicy reconcile requests.
func (r *BackendTLSPolicyReconciler) findBackendTLSPoliciesForHTTPRoute(ctx context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}

	// Collect service names referenced by this route
	serviceNames := make(map[string]struct{})
	for _, rule := range route.Spec.Rules {
		for _, backendRef := range rule.BackendRefs {
			if backendRef.Kind != nil && *backendRef.Kind != "Service" {
				continue
			}
			if backendRef.Group != nil && *backendRef.Group != "" {
				continue
			}
			serviceNames[string(backendRef.Name)] = struct{}{}
		}
	}

	if len(serviceNames) == 0 {
		return nil
	}

	// Find BackendTLSPolicies targeting any of these services
	var policyList gatewayv1.BackendTLSPolicyList
	if err := r.List(ctx, &policyList, client.InNamespace(route.Namespace)); err != nil {
		r.Logger.Error("failed to list BackendTLSPolicies", "error", err)
		return nil
	}

	var requests []reconcile.Request
	for _, policy := range policyList.Items {
		for _, targetRef := range policy.Spec.TargetRefs {
			if _, ok := serviceNames[string(targetRef.Name)]; ok {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      policy.Name,
						Namespace: policy.Namespace,
					},
				})
				break
			}
		}
	}

	return requests
}

// findBackendTLSPoliciesForGateway maps Gateway changes to BackendTLSPolicy reconcile requests.
func (r *BackendTLSPolicyReconciler) findBackendTLSPoliciesForGateway(ctx context.Context, obj client.Object) []reconcile.Request {
	gateway, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}

	if !isOurGatewayClass(ctx, r.Client, string(gateway.Spec.GatewayClassName)) {
		return nil
	}

	// List all BackendTLSPolicies — any of them might need status updates
	// when a Gateway changes (new ancestor or removed ancestor).
	var policyList gatewayv1.BackendTLSPolicyList
	if err := r.List(ctx, &policyList, client.InNamespace(gateway.Namespace)); err != nil {
		r.Logger.Error("failed to list BackendTLSPolicies for Gateway watch", "error", err)
		return nil
	}

	var requests []reconcile.Request
	for _, policy := range policyList.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      policy.Name,
				Namespace: policy.Namespace,
			},
		})
	}

	return requests
}

// findBackendTLSPoliciesForConfigMap re-reconciles BackendTLSPolicies when a ConfigMap
// referenced as a CA cert changes.
func (r *BackendTLSPolicyReconciler) findBackendTLSPoliciesForConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return nil
	}

	var policyList gatewayv1.BackendTLSPolicyList
	if err := r.List(ctx, &policyList, client.InNamespace(cm.Namespace)); err != nil {
		r.Logger.Error("failed to list BackendTLSPolicies for ConfigMap watch", "error", err)
		return nil
	}

	var requests []reconcile.Request
	for _, policy := range policyList.Items {
		for _, ref := range policy.Spec.Validation.CACertificateRefs {
			if ref.Kind == "ConfigMap" && string(ref.Name) == cm.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      policy.Name,
						Namespace: policy.Namespace,
					},
				})
				break
			}
		}
	}

	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackendTLSPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.BackendTLSPolicy{}).
		Watches(
			&gatewayv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(r.findBackendTLSPoliciesForHTTPRoute),
		).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.findBackendTLSPoliciesForGateway),
		).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.findBackendTLSPoliciesForConfigMap),
		).
		Complete(r)
}
