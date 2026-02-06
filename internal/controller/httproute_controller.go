package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/varnish/gateway/internal/ghost"
	"github.com/varnish/gateway/internal/status"
	"github.com/varnish/gateway/internal/vcl"
)

// HTTPRouteReconciler reconciles HTTPRoute objects.
type HTTPRouteReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config Config
	Logger *slog.Logger

	// ConfigMap content tracking for change detection
	configMapHashes map[string]string // key: namespace/name -> hash of Data
}

// Reconcile handles HTTPRoute reconciliation.
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Logger.With("httproute", req.NamespacedName)
	log.Debug("reconciling HTTPRoute")

	// Initialize configMapHashes if nil
	if r.configMapHashes == nil {
		r.configMapHashes = make(map[string]string)
	}

	// 1. Fetch the HTTPRoute
	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, req.NamespacedName, &route); err != nil {
		if apierrors.IsNotFound(err) {
			// Route deleted - we need to regenerate VCL for affected Gateways
			// but we can't know which Gateway was affected without the route
			// The Gateway watch will handle this via the findHTTPRoutesForGateway mapper
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("r.Get(%s): %w", req.NamespacedName, err)
	}
	// 2. Skip if no parentRefs
	if len(route.Spec.ParentRefs) == 0 {
		log.Debug("HTTPRoute has no parentRefs, skipping")
		return ctrl.Result{}, nil
	}

	// 3. Process each parentRef
	for _, parentRef := range route.Spec.ParentRefs {
		if err := r.processParentRef(ctx, &route, parentRef); err != nil {
			// Log but continue processing other parentRefs
			log.Error("failed to process parentRef",
				"parentRef", parentRef.Name,
				"error", err)
		}
	}

	// 4. Update HTTPRoute status using Server-Side Apply - no conflicts with other controllers
	// Prepare for SSA: set GVK and ensure managedFields is cleared
	route.SetGroupVersionKind(gatewayv1.SchemeGroupVersion.WithKind("HTTPRoute"))
	route.SetManagedFields(nil)
	if err := r.Status().Patch(ctx, &route, client.Apply,
		client.FieldOwner("varnish-httproute-controller"),
		client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("r.Status().Patch: %w", err)
	}

	log.Debug("HTTPRoute reconciliation complete")
	return ctrl.Result{}, nil
}

// processParentRef handles a single parentRef for an HTTPRoute.
func (r *HTTPRouteReconciler) processParentRef(ctx context.Context, route *gatewayv1.HTTPRoute, parentRef gatewayv1.ParentReference) error {
	log := r.Logger.With("httproute", types.NamespacedName{Name: route.Name, Namespace: route.Namespace},
		"parentRef", parentRef.Name)

	// Skip if Kind is not Gateway (or empty, which defaults to Gateway)
	if parentRef.Kind != nil && *parentRef.Kind != "Gateway" {
		log.Debug("parentRef is not a Gateway, skipping")
		return nil
	}

	// Get the parent Gateway
	gateway, err := r.getParentGateway(ctx, route, parentRef)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Gateway not found - set Accepted=false
			status.SetHTTPRouteAccepted(route, parentRef, ControllerName, false,
				string(gatewayv1.RouteReasonNoMatchingParent),
				fmt.Sprintf("Gateway %s not found", parentRef.Name))
			status.SetHTTPRouteResolvedRefs(route, parentRef, ControllerName, true,
				string(gatewayv1.RouteReasonResolvedRefs),
				"References resolved")
			return nil
		}
		return fmt.Errorf("r.getParentGateway: %w", err)
	}

	// Check if Gateway uses our GatewayClass
	if string(gateway.Spec.GatewayClassName) != r.Config.GatewayClassName {
		log.Debug("Gateway uses different GatewayClass, skipping",
			"gatewayClass", gateway.Spec.GatewayClassName,
			"expected", r.Config.GatewayClassName)
		// Don't set status for Gateways managed by other controllers
		return nil
	}

	// Check if the route's namespace is allowed by the Gateway's listeners
	allowed, reason := r.isRouteAllowedByGateway(ctx, route, gateway)
	if !allowed {
		log.Info("route namespace not allowed by Gateway listeners", "reason", reason)
		status.SetHTTPRouteAccepted(route, parentRef, ControllerName, false,
			string(gatewayv1.RouteReasonNotAllowedByListeners), reason)
		status.SetHTTPRouteResolvedRefs(route, parentRef, ControllerName, true,
			string(gatewayv1.RouteReasonResolvedRefs), "All references resolved")
		return nil
	}

	// List all HTTPRoutes attached to this Gateway
	routes, err := r.listRoutesForGateway(ctx, gateway)
	if err != nil {
		return fmt.Errorf("r.listRoutesForGateway: %w", err)
	}

	// Update Gateway's ConfigMap with generated VCL
	if err := r.updateConfigMap(ctx, gateway, routes); err != nil {
		status.SetHTTPRouteAccepted(route, parentRef, ControllerName, false,
			string(gatewayv1.RouteReasonPending),
			fmt.Sprintf("Failed to update ConfigMap: %v", err))
		return fmt.Errorf("r.updateConfigMap: %w", err)
	}

	// Update Gateway's listener AttachedRoutes count
	if err := r.updateGatewayListenerStatus(ctx, gateway, routes); err != nil {
		log.Error("failed to update Gateway listener status", "error", err)
		// Don't return error - the route is still accepted
	}

	// Set success status
	status.SetHTTPRouteAccepted(route, parentRef, ControllerName, true,
		string(gatewayv1.RouteReasonAccepted),
		"Route accepted")
	status.SetHTTPRouteResolvedRefs(route, parentRef, ControllerName, true,
		string(gatewayv1.RouteReasonResolvedRefs),
		"All references resolved")

	return nil
}

// getParentGateway fetches the Gateway referenced by parentRef.
func (r *HTTPRouteReconciler) getParentGateway(ctx context.Context, route *gatewayv1.HTTPRoute, parentRef gatewayv1.ParentReference) (*gatewayv1.Gateway, error) {
	// Determine namespace - default to route's namespace
	namespace := route.Namespace
	if parentRef.Namespace != nil {
		namespace = string(*parentRef.Namespace)
	}

	var gateway gatewayv1.Gateway
	err := r.Get(ctx, types.NamespacedName{
		Name:      string(parentRef.Name),
		Namespace: namespace,
	}, &gateway)
	if err != nil {
		return nil, err
	}

	return &gateway, nil
}

// listRoutesForGateway returns all HTTPRoutes attached to a Gateway.
func (r *HTTPRouteReconciler) listRoutesForGateway(ctx context.Context, gateway *gatewayv1.Gateway) ([]gatewayv1.HTTPRoute, error) {
	var routeList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routeList); err != nil {
		return nil, fmt.Errorf("r.List(HTTPRouteList): %w", err)
	}

	var attached []gatewayv1.HTTPRoute
	for _, route := range routeList.Items {
		if !r.routeAttachedToGateway(&route, gateway) {
			continue
		}
		allowed, _ := r.isRouteAllowedByGateway(ctx, &route, gateway)
		if !allowed {
			continue
		}
		attached = append(attached, route)
	}

	return attached, nil
}

// routeAttachedToGateway checks if a route references the given Gateway.
func (r *HTTPRouteReconciler) routeAttachedToGateway(route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) bool {
	for _, parentRef := range route.Spec.ParentRefs {
		// Skip non-Gateway refs
		if parentRef.Kind != nil && *parentRef.Kind != "Gateway" {
			continue
		}

		// Check name
		if string(parentRef.Name) != gateway.Name {
			continue
		}

		// Check namespace
		refNamespace := route.Namespace
		if parentRef.Namespace != nil {
			refNamespace = string(*parentRef.Namespace)
		}
		if refNamespace != gateway.Namespace {
			continue
		}

		// Check group (default to gateway.networking.k8s.io)
		if parentRef.Group != nil && *parentRef.Group != gatewayv1.Group(gatewayv1.GroupName) {
			continue
		}

		return true
	}
	return false
}

// isRouteAllowedByGateway checks if the route's namespace is allowed by any of the Gateway's listeners.
// Returns (true, "") if any listener allows the route, or (false, reason) if none do.
func (r *HTTPRouteReconciler) isRouteAllowedByGateway(ctx context.Context, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) (bool, string) {
	if len(gateway.Spec.Listeners) == 0 {
		return false, "Gateway has no listeners"
	}

	for _, listener := range gateway.Spec.Listeners {
		allowed, err := r.listenerAllowsRouteNamespace(ctx, route, gateway, listener)
		if err != nil {
			r.Logger.Error("failed to check listener namespace policy",
				"listener", listener.Name, "error", err)
			continue
		}
		if allowed {
			return true, ""
		}
	}

	return false, fmt.Sprintf("Route namespace %q is not allowed by any listener on Gateway %s/%s",
		route.Namespace, gateway.Namespace, gateway.Name)
}

// listenerAllowsRouteNamespace checks if a specific listener allows routes from the route's namespace.
func (r *HTTPRouteReconciler) listenerAllowsRouteNamespace(ctx context.Context, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway, listener gatewayv1.Listener) (bool, error) {
	// Determine the "from" policy. Default is Same when AllowedRoutes or Namespaces or From is nil.
	from := gatewayv1.NamespacesFromSame
	if listener.AllowedRoutes != nil && listener.AllowedRoutes.Namespaces != nil && listener.AllowedRoutes.Namespaces.From != nil {
		from = *listener.AllowedRoutes.Namespaces.From
	}

	switch from {
	case gatewayv1.NamespacesFromAll:
		return true, nil

	case gatewayv1.NamespacesFromSame:
		return route.Namespace == gateway.Namespace, nil

	case gatewayv1.NamespacesFromSelector:
		if listener.AllowedRoutes.Namespaces.Selector == nil {
			return false, nil
		}
		selector, err := metav1.LabelSelectorAsSelector(listener.AllowedRoutes.Namespaces.Selector)
		if err != nil {
			return false, fmt.Errorf("metav1.LabelSelectorAsSelector: %w", err)
		}
		// Fetch the route's namespace to check labels
		var ns corev1.Namespace
		if err := r.Get(ctx, types.NamespacedName{Name: route.Namespace}, &ns); err != nil {
			return false, fmt.Errorf("r.Get(Namespace %s): %w", route.Namespace, err)
		}
		return selector.Matches(labels.Set(ns.Labels)), nil

	default:
		return false, fmt.Errorf("unknown FromNamespaces value: %s", from)
	}
}

// updateConfigMap updates the Gateway's ConfigMap with routing.json (preserves main.vcl).
func (r *HTTPRouteReconciler) updateConfigMap(ctx context.Context, gateway *gatewayv1.Gateway, routes []gatewayv1.HTTPRoute) error {
	// Generate v2 routing.json for ghost with path-based routing
	collectedRoutes := vcl.CollectHTTPRouteBackendsV2(routes, gateway.Namespace)

	// Group routes by hostname
	routesByHost := make(map[string][]ghost.Route)
	for _, route := range collectedRoutes {
		routesByHost[route.Hostname] = append(routesByHost[route.Hostname], route)
	}

	// Generate v2 routing config
	routingConfig := ghost.GenerateRoutingConfigV2(routesByHost, nil)
	routingJSON, err := json.MarshalIndent(routingConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("json.MarshalIndent: %w", err)
	}

	// Fetch existing ConfigMap
	cmName := fmt.Sprintf("%s-vcl", gateway.Name)
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: gateway.Namespace}, &cm); err != nil {
		return fmt.Errorf("r.Get(%s): %w", cmName, err)
	}

	// Compute hash of new routing.json
	cmKey := fmt.Sprintf("%s/%s", gateway.Namespace, cmName)
	oldHash := r.configMapHashes[cmKey]
	newHash := r.computeConfigMapHash(string(routingJSON))

	// Update ConfigMap data (only routing.json, preserve main.vcl owned by Gateway controller)
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data["routing.json"] = string(routingJSON)

	if err := r.Update(ctx, &cm); err != nil {
		return fmt.Errorf("r.Update(%s): %w", cmName, err)
	}

	// Store new hash
	r.configMapHashes[cmKey] = newHash

	// Only log if content actually changed
	if oldHash != newHash {
		r.Logger.Info("updated ConfigMap",
			"configmap", cmName,
			"routes", len(routes),
			"backends", len(collectedRoutes))
	} else {
		r.Logger.Debug("reconciled ConfigMap (no content change)",
			"configmap", cmName,
			"routes", len(routes),
			"backends", len(collectedRoutes))
	}

	return nil
}

// computeConfigMapHash computes a hash of the routing.json content
func (r *HTTPRouteReconciler) computeConfigMapHash(routingJSON string) string {
	h := sha256.New()
	h.Write([]byte(routingJSON))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// updateGatewayListenerStatus updates AttachedRoutes count on Gateway listeners.
// Creates a minimal patch object to avoid conflicts with Gateway controller.
func (r *HTTPRouteReconciler) updateGatewayListenerStatus(ctx context.Context, gateway *gatewayv1.Gateway, routes []gatewayv1.HTTPRoute) error {
	// Count routes per listener
	attachedCount := int32(len(routes))

	// Create minimal Gateway object for SSA patch - only include fields we own
	patch := &gatewayv1.Gateway{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gatewayv1.GroupVersion.String(),
			Kind:       "Gateway",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      gateway.Name,
			Namespace: gateway.Namespace,
		},
	}

	// Build listener statuses with only AttachedRoutes field
	// We must include SupportedKinds even though Gateway controller owns it,
	// because it's a required field for validation
	patch.Status.Listeners = make([]gatewayv1.ListenerStatus, len(gateway.Status.Listeners))
	for i, listener := range gateway.Status.Listeners {
		patch.Status.Listeners[i] = gatewayv1.ListenerStatus{
			Name:           listener.Name,
			AttachedRoutes: attachedCount,
			// Include SupportedKinds to satisfy API validation (required field)
			// Gateway controller is the field owner, but we need to set it to avoid null
			SupportedKinds: []gatewayv1.RouteGroupKind{
				{
					Group: ptr(gatewayv1.Group("gateway.networking.k8s.io")),
					Kind:  "HTTPRoute",
				},
			},
			// DO NOT set Conditions - those are owned by Gateway controller
		}
	}

	// Use Server-Side Apply - HTTPRoute controller owns AttachedRoutes field
	// Gateway controller owns conditions - no conflicts!
	if err := r.Status().Patch(ctx, patch, client.Apply,
		client.FieldOwner("varnish-httproute-controller"),
		client.ForceOwnership); err != nil {
		return fmt.Errorf("r.Status().Patch: %w", err)
	}

	return nil
}

// findHTTPRoutesForGateway returns reconcile requests for all HTTPRoutes
// attached to a Gateway when the Gateway changes.
func (r *HTTPRouteReconciler) findHTTPRoutesForGateway(ctx context.Context, obj client.Object) []reconcile.Request {
	gateway, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}

	// Skip Gateways that don't use our GatewayClass
	if string(gateway.Spec.GatewayClassName) != r.Config.GatewayClassName {
		return nil
	}

	// List all HTTPRoutes
	var routeList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routeList); err != nil {
		r.Logger.Error("failed to list HTTPRoutes", "error", err)
		return nil
	}

	// Find routes attached to this Gateway
	var requests []reconcile.Request
	for _, route := range routeList.Items {
		if r.routeAttachedToGateway(&route, gateway) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      route.Name,
					Namespace: route.Namespace,
				},
			})
		}
	}

	if len(requests) > 0 {
		r.Logger.Debug("Gateway changed, re-reconciling attached HTTPRoutes",
			"gateway", gateway.Name,
			"routes", len(requests))
	}

	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.findHTTPRoutesForGateway),
			// Only trigger on spec changes (generation bump), ignore status-only updates
			// to prevent reconciliation loops between HTTPRoute and Gateway controllers
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Complete(r)
}
