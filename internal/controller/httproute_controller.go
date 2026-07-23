package controller

import (
	"context"
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
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

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
	log.Debug("reconciling HTTPRoute")

	// 1. Fetch the HTTPRoute
	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, req.NamespacedName, &route); err != nil {
		if apierrors.IsNotFound(err) {
			// Route was deleted. We can't read its parentRefs, so regenerate
			// routing.json for all our Gateways to remove any stale entries.
			// The Gateway controller's HTTPRoute watch separately handles
			// updating AttachedRoutes counts.
			if err := r.regenerateAllGateways(ctx); err != nil {
				log.Error("failed to regenerate routing after route deletion", "error", err)
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("r.Get(%s): %w", req.NamespacedName, err)
	}
	// Capture the Gateways this route was previously programmed onto (from its
	// existing status) BEFORE we mutate status below. Comparing against the
	// current parentRefs lets us regenerate routing.json for Gateways the route
	// has since detached from (edited parentRefs, moved A→B, or dropped to zero).
	previousGateways := gatewaysFromRouteStatus(&route)
	currentGateways := gatewaysFromRouteSpec(&route)

	// 2. Handle routes with no parentRefs.
	if len(route.Spec.ParentRefs) == 0 {
		log.Debug("HTTPRoute has no parentRefs")
		// The route detached from every parent. Regenerate any Gateways we
		// previously programmed it onto so its stale routing.json entries are
		// removed. currentGateways is empty, so all previous parents are detached.
		if err := r.regenerateDetachedGateways(ctx, previousGateways, currentGateways); err != nil {
			log.Error("failed to regenerate detached gateways", "error", err)
			return ctrl.Result{}, err
		}
		// Drop our now-stale status.Parents entries (currentGateways is empty).
		if len(previousGateways) > 0 {
			pruneDetachedRouteParents(&route, currentGateways)
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
		}
		return ctrl.Result{}, nil
	}

	// 3. Process each parentRef
	// Cache route lists per gateway to avoid redundant cluster-wide List calls
	// when multiple parentRefs target the same (or different) gateways.
	routeCache := make(map[types.NamespacedName][]gatewayv1.HTTPRoute)
	var processErr error
	for _, parentRef := range route.Spec.ParentRefs {
		if err := r.processParentRef(ctx, &route, parentRef, routeCache); err != nil {
			// Log but continue processing other parentRefs
			log.Error("failed to process parentRef",
				"parentRef", parentRef.Name,
				"error", err)
			processErr = err
		}
	}

	// 3b. Regenerate routing.json for Gateways the route has detached from
	// (present in the previous status but not in the current parentRefs), and
	// prune their stale status.Parents entries.
	if err := r.regenerateDetachedGateways(ctx, previousGateways, currentGateways); err != nil {
		log.Error("failed to regenerate detached gateways", "error", err)
		processErr = err
	}
	pruneDetachedRouteParents(&route, currentGateways)

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
// routeCache avoids redundant cluster-wide HTTPRoute List calls across parentRefs.
func (r *HTTPRouteReconciler) processParentRef(ctx context.Context, route *gatewayv1.HTTPRoute, parentRef gatewayv1.ParentReference, routeCache map[types.NamespacedName][]gatewayv1.HTTPRoute) error {
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

	// Check if Gateway uses a GatewayClass managed by our controller
	if !isOurGatewayClass(ctx, r.Client, string(gateway.Spec.GatewayClassName)) {
		log.Debug("Gateway uses GatewayClass not managed by us, skipping",
			"gatewayClass", gateway.Spec.GatewayClassName)
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
			return nil
		}
	}

	// Validate port references an actual listener
	if parentRef.Port != nil {
		found := false
		for _, listener := range gateway.Spec.Listeners {
			if listener.Port == gatewayv1.PortNumber(*parentRef.Port) {
				found = true
				break
			}
		}
		if !found {
			log.Info("parentRef port does not match any listener",
				"port", *parentRef.Port)
			status.SetHTTPRouteAccepted(route, parentRef, ControllerName, false,
				string(gatewayv1.RouteReasonNoMatchingParent),
				fmt.Sprintf("No listener with port %d on Gateway %s", *parentRef.Port, gateway.Name))
			status.SetHTTPRouteResolvedRefs(route, parentRef, ControllerName, true,
				string(gatewayv1.RouteReasonResolvedRefs),
				"References resolved")
			return nil
		}
	}

	// Check if the route's namespace is allowed by the Gateway's listeners
	allowed, reasonCode, reason := isRouteAllowedByGateway(ctx, r.Client, route, gateway)
	if !allowed {
		log.Info("route not allowed by Gateway listeners", "reasonCode", reasonCode, "reason", reason)
		status.SetHTTPRouteAccepted(route, parentRef, ControllerName, false,
			reasonCode, reason)
		status.SetHTTPRouteResolvedRefs(route, parentRef, ControllerName, true,
			string(gatewayv1.RouteReasonResolvedRefs), "All references resolved")
		return nil
	}

	// List all HTTPRoutes attached to this Gateway (cached per reconciliation)
	gwKey := types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace}
	routes, ok := routeCache[gwKey]
	if !ok {
		var err error
		routes, err = r.listRoutesForGateway(ctx, gateway)
		if err != nil {
			status.SetHTTPRouteAccepted(route, parentRef, ControllerName, false,
				string(gatewayv1.RouteReasonPending),
				fmt.Sprintf("Failed to list routes for Gateway: %v", err))
			status.SetHTTPRouteResolvedRefs(route, parentRef, ControllerName, true,
				string(gatewayv1.RouteReasonResolvedRefs),
				"References resolved")
			return fmt.Errorf("r.listRoutesForGateway: %w", err)
		}
		routeCache[gwKey] = routes
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
	return listAcceptedRoutesForGateway(ctx, r.Client, gateway)
}

// listAcceptedRoutesForGateway returns all HTTPRoutes that are attached to and accepted by a Gateway (package-level).
func listAcceptedRoutesForGateway(ctx context.Context, c client.Client, gateway *gatewayv1.Gateway) ([]gatewayv1.HTTPRoute, error) {
	var routeList gatewayv1.HTTPRouteList
	if err := c.List(ctx, &routeList); err != nil {
		return nil, fmt.Errorf("List(HTTPRouteList): %w", err)
	}

	var attached []gatewayv1.HTTPRoute
	for _, route := range routeList.Items {
		if !routeAttachedToGateway(&route, gateway) {
			continue
		}
		allowed, _, _ := isRouteAllowedByGateway(ctx, c, &route, gateway)
		if !allowed {
			continue
		}
		attached = append(attached, route)
	}

	return attached, nil
}

// parentRefTargetsGateway reports whether a single parentRef references the given
// Gateway (correct Kind/Group, name, and namespace). It does NOT apply
// sectionName/port or AllowedRoutes policy — that is layered on by callers.
func parentRefTargetsGateway(parentRef *gatewayv1.ParentReference, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) bool {
	// Skip non-Gateway refs
	if parentRef.Kind != nil && *parentRef.Kind != "Gateway" {
		return false
	}
	// Check group (default to gateway.networking.k8s.io)
	if parentRef.Group != nil && *parentRef.Group != gatewayv1.Group(gatewayv1.GroupName) {
		return false
	}
	// Check name
	if string(parentRef.Name) != gateway.Name {
		return false
	}
	// Check namespace (default to route's namespace)
	refNamespace := route.Namespace
	if parentRef.Namespace != nil {
		refNamespace = string(*parentRef.Namespace)
	}
	return refNamespace == gateway.Namespace
}

// routeAttachedToGateway checks if a route references the given Gateway (package-level).
func routeAttachedToGateway(route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) bool {
	for i := range route.Spec.ParentRefs {
		if parentRefTargetsGateway(&route.Spec.ParentRefs[i], route, gateway) {
			return true
		}
	}
	return false
}

// listenerAttachment records the outcome of computing which of a Gateway's
// listeners a route validly attaches to.
type listenerAttachment struct {
	// listenerNames is the deduplicated set of listener names the route attaches
	// to: for each parentRef, candidate listeners are restricted by sectionName/port,
	// then MUST individually satisfy the listener's AllowedRoutes namespace policy
	// AND have a hostname intersection with the route. This is the union of passing
	// (parentRef × listener) pairs.
	listenerNames []string
	// namespaceAllowed is true if at least one candidate listener allowed the
	// route's namespace (even if the hostname did not intersect). It distinguishes
	// NotAllowedByListeners (no listener allowed the namespace) from
	// NoMatchingListenerHostname (namespace allowed, hostname mismatch).
	namespaceAllowed bool
}

// computeListenerAttachment computes, per listener, whether a route validly
// attaches to a Gateway. Unlike a whole-Gateway "any listener allows it" check,
// this enforces the AllowedRoutes namespace policy AND hostname intersection on
// the SAME listener, restricted per parentRef by sectionName/port. The resulting
// listenerNames set is the authoritative attachment used both for the Accepted
// status and for the socket set programmed into routing.json — closing the
// per-listener namespace bypass where a foreign-namespace route could be served on
// a from:Same listener merely because some other listener allowed it.
func computeListenerAttachment(ctx context.Context, c client.Reader, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) listenerAttachment {
	var att listenerAttachment
	seen := make(map[string]bool)

	for i := range route.Spec.ParentRefs {
		parentRef := &route.Spec.ParentRefs[i]
		if !parentRefTargetsGateway(parentRef, route, gateway) {
			continue
		}

		for j := range gateway.Spec.Listeners {
			listener := &gateway.Spec.Listeners[j]

			// Restrict candidate listeners by this parentRef's sectionName/port.
			if parentRef.SectionName != nil && string(listener.Name) != string(*parentRef.SectionName) {
				continue
			}
			if parentRef.Port != nil && listener.Port != *parentRef.Port {
				continue
			}

			// Namespace policy MUST hold on this specific listener.
			allowed, err := listenerAllowsRouteNamespace(ctx, c, route, gateway, *listener)
			if err != nil || !allowed {
				continue
			}
			att.namespaceAllowed = true

			// Hostname intersection MUST hold on this same listener.
			if !hostnamesIntersect(route.Spec.Hostnames, listener.Hostname) {
				continue
			}

			if !seen[string(listener.Name)] {
				seen[string(listener.Name)] = true
				att.listenerNames = append(att.listenerNames, string(listener.Name))
			}
		}
	}

	return att
}

// isRouteAllowedByGateway checks if the route validly attaches to at least one of
// the Gateway's listeners (package-level). Per-listener, a route must satisfy BOTH
// the AllowedRoutes namespace policy AND a hostname intersection on the same
// listener. Returns (true, "", "") when at least one listener passes, or
// (false, reasonCode, message) otherwise. The reasonCode distinguishes between
// NotAllowedByListeners and NoMatchingListenerHostname.
func isRouteAllowedByGateway(ctx context.Context, c client.Reader, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) (bool, string, string) {
	if len(gateway.Spec.Listeners) == 0 {
		return false, string(gatewayv1.RouteReasonNotAllowedByListeners), "Gateway has no listeners"
	}

	att := computeListenerAttachment(ctx, c, route, gateway)
	if len(att.listenerNames) > 0 {
		return true, "", ""
	}

	if att.namespaceAllowed {
		// A listener allowed the namespace, but no allowed listener's hostname intersected.
		return false, string(gatewayv1.RouteReasonNoMatchingListenerHostname),
			fmt.Sprintf("No matching listener hostname on Gateway %s/%s", gateway.Namespace, gateway.Name)
	}

	return false, string(gatewayv1.RouteReasonNotAllowedByListeners),
		fmt.Sprintf("Route not allowed by any listener on Gateway %s/%s", gateway.Namespace, gateway.Name)
}

// listenerAllowsRouteNamespace checks if a specific listener allows routes from the route's namespace (package-level).
func listenerAllowsRouteNamespace(ctx context.Context, c client.Reader, route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway, listener gatewayv1.Listener) (bool, error) {
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
		if err := c.Get(ctx, types.NamespacedName{Name: route.Namespace}, &ns); err != nil {
			return false, fmt.Errorf("Get(Namespace %s): %w", route.Namespace, err)
		}
		return selector.Matches(labels.Set(ns.Labels)), nil

	default:
		return false, fmt.Errorf("unknown FromNamespaces value: %s", from)
	}
}

// effectiveHostnames computes the set of effective hostnames for a route against
// the gateway's listeners filtered by sectionName and/or port. This is the
// intersection of route hostnames with listener hostnames, producing the most
// specific hostname from each match pair.
//
// If sectionName is non-nil, only listeners matching that name are considered.
// If port is non-nil, only listeners matching that port are considered.
// If allowedNames is non-nil, only listeners whose name is in the set are
// considered — this restricts hostnames to listeners that actually passed the
// per-listener namespace policy, keeping the emitted hostname set consistent with
// the programmed socket set.
//
// The second return value reports whether the match is a genuine catch-all: a
// route with no hostnames attaching to a listener with no hostname. This is
// distinct from "no hostnames intersected" (empty slice, false) and from
// "sectionName/port/allowedNames matched no listener" (empty slice, false); those
// cases mean this parentRef contributes nothing, NOT that the route serves all
// hostnames.
func effectiveHostnames(route *gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway, sectionName *gatewayv1.SectionName, port *gatewayv1.PortNumber, allowedNames map[string]bool) ([]gatewayv1.Hostname, bool) {
	routeHostnames := route.Spec.Hostnames

	// Collect listener hostnames, filtered by sectionName, port, and allowedNames.
	var listenerHostnames []*gatewayv1.Hostname
	for i := range gateway.Spec.Listeners {
		l := &gateway.Spec.Listeners[i]
		if sectionName != nil && string(l.Name) != string(*sectionName) {
			continue
		}
		if port != nil && l.Port != *port {
			continue
		}
		if allowedNames != nil && !allowedNames[string(l.Name)] {
			continue
		}
		listenerHostnames = append(listenerHostnames, l.Hostname)
	}

	// Route with no hostnames: effective hostnames are the listener hostnames,
	// unless a candidate listener also has no hostname — then it is a genuine
	// catch-all and the route serves all hostnames.
	if len(routeHostnames) == 0 {
		seen := make(map[string]bool)
		var result []gatewayv1.Hostname
		for _, lh := range listenerHostnames {
			if lh == nil {
				// Genuine catch-all: route with no hostnames + listener with no hostname.
				return nil, true
			}
			h := string(*lh)
			if !seen[h] {
				seen[h] = true
				result = append(result, gatewayv1.Hostname(h))
			}
		}
		return result, false
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

	return result, false
}

// computeEffectiveHostname returns the most specific hostname from the intersection
// of a route hostname and listener hostname. Returns "" if they don't intersect.
// wildcardCovers reports whether a hostname pattern covers a candidate hostname.
// A pattern of the form "*.example.com" covers any single- or multi-level
// subdomain (foo.example.com, foo.bar.example.com) but NOT the apex
// (example.com). A non-wildcard pattern covers only an exact match.
//
// This is the shared primitive behind computeEffectiveHostname, hostnameMatches,
// and listenerCoversHostname. Note it deliberately does not treat "*.example.com"
// as covering "example.com" — callers that need that apex behavior (the symmetric
// route/listener intersection checks) handle it explicitly.
func wildcardCovers(pattern, hostname string) bool {
	if pattern == hostname {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		return strings.HasSuffix(hostname, pattern[1:]) // pattern[1:] == ".example.com"
	}
	return false
}

func computeEffectiveHostname(routeHostname, listenerHostname string) string {
	// Exact match, or listener wildcard covering the route: the route hostname
	// is the more specific of the two (*.example.com + foo.example.com → foo.example.com).
	if wildcardCovers(listenerHostname, routeHostname) {
		return routeHostname
	}

	// Route wildcard covering the listener: the listener hostname is more specific
	// (*.example.com + foo.example.com → foo.example.com).
	if wildcardCovers(routeHostname, listenerHostname) {
		return listenerHostname
	}

	// A route wildcard also intersects the apex it derives from, and the apex
	// (the listener) is the more specific hostname: *.example.com + example.com → example.com.
	if strings.HasPrefix(routeHostname, "*.") && listenerHostname == routeHostname[2:] {
		return listenerHostname
	}

	return ""
}

// filterRouteHostnames returns copies of routes with hostnames filtered to only
// those that intersect with the gateway's listeners the route validly attaches to.
// Each route's parentRefs are inspected to determine which listener(s) it targets
// via sectionName/port, and attachments restricts consideration to listeners that
// passed the per-listener namespace policy — so a route only gets hostnames from
// listeners it is actually permitted (and programmed) to serve on.
//
// attachments maps a route key ("namespace/name") to its computed listener
// attachment. A route whose key is absent falls back to no allowed-name filtering.
func filterRouteHostnames(routes []gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway, attachments map[string]listenerAttachment) []gatewayv1.HTTPRoute {
	filtered := make([]gatewayv1.HTTPRoute, 0, len(routes))
	for i := range routes {
		key := routes[i].Namespace + "/" + routes[i].Name

		// Restrict to listeners that passed the per-listener namespace policy.
		var allowedNames map[string]bool
		if att, ok := attachments[key]; ok {
			allowedNames = make(map[string]bool, len(att.listenerNames))
			for _, n := range att.listenerNames {
				allowedNames[n] = true
			}
		}

		// Collect effective hostnames across all parentRefs targeting this gateway.
		// A route may have multiple parentRefs with different sectionNames.
		seen := make(map[string]bool)
		var allEffective []gatewayv1.Hostname
		isCatchAll := false

		for j := range routes[i].Spec.ParentRefs {
			parentRef := &routes[i].Spec.ParentRefs[j]
			if !parentRefTargetsGateway(parentRef, &routes[i], gateway) {
				continue
			}

			effective, catchAll := effectiveHostnames(&routes[i], gateway, parentRef.SectionName, parentRef.Port, allowedNames)
			if catchAll {
				// Genuine catch-all: the route serves all hostnames. This is a
				// superset of anything other parentRefs could contribute.
				isCatchAll = true
				break
			}
			if len(effective) == 0 {
				// No hostname intersection from this parentRef (or it matched no
				// permitted listener). It contributes nothing — skip it, but do NOT
				// discard hostnames contributed by other parentRefs.
				continue
			}
			// Apply listener isolation: remove hostnames claimed by more specific listeners
			if parentRef.SectionName != nil {
				effective = filterForListenerIsolation(effective, *parentRef.SectionName, gateway)
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
			// No intersection from any parentRef and not a genuine catch-all -
			// skip this route entirely rather than programming its full hostname set.
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
	// Compute the authoritative per-listener attachment for each route once. This
	// enforces the AllowedRoutes namespace policy AND hostname intersection on the
	// same listener, and drives both the emitted hostname set and the socket set —
	// so a route is only ever programmed onto listeners it is truly permitted on.
	attachments := make(map[string]listenerAttachment, len(routes))
	routeListeners := make(map[string][]string, len(routes))
	for i := range routes {
		key := routes[i].Namespace + "/" + routes[i].Name
		att := computeListenerAttachment(ctx, r.Client, &routes[i], gateway)
		attachments[key] = att
		routeListeners[key] = att.listenerNames
	}

	// Filter route hostnames to only those intersecting with the listeners the
	// route validly attaches to.
	filteredRoutes := filterRouteHostnames(routes, gateway, attachments)

	// Build port map to resolve service ports to target ports
	portMap := r.buildServicePortMap(ctx, filteredRoutes, gateway.Namespace)

	// Compute blocked cross-namespace backend refs (not permitted by ReferenceGrants)
	blockedRefs := r.computeBlockedBackendRefs(ctx, filteredRoutes)

	// Generate routing.json for ghost with path-based routing. routeListeners
	// supplies the authoritative socket set per route (respecting namespace policy).
	collectedRoutes := vcl.CollectHTTPRouteBackends(filteredRoutes, gateway, gateway.Namespace, portMap, blockedRefs, routeListeners)

	// Attach VarnishCachePolicy to each route
	r.attachCachePolicies(ctx, collectedRoutes, filteredRoutes, gateway)

	// Attach BackendTLSPolicy to each route
	r.attachBackendTLS(ctx, collectedRoutes)

	// Attach ExternalProxy hints for routes whose Service is type ExternalName
	r.attachExternalProxies(ctx, collectedRoutes)

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

	newRoutingJSON := string(routingJSON)

	// Update ConfigMap data (only routing.json, preserve main.vcl owned by Gateway controller)
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}

	// Avoid unnecessary writes/reconciles when content is unchanged.
	if cm.Data["routing.json"] == newRoutingJSON {
		r.Logger.Debug("reconciled ConfigMap (no content change)",
			"configmap", cmName,
			"routes", len(routes),
			"backends", len(collectedRoutes))
		return nil
	}
	cm.Data["routing.json"] = newRoutingJSON

	if err := r.Update(ctx, &cm); err != nil {
		return fmt.Errorf("r.Update(%s): %w", cmName, err)
	}
	r.Logger.Info("updated ConfigMap",
		"configmap", cmName,
		"routes", len(routes),
		"backends", len(collectedRoutes))

	return nil
}

// attachCachePolicies resolves VarnishCachePolicies for each collected route.
// It looks up VCPs targeting the route's parent HTTPRoute, specific rules within it,
// and the parent Gateway. Most specific VCP wins (rule > route > gateway).
func (r *HTTPRouteReconciler) attachCachePolicies(ctx context.Context, collectedRoutes []ghost.Route, httpRoutes []gatewayv1.HTTPRoute, gateway *gatewayv1.Gateway) {
	// Build lookup: routeName (namespace/name) → HTTPRoute
	routeMap := make(map[string]*gatewayv1.HTTPRoute)
	for i := range httpRoutes {
		rt := &httpRoutes[i]
		ns := rt.Namespace
		if ns == "" {
			ns = gateway.Namespace
		}
		routeMap[ns+"/"+rt.Name] = rt
	}

	for i := range collectedRoutes {
		cr := &collectedRoutes[i]
		httpRoute := routeMap[cr.RouteName]
		if httpRoute == nil {
			continue
		}
		cp := ResolveCachePolicyForRoute(ctx, r.Client, httpRoute, gateway, cr.RuleName)
		if cp != nil {
			cr.CachePolicy = cp
		}
	}
}

// attachBackendTLS resolves BackendTLSPolicies for each collected route.
// It looks up policies targeting the route's backend Service and attaches TLS config.
func (r *HTTPRouteReconciler) attachBackendTLS(ctx context.Context, collectedRoutes []ghost.Route) {
	// Collect unique namespaces from routes
	namespaces := make(map[string]struct{})
	for _, route := range collectedRoutes {
		namespaces[route.Namespace] = struct{}{}
	}

	// Build lookup: "namespace/serviceName" → BackendTLSPolicy
	// Only include policies that have valid CA cert configuration (either
	// wellKnownCACertificates: System or valid caCertificateRefs).
	policyMap := make(map[string]*gatewayv1.BackendTLSPolicy)
	for ns := range namespaces {
		var policyList gatewayv1.BackendTLSPolicyList
		if err := r.List(ctx, &policyList, client.InNamespace(ns)); err != nil {
			r.Logger.Error("failed to list BackendTLSPolicies", "namespace", ns, "error", err)
			continue
		}
		for i := range policyList.Items {
			policy := &policyList.Items[i]

			// Accept policies with wellKnownCACertificates: System or caCertificateRefs
			hasWellKnown := policy.Spec.Validation.WellKnownCACertificates != nil &&
				*policy.Spec.Validation.WellKnownCACertificates == gatewayv1.WellKnownCACertificatesSystem
			hasCertRefs := len(policy.Spec.Validation.CACertificateRefs) > 0

			if !hasWellKnown && !hasCertRefs {
				r.Logger.Warn("BackendTLSPolicy has no CA certificate configuration, skipping",
					"policy", fmt.Sprintf("%s/%s", policy.Namespace, policy.Name))
				continue
			}

			for _, targetRef := range policy.Spec.TargetRefs {
				// Only handle Service targets
				if targetRef.Group != "" || targetRef.Kind != "Service" {
					continue
				}
				key := ns + "/" + string(targetRef.Name)
				policyMap[key] = policy
			}
		}
	}

	// Attach TLS config to routes whose backends match a policy
	for i := range collectedRoutes {
		cr := &collectedRoutes[i]
		key := cr.Namespace + "/" + cr.Service
		policy, ok := policyMap[key]
		if !ok {
			continue
		}
		cr.BackendTLS = &ghost.BackendTLS{
			Hostname: string(policy.Spec.Validation.Hostname),
		}
	}
}

// attachExternalProxies resolves routes whose backend Service is of type
// ExternalName and attaches an ExternalProxy hint. The ghost VMOD uses this
// to drive its internal HTTP client instead of looking up native backends.
// TLS is inferred from the Service port's appProtocol == "https"; a
// BackendTLSPolicy attached via attachBackendTLS will still apply (its
// hostname overrides SNI in the synthetic backend).
func (r *HTTPRouteReconciler) attachExternalProxies(ctx context.Context, collectedRoutes []ghost.Route) {
	type svcInfo struct {
		externalName string
		ports        []corev1.ServicePort
	}

	cache := make(map[types.NamespacedName]*svcInfo)
	for i := range collectedRoutes {
		cr := &collectedRoutes[i]
		if cr.Service == "" {
			continue
		}
		key := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Service}
		info, cached := cache[key]
		if !cached {
			var svc corev1.Service
			if err := r.Get(ctx, key, &svc); err == nil &&
				svc.Spec.Type == corev1.ServiceTypeExternalName &&
				svc.Spec.ExternalName != "" {
				info = &svcInfo{
					externalName: svc.Spec.ExternalName,
					ports:        svc.Spec.Ports,
				}
			}
			cache[key] = info
		}
		if info == nil {
			continue
		}

		// For ExternalName services buildServicePortMap skips translation, so
		// cr.Port equals the BackendRef.port (the Service port the user wrote).
		cr.ExternalProxy = &ghost.ExternalProxy{
			Hostname: info.externalName,
			Port:     cr.Port,
			TLS:      portUsesHTTPS(info.ports, cr.Port),
		}
	}
}

// portUsesHTTPS reports whether the named Service port has appProtocol=https.
func portUsesHTTPS(ports []corev1.ServicePort, port int) bool {
	for _, sp := range ports {
		if int(sp.Port) == port {
			return sp.AppProtocol != nil && strings.EqualFold(*sp.AppProtocol, "https")
		}
	}
	return false
}

// findHTTPRoutesForBackendTLSPolicy returns reconcile requests for HTTPRoutes
// whose backend Services are targeted by the given BackendTLSPolicy.
func (r *HTTPRouteReconciler) findHTTPRoutesForBackendTLSPolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	policy, ok := obj.(*gatewayv1.BackendTLSPolicy)
	if !ok {
		return nil
	}

	serviceNames := serviceNamesFromPolicy(policy)
	if len(serviceNames) == 0 {
		return nil
	}

	// Find HTTPRoutes referencing any of these services
	var routeList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routeList); err != nil {
		r.Logger.Error("failed to list HTTPRoutes for BackendTLSPolicy watch", "error", err)
		return nil
	}

	var requests []reconcile.Request
	for _, route := range routeList.Items {
		for svcName := range serviceNames {
			if routeReferencesService(&route, svcName, policy.Namespace) {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      route.Name,
						Namespace: route.Namespace,
					},
				})
				break // avoid duplicate requests for the same route
			}
		}
	}

	if len(requests) > 0 {
		r.Logger.Debug("BackendTLSPolicy changed, re-reconciling referencing HTTPRoutes",
			"policy", fmt.Sprintf("%s/%s", policy.Namespace, policy.Name),
			"routes", len(requests))
	}

	return requests
}

// buildServicePortMap iterates all BackendRefs across routes, fetches each Service,
// and maps {namespace/service:servicePort → targetPort}. This resolves the domain
// mismatch between HTTPRoute BackendRef ports (service ports) and EndpointSlice
// ports (target/container ports).
func (r *HTTPRouteReconciler) buildServicePortMap(ctx context.Context, routes []gatewayv1.HTTPRoute, defaultNS string) vcl.ServicePortMap {
	portMap := make(vcl.ServicePortMap)
	for _, route := range routes {
		routeNS := route.Namespace
		if routeNS == "" {
			routeNS = defaultNS
		}
		for _, rule := range route.Spec.Rules {
			for _, backend := range rule.BackendRefs {
				// Skip non-Service backends
				if backend.Kind != nil && *backend.Kind != "Service" {
					continue
				}
				if backend.Group != nil && *backend.Group != "" {
					continue
				}
				if backend.Name == "" {
					continue
				}

				backendNS := routeNS
				if backend.Namespace != nil {
					backendNS = string(*backend.Namespace)
				}

				servicePort := 80
				if backend.Port != nil {
					servicePort = int(*backend.Port)
				}

				key := fmt.Sprintf("%s/%s:%d", backendNS, backend.Name, servicePort)
				if _, exists := portMap[key]; exists {
					continue // already resolved
				}

				// Fetch the Service to find the targetPort
				var svc corev1.Service
				if err := r.Get(ctx, types.NamespacedName{
					Name:      string(backend.Name),
					Namespace: backendNS,
				}, &svc); err != nil {
					r.Logger.Debug("failed to fetch Service for port resolution",
						"service", fmt.Sprintf("%s/%s", backendNS, backend.Name),
						"error", err)
					continue // leave unmapped, falls through to service port
				}

				// ExternalName services have no underlying pods, so targetPort
				// translation doesn't apply — the ghost VMOD's external proxy
				// connects directly to the externalName on the BackendRef port.
				if svc.Spec.Type == corev1.ServiceTypeExternalName {
					continue
				}

				// Find matching port in Service spec
				for _, sp := range svc.Spec.Ports {
					if int(sp.Port) != servicePort {
						continue
					}
					if sp.TargetPort.Type == intstr.Int {
						if sp.TargetPort.IntVal != 0 {
							portMap[key] = vcl.ServicePortMapping{Port: int(sp.TargetPort.IntVal)}
						}
						// IntVal == 0 means targetPort defaults to servicePort (no mapping needed)
					} else {
						// Named port: store the service port name so the chaperone can
						// filter EndpointSlice ports by name instead of number.
						portMap[key] = vcl.ServicePortMapping{Port: 0, Name: sp.Name}
					}
					break
				}
			}
		}
	}
	return portMap
}

// gatewaysFromRouteSpec returns the set of Gateways (our group) currently
// referenced by the route's parentRefs, keyed by NamespacedName.
func gatewaysFromRouteSpec(route *gatewayv1.HTTPRoute) map[types.NamespacedName]bool {
	result := make(map[types.NamespacedName]bool)
	for i := range route.Spec.ParentRefs {
		pr := &route.Spec.ParentRefs[i]
		if pr.Kind != nil && *pr.Kind != "Gateway" {
			continue
		}
		if pr.Group != nil && *pr.Group != gatewayv1.Group(gatewayv1.GroupName) {
			continue
		}
		ns := route.Namespace
		if pr.Namespace != nil {
			ns = string(*pr.Namespace)
		}
		result[types.NamespacedName{Name: string(pr.Name), Namespace: ns}] = true
	}
	return result
}

// gatewaysFromRouteStatus returns the set of Gateways this controller previously
// programmed the route onto, derived from the route's existing status.Parents
// entries owned by our ControllerName. Used to detect parents the route has since
// detached from.
func gatewaysFromRouteStatus(route *gatewayv1.HTTPRoute) map[types.NamespacedName]bool {
	result := make(map[types.NamespacedName]bool)
	for _, ps := range route.Status.Parents {
		if string(ps.ControllerName) != ControllerName {
			continue
		}
		pr := ps.ParentRef
		if pr.Kind != nil && *pr.Kind != "Gateway" {
			continue
		}
		if pr.Group != nil && *pr.Group != gatewayv1.Group(gatewayv1.GroupName) {
			continue
		}
		ns := route.Namespace
		if pr.Namespace != nil {
			ns = string(*pr.Namespace)
		}
		result[types.NamespacedName{Name: string(pr.Name), Namespace: ns}] = true
	}
	return result
}

// pruneDetachedRouteParents removes RouteParentStatus entries owned by our
// controller whose Gateway is no longer referenced by the route's spec (the
// `current` set). Entries owned by other controllers, and entries for still-current
// Gateways, are preserved.
func pruneDetachedRouteParents(route *gatewayv1.HTTPRoute, current map[types.NamespacedName]bool) {
	kept := make([]gatewayv1.RouteParentStatus, 0, len(route.Status.Parents))
	for _, ps := range route.Status.Parents {
		if string(ps.ControllerName) == ControllerName {
			pr := ps.ParentRef
			isGateway := pr.Kind == nil || *pr.Kind == "Gateway"
			isOurGroup := pr.Group == nil || *pr.Group == gatewayv1.Group(gatewayv1.GroupName)
			if isGateway && isOurGroup {
				ns := route.Namespace
				if pr.Namespace != nil {
					ns = string(*pr.Namespace)
				}
				nn := types.NamespacedName{Name: string(pr.Name), Namespace: ns}
				if !current[nn] {
					continue // drop stale entry for a detached Gateway
				}
			}
		}
		kept = append(kept, ps)
	}
	route.Status.Parents = kept
}

// regenerateDetachedGateways regenerates routing.json for every Gateway present in
// `previous` but absent from `current` — i.e. Gateways the route has detached from.
// This removes the route's stale routing.json entries when parentRefs are edited,
// the route is moved between Gateways, or its parentRefs are dropped to zero.
// Deleted Gateways and Gateways not managed by our GatewayClass are skipped.
func (r *HTTPRouteReconciler) regenerateDetachedGateways(ctx context.Context, previous, current map[types.NamespacedName]bool) error {
	for nn := range previous {
		if current[nn] {
			continue
		}
		var gw gatewayv1.Gateway
		if err := r.Get(ctx, nn, &gw); err != nil {
			if apierrors.IsNotFound(err) {
				continue // Gateway gone; its ConfigMap went with it
			}
			return fmt.Errorf("r.Get(%s): %w", nn, err)
		}
		if !isOurGatewayClass(ctx, r.Client, string(gw.Spec.GatewayClassName)) {
			continue
		}
		routes, err := r.listRoutesForGateway(ctx, &gw)
		if err != nil {
			return fmt.Errorf("r.listRoutesForGateway(%s): %w", nn, err)
		}
		if err := r.updateConfigMap(ctx, &gw, routes); err != nil {
			return fmt.Errorf("r.updateConfigMap(%s): %w", nn, err)
		}
		r.Logger.Info("regenerated routing.json for detached gateway", "gateway", nn.String())
	}
	return nil
}

// regenerateAllGateways lists all Gateways managed by our GatewayClass and
// regenerates routing.json for each. This is called when an HTTPRoute is deleted
// and we can no longer read its parentRefs to know which Gateway was affected.
// The no-op check in updateConfigMap avoids unnecessary writes for unaffected Gateways.
func (r *HTTPRouteReconciler) regenerateAllGateways(ctx context.Context) error {
	var gatewayList gatewayv1.GatewayList
	if err := r.List(ctx, &gatewayList); err != nil {
		return fmt.Errorf("List(GatewayList): %w", err)
	}

	for i := range gatewayList.Items {
		gw := &gatewayList.Items[i]
		if !isOurGatewayClass(ctx, r.Client, string(gw.Spec.GatewayClassName)) {
			continue
		}
		routes, err := r.listRoutesForGateway(ctx, gw)
		if err != nil {
			return fmt.Errorf("listRoutesForGateway(%s/%s): %w", gw.Namespace, gw.Name, err)
		}
		if err := r.updateConfigMap(ctx, gw, routes); err != nil {
			return fmt.Errorf("updateConfigMap(%s/%s): %w", gw.Namespace, gw.Name, err)
		}
	}
	return nil
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
			// Check cross-namespace ReferenceGrant
			namespace := route.Namespace
			if backendRef.Namespace != nil {
				namespace = string(*backendRef.Namespace)
			}
			if namespace != route.Namespace {
				allowed, err := IsReferenceAllowed(ctx, r.Client, httpRouteServiceRef(route.Namespace, namespace, string(backendRef.Name)))
				if err != nil {
					return false, string(gatewayv1.RouteReasonRefNotPermitted),
						fmt.Sprintf("Failed to check ReferenceGrant for Service %q in namespace %q: %v", backendRef.Name, namespace, err)
				}
				if !allowed {
					return false, string(gatewayv1.RouteReasonRefNotPermitted),
						fmt.Sprintf("Cross-namespace reference to Service %q in namespace %q not permitted by any ReferenceGrant", backendRef.Name, namespace)
				}
			}

			// Check Service exists
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

// httpRouteServiceRef builds a CrossNamespaceRef for an HTTPRoute referencing a Service.
func httpRouteServiceRef(routeNamespace, targetNamespace, serviceName string) CrossNamespaceRef {
	return CrossNamespaceRef{
		FromGroup:     "gateway.networking.k8s.io",
		FromKind:      "HTTPRoute",
		FromNamespace: routeNamespace,
		ToGroup:       "",
		ToKind:        "Service",
		ToNamespace:   targetNamespace,
		ToName:        serviceName,
	}
}

// computeBlockedBackendRefs returns the set of cross-namespace backend refs
// that are not permitted by any ReferenceGrant.
func (r *HTTPRouteReconciler) computeBlockedBackendRefs(ctx context.Context, routes []gatewayv1.HTTPRoute) map[string]bool {
	blocked := make(map[string]bool)
	for _, route := range routes {
		routeNS := route.Namespace
		for _, rule := range route.Spec.Rules {
			for _, backendRef := range rule.BackendRefs {
				ns := routeNS
				if backendRef.Namespace != nil {
					ns = string(*backendRef.Namespace)
				}
				if ns == routeNS {
					continue
				}
				allowed, err := IsReferenceAllowed(ctx, r.Client, httpRouteServiceRef(routeNS, ns, string(backendRef.Name)))
				if err != nil {
					r.Logger.Error("failed to check ReferenceGrant", "error", err,
						"route", routeNS+"/"+route.Name, "service", ns+"/"+string(backendRef.Name))
				}
				if !allowed {
					blocked[vcl.BlockedBackendKey(routeNS, ns, string(backendRef.Name))] = true
				}
			}
		}
	}
	return blocked
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

		// Check port: if specified, must match listener port
		if parentRef.Port != nil {
			if gatewayv1.PortNumber(*parentRef.Port) != listener.Port {
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

// hostnameMatches checks if a route hostname intersects a listener hostname.
// Either side may be a wildcard; they intersect if one covers the other, plus
// the apex special case where *.example.com intersects example.com.
func hostnameMatches(routeHostname, listenerHostname string) bool {
	if wildcardCovers(listenerHostname, routeHostname) || wildcardCovers(routeHostname, listenerHostname) {
		return true
	}
	// Route *.example.com also intersects listener example.com.
	return strings.HasPrefix(routeHostname, "*.") && listenerHostname == routeHostname[2:]
}

// filterForListenerIsolation removes effective hostnames that are "claimed" by a
// more specific listener on the same Gateway. Per Gateway API spec, when multiple
// listeners could match a hostname, the most specific listener wins and routes
// attached to less-specific listeners should not be accessible for those hostnames.
func filterForListenerIsolation(hostnames []gatewayv1.Hostname, sectionName gatewayv1.SectionName, gateway *gatewayv1.Gateway) []gatewayv1.Hostname {
	var targetListener *gatewayv1.Listener
	for i := range gateway.Spec.Listeners {
		if string(gateway.Spec.Listeners[i].Name) == string(sectionName) {
			targetListener = &gateway.Spec.Listeners[i]
			break
		}
	}
	if targetListener == nil {
		return hostnames
	}

	targetSpec := listenerHostnameSpecificity(targetListener)

	var filtered []gatewayv1.Hostname
	for _, h := range hostnames {
		claimed := false
		hs := string(h)
		for i := range gateway.Spec.Listeners {
			l := &gateway.Spec.Listeners[i]
			if string(l.Name) == string(sectionName) {
				continue
			}
			if listenerHostnameSpecificity(l) <= targetSpec {
				continue
			}
			if listenerCoversHostname(l, hs) {
				claimed = true
				break
			}
		}
		if !claimed {
			filtered = append(filtered, h)
		}
	}
	return filtered
}

// listenerHostnameSpecificity returns a numeric specificity for a listener's hostname.
// Exact hostnames > wildcards > no hostname.
func listenerHostnameSpecificity(l *gatewayv1.Listener) int {
	if l.Hostname == nil {
		return 0
	}
	h := string(*l.Hostname)
	if strings.HasPrefix(h, "*.") {
		return len(h)
	}
	return len(h) + 10000
}

// listenerCoversHostname checks if a listener's hostname space fully covers the
// given (already-resolved effective) hostname. This is directional — only the
// listener may be a wildcard — and deliberately omits the *.example.com/example.com
// apex case, since the argument is a concrete effective hostname, not a route pattern.
func listenerCoversHostname(l *gatewayv1.Listener, hostname string) bool {
	if l.Hostname == nil {
		return true
	}
	return wildcardCovers(string(*l.Hostname), hostname)
}

// findHTTPRoutesForGateway returns reconcile requests for all HTTPRoutes
// attached to a Gateway when the Gateway changes.
func (r *HTTPRouteReconciler) findHTTPRoutesForGateway(ctx context.Context, obj client.Object) []reconcile.Request {
	gateway, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}

	// Skip Gateways not managed by our controller
	if !isOurGatewayClass(ctx, r.Client, string(gateway.Spec.GatewayClassName)) {
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
		if routeAttachedToGateway(&route, gateway) {
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

// findHTTPRoutesForService returns reconcile requests for all HTTPRoutes
// that reference the changed Service as a backend.
func (r *HTTPRouteReconciler) findHTTPRoutesForService(ctx context.Context, obj client.Object) []reconcile.Request {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return nil
	}

	// List all HTTPRoutes
	var routeList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routeList); err != nil {
		r.Logger.Error("failed to list HTTPRoutes for Service watch", "error", err)
		return nil
	}

	var requests []reconcile.Request
	for _, route := range routeList.Items {
		if routeReferencesService(&route, svc.Name, svc.Namespace) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      route.Name,
					Namespace: route.Namespace,
				},
			})
		}
	}

	if len(requests) > 0 {
		r.Logger.Debug("Service changed, re-reconciling referencing HTTPRoutes",
			"service", fmt.Sprintf("%s/%s", svc.Namespace, svc.Name),
			"routes", len(requests))
	}

	return requests
}

// routeReferencesService checks if any backendRef in the route references the given Service.
func routeReferencesService(route *gatewayv1.HTTPRoute, serviceName, serviceNamespace string) bool {
	for _, rule := range route.Spec.Rules {
		for _, backendRef := range rule.BackendRefs {
			// Skip non-Service refs
			if backendRef.Kind != nil && *backendRef.Kind != "Service" {
				continue
			}
			if backendRef.Group != nil && *backendRef.Group != "" {
				continue
			}
			if string(backendRef.Name) != serviceName {
				continue
			}
			refNS := route.Namespace
			if backendRef.Namespace != nil {
				refNS = string(*backendRef.Namespace)
			}
			if refNS == serviceNamespace {
				return true
			}
		}
	}
	return false
}

// findHTTPRoutesForReferenceGrant returns reconcile requests for all HTTPRoutes
// that have cross-namespace backend refs into the ReferenceGrant's namespace.
func (r *HTTPRouteReconciler) findHTTPRoutesForReferenceGrant(ctx context.Context, obj client.Object) []reconcile.Request {
	grant, ok := obj.(*gatewayv1beta1.ReferenceGrant)
	if !ok {
		return nil
	}

	var routeList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routeList); err != nil {
		r.Logger.Error("failed to list HTTPRoutes for ReferenceGrant watch", "error", err)
		return nil
	}

	var requests []reconcile.Request
	for _, route := range routeList.Items {
		if routeHasCrossNamespaceRefTo(&route, grant.Namespace) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      route.Name,
					Namespace: route.Namespace,
				},
			})
		}
	}

	if len(requests) > 0 {
		r.Logger.Debug("ReferenceGrant changed, re-reconciling affected HTTPRoutes",
			"grant", fmt.Sprintf("%s/%s", grant.Namespace, grant.Name),
			"routes", len(requests))
	}

	return requests
}

// routeHasCrossNamespaceRefTo checks if any backendRef in the route references
// a Service in the given namespace from a different namespace.
func routeHasCrossNamespaceRefTo(route *gatewayv1.HTTPRoute, targetNamespace string) bool {
	for _, rule := range route.Spec.Rules {
		for _, backendRef := range rule.BackendRefs {
			if backendRef.Namespace == nil {
				continue
			}
			if string(*backendRef.Namespace) == targetNamespace && route.Namespace != targetNamespace {
				return true
			}
		}
	}
	return false
}

// findHTTPRoutesForVCP returns reconcile requests for HTTPRoutes affected by a VCP change.
func (r *HTTPRouteReconciler) findHTTPRoutesForVCP(ctx context.Context, obj client.Object) []reconcile.Request {
	vcp, ok := obj.(*gatewayparamsv1alpha1.VarnishCachePolicy)
	if !ok {
		return nil
	}

	targetKind := string(vcp.Spec.TargetRef.Kind)
	targetName := string(vcp.Spec.TargetRef.Name)

	switch targetKind {
	case "HTTPRoute":
		return []reconcile.Request{{
			NamespacedName: types.NamespacedName{
				Name:      targetName,
				Namespace: vcp.Namespace,
			},
		}}

	case "Gateway":
		var gw gatewayv1.Gateway
		if err := r.Get(ctx, types.NamespacedName{Name: targetName, Namespace: vcp.Namespace}, &gw); err != nil {
			return nil
		}
		routes, err := listAcceptedRoutesForGateway(ctx, r.Client, &gw)
		if err != nil {
			r.Logger.Error("failed to list routes for gateway VCP", "error", err)
			return nil
		}
		var requests []reconcile.Request
		for _, route := range routes {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      route.Name,
					Namespace: route.Namespace,
				},
			})
		}
		return requests
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{RateLimiter: defaultRateLimiter()}).
		For(&gatewayv1.HTTPRoute{}).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.findHTTPRoutesForGateway),
			// Only trigger on spec changes (generation bump), ignore status-only updates
			// to prevent reconciliation loops between HTTPRoute and Gateway controllers
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&corev1.Service{},
			handler.EnqueueRequestsFromMapFunc(r.findHTTPRoutesForService),
			// Only trigger on spec changes (generation bump), ignore status-only updates
			// (e.g., endpoint readiness) to avoid unnecessary cluster-wide HTTPRoute lists.
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Watches(
			&gatewayparamsv1alpha1.VarnishCachePolicy{},
			handler.EnqueueRequestsFromMapFunc(r.findHTTPRoutesForVCP),
		).
		Watches(
			&gatewayv1.BackendTLSPolicy{},
			handler.EnqueueRequestsFromMapFunc(r.findHTTPRoutesForBackendTLSPolicy),
		).
		Watches(
			&gatewayv1beta1.ReferenceGrant{},
			handler.EnqueueRequestsFromMapFunc(r.findHTTPRoutesForReferenceGrant),
		).
		Complete(r)
}
