package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

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
			// Route deleted - update AttachedRoutes on all Gateways with our GatewayClass
			r.updateAttachedRoutesOnDeletion(ctx, log)
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
	var processErr error
	for _, parentRef := range route.Spec.ParentRefs {
		if err := r.processParentRef(ctx, &route, parentRef); err != nil {
			// Log but continue processing other parentRefs
			log.Error("failed to process parentRef",
				"parentRef", parentRef.Name,
				"error", err)
			processErr = err
		}
	}

	// 4. Update HTTPRoute status
	// Re-fetch the route to get the latest resourceVersion before updating status.
	// processParentRef may have triggered other writes (e.g., ConfigMap, Gateway status)
	// and the original object may be stale.
	routeStatus := route.Status
	if err := r.Get(ctx, req.NamespacedName, &route); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("r.Get (re-fetch for status): %w", err)
	}
	route.Status = routeStatus
	if err := r.Status().Update(ctx, &route); err != nil {
		return ctrl.Result{}, fmt.Errorf("r.Status().Update: %w", err)
	}

	// If any parentRef failed (e.g., ConfigMap not yet created), requeue
	if processErr != nil {
		log.Info("requeuing HTTPRoute due to processing error", "error", processErr)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
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
		status.SetHTTPRouteAccepted(route, parentRef, ControllerName, false,
			string(gatewayv1.RouteReasonPending),
			fmt.Sprintf("Failed to get Gateway %s: %v", parentRef.Name, err))
		status.SetHTTPRouteResolvedRefs(route, parentRef, ControllerName, true,
			string(gatewayv1.RouteReasonResolvedRefs),
			"References resolved")
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

	// Validate sectionName references an actual listener
	if parentRef.SectionName != nil {
		found := false
		for _, listener := range gateway.Spec.Listeners {
			if string(listener.Name) == string(*parentRef.SectionName) {
				found = true
				break
			}
		}
		if !found {
			log.Info("parentRef sectionName does not match any listener",
				"sectionName", *parentRef.SectionName)
			status.SetHTTPRouteAccepted(route, parentRef, ControllerName, false,
				string(gatewayv1.RouteReasonNoMatchingParent),
				fmt.Sprintf("No listener named %q on Gateway %s", *parentRef.SectionName, gateway.Name))
			status.SetHTTPRouteResolvedRefs(route, parentRef, ControllerName, true,
				string(gatewayv1.RouteReasonResolvedRefs),
				"References resolved")
			r.updateAttachedRoutesForGateway(ctx, gateway)
			return nil
		}
	}

	// Check if the route's namespace is allowed by the Gateway's listeners
	allowed, reasonCode, reason := r.isRouteAllowedByGateway(ctx, route, gateway)
	if !allowed {
		log.Info("route not allowed by Gateway listeners", "reasonCode", reasonCode, "reason", reason)
		status.SetHTTPRouteAccepted(route, parentRef, ControllerName, false,
			reasonCode, reason)
		status.SetHTTPRouteResolvedRefs(route, parentRef, ControllerName, true,
			string(gatewayv1.RouteReasonResolvedRefs), "All references resolved")
		r.updateAttachedRoutesForGateway(ctx, gateway)
		return nil
	}

	// List all HTTPRoutes attached to this Gateway
	routes, err := r.listRoutesForGateway(ctx, gateway)
	if err != nil {
		status.SetHTTPRouteAccepted(route, parentRef, ControllerName, false,
			string(gatewayv1.RouteReasonPending),
			fmt.Sprintf("Failed to list routes for Gateway: %v", err))
		status.SetHTTPRouteResolvedRefs(route, parentRef, ControllerName, true,
			string(gatewayv1.RouteReasonResolvedRefs),
			"References resolved")
		return fmt.Errorf("r.listRoutesForGateway: %w", err)
	}

	// Update Gateway's listener AttachedRoutes count (control plane, independent of data plane)
	if err := r.updateGatewayListenerStatus(ctx, gateway, routes); err != nil {
		log.Error("failed to update Gateway listener status", "error", err)
		// Don't return error - the route is still accepted
	}

	// Update Gateway's ConfigMap with generated VCL
	if err := r.updateConfigMap(ctx, gateway, routes); err != nil {
		// If ConfigMap doesn't exist yet, set Pending and return wrapped error for requeue
		status.SetHTTPRouteAccepted(route, parentRef, ControllerName, false,
			string(gatewayv1.RouteReasonPending),
			fmt.Sprintf("Failed to update ConfigMap: %v", err))
		status.SetHTTPRouteResolvedRefs(route, parentRef, ControllerName, true,
			string(gatewayv1.RouteReasonResolvedRefs),
			"References resolved")
		return fmt.Errorf("r.updateConfigMap: %w", err)
	}

	// Set success status
	status.SetHTTPRouteAccepted(route, parentRef, ControllerName, true,
		string(gatewayv1.RouteReasonAccepted),
		"Route accepted")

	// Validate backend refs
	resolved, reason, message := r.validateBackendRefs(ctx, route)
	status.SetHTTPRouteResolvedRefs(route, parentRef, ControllerName, resolved,
		reason, message)

	// If backends not resolved due to BackendNotFound, requeue to retry
	// (Service may not exist yet due to race condition)
	if !resolved && reason == string(gatewayv1.RouteReasonBackendNotFound) {
		return fmt.Errorf("backend not found (will requeue): %s", message)
	}

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
		allowed, _, _ := r.isRouteAllowedByGateway(ctx, &route, gateway)
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

// isRouteAllowedByGateway checks if the route is allowed by any of the Gateway's listeners.
// A route is allowed if at least one listener:
// 1. Allows the route's namespace (per AllowedRoutes policy)
// 2. Has hostname intersection with the route (or either has no hostname)
// Returns (true, "", "") if any listener allows the route, or (false, reasonCode, message) if none do.
// The reasonCode distinguishes between NotAllowedByListeners and NoMatchingListenerHostname.
func (r *HTTPRouteReconciler) isRouteAllowedByGateway(ctx context.Context, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) (bool, string, string) {
	if len(gateway.Spec.Listeners) == 0 {
		return false, string(gatewayv1.RouteReasonNotAllowedByListeners), "Gateway has no listeners"
	}

	namespaceAllowed := false
	for _, listener := range gateway.Spec.Listeners {
		// Check namespace policy
		allowed, err := r.listenerAllowsRouteNamespace(ctx, route, gateway, listener)
		if err != nil {
			r.Logger.Error("failed to check listener namespace policy",
				"listener", listener.Name, "error", err)
			continue
		}
		if !allowed {
			continue
		}
		namespaceAllowed = true

		// Check hostname intersection
		if !hostnamesIntersect(route.Spec.Hostnames, listener.Hostname) {
			continue
		}

		return true, "", ""
	}

	if namespaceAllowed {
		// Namespace was allowed by at least one listener, but hostnames didn't intersect
		return false, string(gatewayv1.RouteReasonNoMatchingListenerHostname),
			fmt.Sprintf("No matching listener hostname on Gateway %s/%s", gateway.Namespace, gateway.Name)
	}

	return false, string(gatewayv1.RouteReasonNotAllowedByListeners),
		fmt.Sprintf("Route not allowed by any listener on Gateway %s/%s", gateway.Namespace, gateway.Name)
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

// effectiveHostnames computes the set of effective hostnames for a route against
// the gateway's listeners filtered by sectionName. This is the intersection of
// route hostnames with listener hostnames, producing the most specific hostname
// from each match pair.
// If sectionName is non-nil, only listeners matching that name are considered.
func effectiveHostnames(route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway, sectionName *gatewayv1.SectionName) []gatewayv1.Hostname {
	routeHostnames := route.Spec.Hostnames

	// Collect listener hostnames, filtered by sectionName if specified
	var listenerHostnames []*gatewayv1.Hostname
	for i := range gateway.Spec.Listeners {
		if sectionName != nil && string(gateway.Spec.Listeners[i].Name) != string(*sectionName) {
			continue
		}
		listenerHostnames = append(listenerHostnames, gateway.Spec.Listeners[i].Hostname)
	}

	// If route has no hostnames and any listener has no hostname, result is catch-all
	// If route has no hostnames, effective hostnames are the listener hostnames
	if len(routeHostnames) == 0 {
		seen := make(map[string]bool)
		var result []gatewayv1.Hostname
		for _, lh := range listenerHostnames {
			if lh == nil {
				// Catch-all: route with no hostnames + listener with no hostname
				if !seen["*"] {
					seen["*"] = true
					// Return empty to signal catch-all (CollectHTTPRouteBackends handles this)
					return nil
				}
			} else {
				h := string(*lh)
				if !seen[h] {
					seen[h] = true
					result = append(result, gatewayv1.Hostname(h))
				}
			}
		}
		return result
	}

	// Route has hostnames - intersect each with each listener hostname
	seen := make(map[string]bool)
	var result []gatewayv1.Hostname
	for _, rh := range routeHostnames {
		for _, lh := range listenerHostnames {
			if lh == nil {
				// Listener with no hostname matches everything - keep route hostname as-is
				h := string(rh)
				if !seen[h] {
					seen[h] = true
					result = append(result, rh)
				}
				continue
			}

			effective := computeEffectiveHostname(string(rh), string(*lh))
			if effective != "" && !seen[effective] {
				seen[effective] = true
				result = append(result, gatewayv1.Hostname(effective))
			}
		}
	}

	return result
}

// computeEffectiveHostname returns the most specific hostname from the intersection
// of a route hostname and listener hostname. Returns "" if they don't intersect.
func computeEffectiveHostname(routeHostname, listenerHostname string) string {
	// Exact match
	if routeHostname == listenerHostname {
		return routeHostname
	}

	// Listener is wildcard: *.example.com + foo.example.com → foo.example.com (more specific)
	// Also matches multi-level subdomains: *.example.com + foo.bar.example.com → foo.bar.example.com
	if strings.HasPrefix(listenerHostname, "*.") {
		suffix := listenerHostname[1:] // ".example.com"
		if strings.HasSuffix(routeHostname, suffix) {
			return routeHostname // route hostname is more specific
		}
	}

	// Route is wildcard: *.example.com + foo.example.com → foo.example.com (listener is more specific)
	// Also matches multi-level subdomains
	if strings.HasPrefix(routeHostname, "*.") {
		suffix := routeHostname[1:] // ".example.com"
		if strings.HasSuffix(listenerHostname, suffix) {
			return listenerHostname // listener hostname is more specific
		}
		// *.example.com + example.com → example.com
		if listenerHostname == routeHostname[2:] {
			return listenerHostname
		}
	}

	return ""
}

// filterRouteHostnames returns copies of routes with hostnames filtered to only
// those that intersect with the gateway's listeners. Each route's parentRefs are
// inspected to determine which listener(s) it targets via sectionName, so that
// a route targeting listener-1 only gets hostnames from that listener.
func filterRouteHostnames(routes []gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) []gatewayv1.HTTPRoute {
	filtered := make([]gatewayv1.HTTPRoute, 0, len(routes))
	for i := range routes {
		// Collect effective hostnames across all parentRefs targeting this gateway.
		// A route may have multiple parentRefs with different sectionNames.
		seen := make(map[string]bool)
		var allEffective []gatewayv1.Hostname
		isCatchAll := false

		for _, parentRef := range routes[i].Spec.ParentRefs {
			// Skip non-Gateway refs
			if parentRef.Kind != nil && *parentRef.Kind != "Gateway" {
				continue
			}
			// Check this parentRef targets the given gateway
			if string(parentRef.Name) != gateway.Name {
				continue
			}
			refNS := routes[i].Namespace
			if parentRef.Namespace != nil {
				refNS = string(*parentRef.Namespace)
			}
			if refNS != gateway.Namespace {
				continue
			}

			effective := effectiveHostnames(&routes[i], gateway, parentRef.SectionName)
			if effective == nil {
				// Catch-all from this parentRef
				isCatchAll = true
				break
			}
			for _, h := range effective {
				hs := string(h)
				if !seen[hs] {
					seen[hs] = true
					allEffective = append(allEffective, h)
				}
			}
		}

		if isCatchAll {
			filtered = append(filtered, routes[i])
			continue
		}
		if len(allEffective) == 0 {
			// No intersection - skip this route entirely
			continue
		}
		// Create a copy with only the intersecting hostnames
		routeCopy := routes[i].DeepCopy()
		routeCopy.Spec.Hostnames = allEffective
		filtered = append(filtered, *routeCopy)
	}
	return filtered
}

// updateConfigMap updates the Gateway's ConfigMap with routing.json (preserves main.vcl).
func (r *HTTPRouteReconciler) updateConfigMap(ctx context.Context, gateway *gatewayv1.Gateway, routes []gatewayv1.HTTPRoute) error {
	// Filter route hostnames to only those intersecting with gateway listeners
	filteredRoutes := filterRouteHostnames(routes, gateway)

	// Generate routing.json for ghost with path-based routing
	collectedRoutes := vcl.CollectHTTPRouteBackends(filteredRoutes, gateway.Namespace)

	// Group routes by hostname
	routesByHost := make(map[string][]ghost.Route)
	for _, route := range collectedRoutes {
		routesByHost[route.Hostname] = append(routesByHost[route.Hostname], route)
	}

	// Generate routing config
	routingConfig := ghost.GenerateRoutingConfig(routesByHost, nil)
	routingJSON, err := json.MarshalIndent(routingConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("json.MarshalIndent: %w", err)
	}

	// Fetch existing ConfigMap
	cmName := fmt.Sprintf("%s-vcl", gateway.Name)
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: gateway.Namespace}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			// ConfigMap not yet created by Gateway controller — will be requeued
			return fmt.Errorf("r.Get(%s): %w", cmName, err)
		}
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

// validateBackendRefs checks that all backend references in the route are valid.
// Returns (true, reason, message) if all refs are resolved, or (false, reason, message) if not.
func (r *HTTPRouteReconciler) validateBackendRefs(ctx context.Context, route *gatewayv1.HTTPRoute) (bool, string, string) {
	for _, rule := range route.Spec.Rules {
		for _, backendRef := range rule.BackendRefs {
			// Check Kind (nil defaults to Service)
			if backendRef.Kind != nil && *backendRef.Kind != "Service" {
				return false, string(gatewayv1.RouteReasonInvalidKind),
					fmt.Sprintf("BackendRef kind %q is not supported", *backendRef.Kind)
			}
			// Check Group (nil defaults to core)
			if backendRef.Group != nil && *backendRef.Group != "" {
				return false, string(gatewayv1.RouteReasonInvalidKind),
					fmt.Sprintf("BackendRef group %q is not supported", *backendRef.Group)
			}
			// Check Service exists
			namespace := route.Namespace
			if backendRef.Namespace != nil {
				namespace = string(*backendRef.Namespace)
			}
			var svc corev1.Service
			if err := r.Get(ctx, types.NamespacedName{
				Name:      string(backendRef.Name),
				Namespace: namespace,
			}, &svc); err != nil {
				if apierrors.IsNotFound(err) {
					return false, string(gatewayv1.RouteReasonBackendNotFound),
						fmt.Sprintf("Service %q not found in namespace %q", backendRef.Name, namespace)
				}
				// Transient error — report as not resolved
				return false, string(gatewayv1.RouteReasonBackendNotFound),
					fmt.Sprintf("Failed to get Service %q: %v", backendRef.Name, err)
			}
		}
	}
	return true, string(gatewayv1.RouteReasonResolvedRefs), "All references resolved"
}

// updateAttachedRoutesOnDeletion updates AttachedRoutes for all Gateways with our
// GatewayClass when a route is deleted. Since we don't have the deleted route's
// parentRefs, we update all managed Gateways.
func (r *HTTPRouteReconciler) updateAttachedRoutesOnDeletion(ctx context.Context, log *slog.Logger) {
	var gatewayList gatewayv1.GatewayList
	if err := r.List(ctx, &gatewayList); err != nil {
		log.Error("failed to list Gateways for deletion cleanup", "error", err)
		return
	}
	for i := range gatewayList.Items {
		gw := &gatewayList.Items[i]
		if string(gw.Spec.GatewayClassName) != r.Config.GatewayClassName {
			continue
		}
		r.updateAttachedRoutesForGateway(ctx, gw)
	}
}

// updateAttachedRoutesForGateway lists routes and updates AttachedRoutes for a Gateway.
// This is a convenience wrapper used in early-return paths where we have a valid Gateway
// but the current route is rejected (invalid sectionName, not allowed, etc.).
func (r *HTTPRouteReconciler) updateAttachedRoutesForGateway(ctx context.Context, gateway *gatewayv1.Gateway) {
	routes, err := r.listRoutesForGateway(ctx, gateway)
	if err != nil {
		r.Logger.Error("failed to list routes for AttachedRoutes update", "error", err)
		return
	}
	if err := r.updateGatewayListenerStatus(ctx, gateway, routes); err != nil {
		r.Logger.Error("failed to update Gateway listener status", "error", err)
	}
}

// updateGatewayListenerStatus updates AttachedRoutes count on Gateway listeners.
// Creates a minimal patch object to avoid conflicts with Gateway controller.
func (r *HTTPRouteReconciler) updateGatewayListenerStatus(ctx context.Context, gateway *gatewayv1.Gateway, routes []gatewayv1.HTTPRoute) error {
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

	// Build listener statuses with per-listener AttachedRoutes count.
	// Use Spec.Listeners (not Status.Listeners) so that when a listener is
	// removed from the spec, we stop claiming its fields via SSA. This allows
	// the removed listener to be cleaned up from the merged status.
	patch.Status.Listeners = make([]gatewayv1.ListenerStatus, len(gateway.Spec.Listeners))
	for i, listener := range gateway.Spec.Listeners {
		count := countRoutesForListener(routes, listener, gateway)
		patch.Status.Listeners[i] = gatewayv1.ListenerStatus{
			Name:           listener.Name,
			AttachedRoutes: count,
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

// countRoutesForListener counts how many routes attach to a specific listener.
// A route attaches to a listener if:
// 1. The route has no sectionName (attaches to all listeners), OR the route's sectionName matches the listener name
// 2. The route's hostnames intersect with the listener's hostname (or either is empty/unset)
func countRoutesForListener(routes []gatewayv1.HTTPRoute, listener gatewayv1.Listener, gateway *gatewayv1.Gateway) int32 {
	var count int32
	for _, route := range routes {
		if routeAttachesToListener(&route, listener, gateway) {
			count++
		}
	}
	return count
}

// routeAttachesToListener checks if a route attaches to a specific listener.
func routeAttachesToListener(route *gatewayv1.HTTPRoute, listener gatewayv1.Listener, gateway *gatewayv1.Gateway) bool {
	// Check if any parentRef targets this listener
	for _, parentRef := range route.Spec.ParentRefs {
		// Skip non-Gateway refs
		if parentRef.Kind != nil && *parentRef.Kind != "Gateway" {
			continue
		}
		if parentRef.Group != nil && *parentRef.Group != gatewayv1.Group(gatewayv1.GroupName) {
			continue
		}

		// Check name and namespace match the gateway
		if string(parentRef.Name) != gateway.Name {
			continue
		}
		refNS := route.Namespace
		if parentRef.Namespace != nil {
			refNS = string(*parentRef.Namespace)
		}
		if refNS != gateway.Namespace {
			continue
		}

		// Check sectionName: if specified, must match listener name
		if parentRef.SectionName != nil {
			if string(*parentRef.SectionName) != string(listener.Name) {
				continue
			}
		}

		// Check hostname intersection
		if hostnamesIntersect(route.Spec.Hostnames, listener.Hostname) {
			return true
		}
	}
	return false
}

// hostnamesIntersect checks if route hostnames intersect with a listener hostname.
// Per Gateway API spec:
// - If listener has no hostname → matches all route hostnames
// - If route has no hostnames → matches all listener hostnames
// - Otherwise → at least one route hostname must match the listener hostname
func hostnamesIntersect(routeHostnames []gatewayv1.Hostname, listenerHostname *gatewayv1.Hostname) bool {
	// Listener with no hostname matches everything
	if listenerHostname == nil {
		return true
	}

	// Route with no hostnames matches everything
	if len(routeHostnames) == 0 {
		return true
	}

	lh := string(*listenerHostname)
	for _, rh := range routeHostnames {
		if hostnameMatches(string(rh), lh) {
			return true
		}
	}
	return false
}

// hostnameMatches checks if a route hostname matches a listener hostname.
// Supports wildcard matching: *.example.com matches foo.example.com
func hostnameMatches(routeHostname, listenerHostname string) bool {
	// Exact match
	if routeHostname == listenerHostname {
		return true
	}

	// Listener wildcard: *.example.com matches foo.example.com and foo.bar.example.com
	if strings.HasPrefix(listenerHostname, "*.") {
		suffix := listenerHostname[1:] // ".example.com"
		if strings.HasSuffix(routeHostname, suffix) {
			return true
		}
	}

	// Route wildcard: *.example.com matches listener example.com or *.example.com
	if strings.HasPrefix(routeHostname, "*.") {
		suffix := routeHostname[1:] // ".example.com"
		if strings.HasSuffix(listenerHostname, suffix) {
			return true
		}
		// Route *.example.com also intersects with listener example.com
		if listenerHostname == routeHostname[2:] {
			return true
		}
	}

	return false
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
