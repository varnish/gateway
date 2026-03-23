package controller

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
	"github.com/varnish/gateway/internal/ghost"
)

// VarnishCachePolicyReconciler reconciles VarnishCachePolicy objects.
type VarnishCachePolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config Config
	Logger *slog.Logger
}

// Reconcile validates a VarnishCachePolicy and triggers re-reconciliation of affected HTTPRoutes.
func (r *VarnishCachePolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Logger.With("vcp", req.NamespacedName)
	log.Debug("reconciling VarnishCachePolicy")

	var vcp gatewayparamsv1alpha1.VarnishCachePolicy
	if err := r.Get(ctx, req.NamespacedName, &vcp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("r.Get(%s): %w", req.NamespacedName, err)
	}

	// Validate the VCP spec
	if err := r.validateSpec(&vcp); err != nil {
		r.setAccepted(&vcp, false, "Invalid", err.Error())
		if statusErr := r.Status().Update(ctx, &vcp); statusErr != nil {
			log.Error("failed to update VCP status", "error", statusErr)
		}
		return ctrl.Result{}, nil
	}

	// Check target exists
	targetKind := string(vcp.Spec.TargetRef.Kind)
	targetName := string(vcp.Spec.TargetRef.Name)

	switch targetKind {
	case "HTTPRoute":
		var route gatewayv1.HTTPRoute
		if err := r.Get(ctx, types.NamespacedName{Name: targetName, Namespace: vcp.Namespace}, &route); err != nil {
			if apierrors.IsNotFound(err) {
				r.setAccepted(&vcp, false, "TargetNotFound",
					fmt.Sprintf("HTTPRoute %q not found in namespace %q", targetName, vcp.Namespace))
				if statusErr := r.Status().Update(ctx, &vcp); statusErr != nil {
					log.Error("failed to update VCP status", "error", statusErr)
				}
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			return ctrl.Result{}, fmt.Errorf("r.Get(HTTPRoute %s): %w", targetName, err)
		}

		// If sectionName is set, validate it matches a named rule
		if vcp.Spec.TargetRef.SectionName != nil {
			sn := string(*vcp.Spec.TargetRef.SectionName)
			found := false
			for _, rule := range route.Spec.Rules {
				if rule.Name != nil && string(*rule.Name) == sn {
					found = true
					break
				}
			}
			if !found {
				r.setAccepted(&vcp, false, "TargetNotFound",
					fmt.Sprintf("No rule named %q in HTTPRoute %q", sn, targetName))
				if statusErr := r.Status().Update(ctx, &vcp); statusErr != nil {
					log.Error("failed to update VCP status", "error", statusErr)
				}
				return ctrl.Result{}, nil
			}
		}

	case "Gateway":
		var gw gatewayv1.Gateway
		if err := r.Get(ctx, types.NamespacedName{Name: targetName, Namespace: vcp.Namespace}, &gw); err != nil {
			if apierrors.IsNotFound(err) {
				r.setAccepted(&vcp, false, "TargetNotFound",
					fmt.Sprintf("Gateway %q not found in namespace %q", targetName, vcp.Namespace))
				if statusErr := r.Status().Update(ctx, &vcp); statusErr != nil {
					log.Error("failed to update VCP status", "error", statusErr)
				}
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			return ctrl.Result{}, fmt.Errorf("r.Get(Gateway %s): %w", targetName, err)
		}

	default:
		r.setAccepted(&vcp, false, "Invalid",
			fmt.Sprintf("Unsupported target kind %q, must be Gateway or HTTPRoute", targetKind))
		if statusErr := r.Status().Update(ctx, &vcp); statusErr != nil {
			log.Error("failed to update VCP status", "error", statusErr)
		}
		return ctrl.Result{}, nil
	}

	// Check for conflicts (another VCP targeting the same route)
	if conflict := r.checkConflict(ctx, &vcp); conflict != "" {
		r.setAccepted(&vcp, false, "Conflicted", conflict)
		if statusErr := r.Status().Update(ctx, &vcp); statusErr != nil {
			log.Error("failed to update VCP status", "error", statusErr)
		}
		return ctrl.Result{}, nil
	}

	// VCP is valid - set Accepted
	r.setAccepted(&vcp, true, "Accepted", "Policy accepted")
	if statusErr := r.Status().Update(ctx, &vcp); statusErr != nil {
		log.Error("failed to update VCP status", "error", statusErr)
	}

	log.Debug("VarnishCachePolicy reconciliation complete")
	return ctrl.Result{}, nil
}

// validateSpec checks the VCP spec for correctness.
func (r *VarnishCachePolicyReconciler) validateSpec(vcp *gatewayparamsv1alpha1.VarnishCachePolicy) error {
	spec := &vcp.Spec

	// Exactly one of defaultTTL or forcedTTL must be set
	hasDefault := spec.DefaultTTL != nil
	hasForced := spec.ForcedTTL != nil
	if hasDefault && hasForced {
		return fmt.Errorf("defaultTTL and forcedTTL are mutually exclusive")
	}
	if !hasDefault && !hasForced {
		return fmt.Errorf("exactly one of defaultTTL or forcedTTL must be set")
	}

	// Validate durations are positive
	if hasDefault && spec.DefaultTTL.Duration <= 0 {
		return fmt.Errorf("defaultTTL must be positive")
	}
	if hasForced && spec.ForcedTTL.Duration <= 0 {
		return fmt.Errorf("forcedTTL must be positive")
	}
	if spec.Grace != nil && spec.Grace.Duration < 0 {
		return fmt.Errorf("grace must be non-negative")
	}
	if spec.Keep != nil && spec.Keep.Duration < 0 {
		return fmt.Errorf("keep must be non-negative")
	}

	// Cache key query params: include and exclude are mutually exclusive
	if spec.CacheKey != nil && spec.CacheKey.QueryParameters != nil {
		qp := spec.CacheKey.QueryParameters
		if len(qp.Include) > 0 && len(qp.Exclude) > 0 {
			return fmt.Errorf("cacheKey.queryParameters.include and exclude are mutually exclusive")
		}
	}

	// Target group must be gateway.networking.k8s.io
	if string(spec.TargetRef.Group) != "gateway.networking.k8s.io" {
		return fmt.Errorf("targetRef.group must be gateway.networking.k8s.io")
	}

	return nil
}

// checkConflict checks if another VCP targets the same resource with the same scope.
// Returns empty string if no conflict, or a conflict message.
func (r *VarnishCachePolicyReconciler) checkConflict(ctx context.Context, vcp *gatewayparamsv1alpha1.VarnishCachePolicy) string {
	var vcpList gatewayparamsv1alpha1.VarnishCachePolicyList
	if err := r.List(ctx, &vcpList, client.InNamespace(vcp.Namespace)); err != nil {
		return ""
	}

	for _, other := range vcpList.Items {
		if other.Name == vcp.Name {
			continue
		}
		// Same target kind, group, and name?
		if string(other.Spec.TargetRef.Kind) != string(vcp.Spec.TargetRef.Kind) {
			continue
		}
		if string(other.Spec.TargetRef.Name) != string(vcp.Spec.TargetRef.Name) {
			continue
		}
		// Same sectionName (or both nil)?
		otherSN := ""
		if other.Spec.TargetRef.SectionName != nil {
			otherSN = string(*other.Spec.TargetRef.SectionName)
		}
		thisSN := ""
		if vcp.Spec.TargetRef.SectionName != nil {
			thisSN = string(*vcp.Spec.TargetRef.SectionName)
		}
		if otherSN != thisSN {
			continue
		}

		// Conflict! Oldest wins (by creation timestamp, then name).
		if other.CreationTimestamp.Before(&vcp.CreationTimestamp) ||
			(other.CreationTimestamp.Equal(&vcp.CreationTimestamp) && other.Name < vcp.Name) {
			return fmt.Sprintf("Conflicts with VarnishCachePolicy %q (older policy takes precedence)", other.Name)
		}
	}

	return ""
}

// setAccepted sets the Accepted condition on the VCP status.
func (r *VarnishCachePolicyReconciler) setAccepted(vcp *gatewayparamsv1alpha1.VarnishCachePolicy, accepted bool, reason, message string) {
	statusVal := metav1.ConditionFalse
	if accepted {
		statusVal = metav1.ConditionTrue
	}

	condition := metav1.Condition{
		Type:               "Accepted",
		Status:             statusVal,
		ObservedGeneration: vcp.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}

	// For now, use a simple status with conditions rather than full PolicyAncestorStatus.
	// We store conditions in ancestors[0] with a self-referencing ancestorRef.
	if len(vcp.Status.Ancestors) == 0 {
		vcp.Status.Ancestors = make([]gatewayparamsv1alpha1.VarnishCachePolicyAncestorStatus, 1)
	}
	vcp.Status.Ancestors[0] = gatewayparamsv1alpha1.VarnishCachePolicyAncestorStatus{
		AncestorRef:    vcp.Spec.TargetRef,
		ControllerName: ControllerName,
		Conditions:     []metav1.Condition{condition},
	}
}

// findVCPsForHTTPRoute returns reconcile requests for all VCPs targeting this HTTPRoute.
func (r *VarnishCachePolicyReconciler) findVCPsForHTTPRoute(ctx context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}

	var vcpList gatewayparamsv1alpha1.VarnishCachePolicyList
	if err := r.List(ctx, &vcpList, client.InNamespace(route.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, vcp := range vcpList.Items {
		if string(vcp.Spec.TargetRef.Kind) == "HTTPRoute" && string(vcp.Spec.TargetRef.Name) == route.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      vcp.Name,
					Namespace: vcp.Namespace,
				},
			})
		}
	}
	return requests
}

// findVCPsForGateway returns reconcile requests for all VCPs targeting this Gateway.
func (r *VarnishCachePolicyReconciler) findVCPsForGateway(ctx context.Context, obj client.Object) []reconcile.Request {
	gw, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}

	var vcpList gatewayparamsv1alpha1.VarnishCachePolicyList
	if err := r.List(ctx, &vcpList, client.InNamespace(gw.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, vcp := range vcpList.Items {
		if string(vcp.Spec.TargetRef.Kind) == "Gateway" && string(vcp.Spec.TargetRef.Name) == gw.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      vcp.Name,
					Namespace: vcp.Namespace,
				},
			})
		}
	}
	return requests
}

// SetupWithManager sets up the VCP controller with the Manager.
func (r *VarnishCachePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayparamsv1alpha1.VarnishCachePolicy{}).
		Watches(
			&gatewayv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(r.findVCPsForHTTPRoute),
		).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.findVCPsForGateway),
		).
		Complete(r)
}

// ResolveCachePolicyForRoute looks up VCPs and returns the CachePolicy for a route.
// Precedence: rule-level VCP > route-level VCP > gateway-level VCP.
// Returns nil if no VCP applies (route stays in pass mode).
func ResolveCachePolicyForRoute(ctx context.Context, c client.Client, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway, ruleName string) *ghost.CachePolicy {
	var vcpList gatewayparamsv1alpha1.VarnishCachePolicyList
	if err := c.List(ctx, &vcpList, client.InNamespace(route.Namespace)); err != nil {
		return nil
	}

	// Also check gateway namespace for gateway-level VCPs
	var gwVCPs gatewayparamsv1alpha1.VarnishCachePolicyList
	if gateway != nil && gateway.Namespace != route.Namespace {
		if err := c.List(ctx, &gwVCPs, client.InNamespace(gateway.Namespace)); err == nil {
			vcpList.Items = append(vcpList.Items, gwVCPs.Items...)
		}
	}

	// Find applicable VCPs by precedence level
	var ruleVCP, routeVCP, gatewayVCP *gatewayparamsv1alpha1.VarnishCachePolicy

	for i := range vcpList.Items {
		vcp := &vcpList.Items[i]

		// Skip VCPs that are not accepted (check status)
		if !isVCPAccepted(vcp) {
			continue
		}

		targetKind := string(vcp.Spec.TargetRef.Kind)
		targetName := string(vcp.Spec.TargetRef.Name)

		switch {
		case targetKind == "HTTPRoute" && targetName == route.Name && vcp.Spec.TargetRef.SectionName != nil:
			// Rule-level VCP
			sn := string(*vcp.Spec.TargetRef.SectionName)
			if ruleName != "" && sn == ruleName {
				if ruleVCP == nil || isOlder(vcp, ruleVCP) {
					ruleVCP = vcp
				}
			}

		case targetKind == "HTTPRoute" && targetName == route.Name && vcp.Spec.TargetRef.SectionName == nil:
			// Route-level VCP
			if routeVCP == nil || isOlder(vcp, routeVCP) {
				routeVCP = vcp
			}

		case targetKind == "Gateway" && gateway != nil && targetName == gateway.Name:
			// Gateway-level VCP
			if gatewayVCP == nil || isOlder(vcp, gatewayVCP) {
				gatewayVCP = vcp
			}
		}
	}

	// Most specific wins
	var winner *gatewayparamsv1alpha1.VarnishCachePolicy
	switch {
	case ruleVCP != nil:
		winner = ruleVCP
	case routeVCP != nil:
		winner = routeVCP
	case gatewayVCP != nil:
		winner = gatewayVCP
	default:
		return nil
	}

	return specToCachePolicy(&winner.Spec)
}

// isVCPAccepted checks if a VCP has been accepted (status condition).
func isVCPAccepted(vcp *gatewayparamsv1alpha1.VarnishCachePolicy) bool {
	for _, ancestor := range vcp.Status.Ancestors {
		for _, cond := range ancestor.Conditions {
			if cond.Type == "Accepted" && cond.Status == metav1.ConditionTrue {
				return true
			}
		}
	}
	// No accepted condition found — treat as not accepted (safe default)
	return false
}

// isOlder returns true if a is older than b (for conflict resolution).
func isOlder(a, b *gatewayparamsv1alpha1.VarnishCachePolicy) bool {
	if a.CreationTimestamp.Before(&b.CreationTimestamp) {
		return true
	}
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.Name < b.Name
	}
	return false
}

// specToCachePolicy converts a VCP spec to a ghost.CachePolicy for routing.json.
func specToCachePolicy(spec *gatewayparamsv1alpha1.VarnishCachePolicySpec) *ghost.CachePolicy {
	cp := &ghost.CachePolicy{
		RequestCoalescing: true, // default
	}

	if spec.DefaultTTL != nil {
		seconds := int(math.Round(spec.DefaultTTL.Duration.Seconds()))
		cp.DefaultTTLSeconds = &seconds
	}
	if spec.ForcedTTL != nil {
		seconds := int(math.Round(spec.ForcedTTL.Duration.Seconds()))
		cp.ForcedTTLSeconds = &seconds
	}
	if spec.Grace != nil {
		cp.GraceSeconds = int(math.Round(spec.Grace.Duration.Seconds()))
	}
	if spec.Keep != nil {
		cp.KeepSeconds = int(math.Round(spec.Keep.Duration.Seconds()))
	}
	if spec.RequestCoalescing != nil {
		cp.RequestCoalescing = *spec.RequestCoalescing
	}

	if spec.CacheKey != nil {
		ck := &ghost.CacheKeyConfig{}
		hasData := false

		if len(spec.CacheKey.Headers) > 0 {
			ck.Headers = spec.CacheKey.Headers
			hasData = true
		}
		if spec.CacheKey.QueryParameters != nil {
			if len(spec.CacheKey.QueryParameters.Include) > 0 {
				ck.QueryParamsInclude = spec.CacheKey.QueryParameters.Include
				hasData = true
			}
			if len(spec.CacheKey.QueryParameters.Exclude) > 0 {
				ck.QueryParamsExclude = spec.CacheKey.QueryParameters.Exclude
				hasData = true
			}
		}

		if hasData {
			cp.CacheKey = ck
		}
	}

	if spec.Bypass != nil && len(spec.Bypass.Headers) > 0 {
		for _, h := range spec.Bypass.Headers {
			cp.BypassHeaders = append(cp.BypassHeaders, ghost.BypassHeaderConfig{
				Name:       h.Name,
				ValueRegex: h.ValueRegex,
			})
		}
	}

	return cp
}

// SortVCPsByPrecedence sorts VCPs by creation timestamp (oldest first) for deterministic ordering.
func SortVCPsByPrecedence(vcps []gatewayparamsv1alpha1.VarnishCachePolicy) {
	sort.Slice(vcps, func(i, j int) bool {
		if vcps[i].CreationTimestamp.Before(&vcps[j].CreationTimestamp) {
			return true
		}
		if vcps[i].CreationTimestamp.Equal(&vcps[j].CreationTimestamp) {
			return vcps[i].Name < vcps[j].Name
		}
		return false
	})
}
