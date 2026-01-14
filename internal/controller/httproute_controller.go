package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
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
}

// Reconcile handles HTTPRoute reconciliation.
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Logger.With("httproute", req.NamespacedName)
	log.Info("reconciling HTTPRoute")

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

	// 4. Update HTTPRoute status
	if err := r.Status().Update(ctx, &route); err != nil {
		return ctrl.Result{}, fmt.Errorf("r.Status().Update: %w", err)
	}

	log.Info("HTTPRoute reconciliation complete")
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
		if r.routeAttachedToGateway(&route, gateway) {
			attached = append(attached, route)
		}
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

// updateConfigMap updates the Gateway's VCL ConfigMap with generated VCL and routing.json.
func (r *HTTPRouteReconciler) updateConfigMap(ctx context.Context, gateway *gatewayv1.Gateway, routes []gatewayv1.HTTPRoute) error {
	// Generate VCL from routes (ghost preamble)
	generatedVCL := vcl.Generate(routes, vcl.GeneratorConfig{})

	// Get user VCL (placeholder for future GatewayClassParameters support)
	userVCL := r.getUserVCL(ctx, gateway)
	finalVCL := vcl.Merge(generatedVCL, userVCL)

	// Generate routing.json for ghost
	routeBackends := vcl.CollectHTTPRouteBackends(routes, gateway.Namespace)
	routingConfig := ghost.GenerateRoutingConfig(routeBackends, nil)
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

	// Update ConfigMap data
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data["main.vcl"] = finalVCL
	cm.Data["routing.json"] = string(routingJSON)

	if err := r.Update(ctx, &cm); err != nil {
		return fmt.Errorf("r.Update(%s): %w", cmName, err)
	}

	r.Logger.Info("updated ConfigMap",
		"configmap", cmName,
		"routes", len(routes),
		"backends", len(routeBackends))

	return nil
}

// getUserVCL returns user-provided VCL from GatewayClassParameters.
// It traverses: Gateway -> GatewayClass -> GatewayClassParameters -> ConfigMap
func (r *HTTPRouteReconciler) getUserVCL(ctx context.Context, gateway *gatewayv1.Gateway) string {
	// 1. Get GatewayClass
	var gatewayClass gatewayv1.GatewayClass
	if err := r.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, &gatewayClass); err != nil {
		if !apierrors.IsNotFound(err) {
			r.Logger.Error("failed to get GatewayClass", "error", err)
		}
		return ""
	}

	// 2. Check if ParametersRef is set
	if gatewayClass.Spec.ParametersRef == nil {
		return ""
	}

	// 3. Validate ParametersRef points to our CRD
	ref := gatewayClass.Spec.ParametersRef
	if string(ref.Group) != gatewayparamsv1alpha1.GroupName ||
		string(ref.Kind) != "GatewayClassParameters" {
		return "" // Not our parameters type
	}

	// 4. Fetch GatewayClassParameters
	var params gatewayparamsv1alpha1.GatewayClassParameters
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, &params); err != nil {
		if !apierrors.IsNotFound(err) {
			r.Logger.Error("failed to get GatewayClassParameters",
				"name", ref.Name, "error", err)
		}
		return ""
	}

	// 5. If UserVCLConfigMapRef is not set, return empty
	if params.Spec.UserVCLConfigMapRef == nil {
		return ""
	}

	// 6. Fetch the ConfigMap containing user VCL
	cmRef := params.Spec.UserVCLConfigMapRef
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{
		Name:      cmRef.Name,
		Namespace: cmRef.Namespace,
	}, &cm); err != nil {
		r.Logger.Error("failed to get user VCL ConfigMap",
			"namespace", cmRef.Namespace, "name", cmRef.Name, "error", err)
		return ""
	}

	// 7. Return VCL from ConfigMap (default key is "user.vcl")
	key := cmRef.Key
	if key == "" {
		key = "user.vcl"
	}

	userVCL, ok := cm.Data[key]
	if !ok {
		r.Logger.Warn("user VCL ConfigMap missing expected key",
			"namespace", cmRef.Namespace, "name", cmRef.Name, "key", key)
		return ""
	}

	r.Logger.Debug("loaded user VCL from ConfigMap",
		"namespace", cmRef.Namespace, "name", cmRef.Name, "key", key)

	return userVCL
}

// updateGatewayListenerStatus updates AttachedRoutes count on Gateway listeners.
func (r *HTTPRouteReconciler) updateGatewayListenerStatus(ctx context.Context, gateway *gatewayv1.Gateway, routes []gatewayv1.HTTPRoute) error {
	// Count routes per listener
	attachedCount := int32(len(routes))

	// Update each listener's AttachedRoutes count
	for i := range gateway.Status.Listeners {
		gateway.Status.Listeners[i].AttachedRoutes = attachedCount
	}

	if err := r.Status().Update(ctx, gateway); err != nil {
		return fmt.Errorf("r.Status().Update: %w", err)
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
		r.Logger.Info("Gateway changed, re-reconciling attached HTTPRoutes",
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
		).
		Complete(r)
}
