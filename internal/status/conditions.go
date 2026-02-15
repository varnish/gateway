package status

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// boolToStatus converts a bool to a metav1.ConditionStatus.
func boolToStatus(val bool) metav1.ConditionStatus {
	if val {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// newCondition builds a metav1.Condition with the given fields.
func newCondition(condType string, status metav1.ConditionStatus, generation int64, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
}

// SetGatewayClassAccepted sets the Accepted condition on a GatewayClass.
func SetGatewayClassAccepted(gc *gatewayv1.GatewayClass, accepted bool, reason, message string) {
	cond := newCondition(string(gatewayv1.GatewayClassConditionStatusAccepted), boolToStatus(accepted), gc.Generation, reason, message)
	setCondition(&gc.Status.Conditions, cond)
}

// SetGatewayAccepted sets the Accepted condition on a Gateway.
func SetGatewayAccepted(gateway *gatewayv1.Gateway, accepted bool, reason, message string) {
	cond := newCondition(string(gatewayv1.GatewayConditionAccepted), boolToStatus(accepted), gateway.Generation, reason, message)
	setCondition(&gateway.Status.Conditions, cond)
}

// SetGatewayProgrammed sets the Programmed condition on a Gateway.
func SetGatewayProgrammed(gateway *gatewayv1.Gateway, programmed bool, reason, message string) {
	cond := newCondition(string(gatewayv1.GatewayConditionProgrammed), boolToStatus(programmed), gateway.Generation, reason, message)
	setCondition(&gateway.Status.Conditions, cond)
}

// setCondition updates or adds a condition in the slice.
// If a condition with the same Type already exists, it is updated.
// Only updates LastTransitionTime if the Status has changed.
func setCondition(conditions *[]metav1.Condition, newCondition metav1.Condition) {
	for i, existing := range *conditions {
		if existing.Type == newCondition.Type {
			// Only update LastTransitionTime if status changed
			if existing.Status == newCondition.Status {
				newCondition.LastTransitionTime = existing.LastTransitionTime
			}
			(*conditions)[i] = newCondition
			return
		}
	}
	*conditions = append(*conditions, newCondition)
}

// SetHTTPRouteAccepted sets the Accepted condition on an HTTPRoute for a specific parent Gateway.
// Each parent Gateway gets its own status entry in route.Status.Parents.
func SetHTTPRouteAccepted(route *gatewayv1.HTTPRoute, parentRef gatewayv1.ParentReference,
	controllerName string, accepted bool, reason, message string) {

	cond := newCondition(string(gatewayv1.RouteConditionAccepted), boolToStatus(accepted), route.Generation, reason, message)
	parentStatus := findOrCreateRouteParentStatus(route, parentRef, controllerName)
	setCondition(&parentStatus.Conditions, cond)
}

// SetHTTPRouteResolvedRefs sets the ResolvedRefs condition on an HTTPRoute for a specific parent Gateway.
func SetHTTPRouteResolvedRefs(route *gatewayv1.HTTPRoute, parentRef gatewayv1.ParentReference,
	controllerName string, resolved bool, reason, message string) {

	cond := newCondition(string(gatewayv1.RouteConditionResolvedRefs), boolToStatus(resolved), route.Generation, reason, message)
	parentStatus := findOrCreateRouteParentStatus(route, parentRef, controllerName)
	setCondition(&parentStatus.Conditions, cond)
}

// findOrCreateRouteParentStatus finds or creates a RouteParentStatus entry for the given parentRef.
// It matches on ParentRef and ControllerName.
func findOrCreateRouteParentStatus(route *gatewayv1.HTTPRoute, parentRef gatewayv1.ParentReference,
	controllerName string) *gatewayv1.RouteParentStatus {

	// Look for existing status entry
	for i := range route.Status.Parents {
		ps := &route.Status.Parents[i]
		if parentRefMatches(ps.ParentRef, parentRef) &&
			string(ps.ControllerName) == controllerName {
			return ps
		}
	}

	// Create new status entry
	newStatus := gatewayv1.RouteParentStatus{
		ParentRef:      parentRef,
		ControllerName: gatewayv1.GatewayController(controllerName),
		Conditions:     []metav1.Condition{},
	}
	route.Status.Parents = append(route.Status.Parents, newStatus)
	return &route.Status.Parents[len(route.Status.Parents)-1]
}

// parentRefMatches checks if two ParentReferences refer to the same parent.
// It compares Group, Kind, Namespace, Name, and SectionName.
func parentRefMatches(a, b gatewayv1.ParentReference) bool {
	// Compare Group (default to gateway.networking.k8s.io)
	aGroup := gatewayv1.GroupName
	if a.Group != nil {
		aGroup = string(*a.Group)
	}
	bGroup := gatewayv1.GroupName
	if b.Group != nil {
		bGroup = string(*b.Group)
	}
	if aGroup != bGroup {
		return false
	}

	// Compare Kind (default to Gateway)
	aKind := "Gateway"
	if a.Kind != nil {
		aKind = string(*a.Kind)
	}
	bKind := "Gateway"
	if b.Kind != nil {
		bKind = string(*b.Kind)
	}
	if aKind != bKind {
		return false
	}

	// Compare Namespace (if specified)
	if a.Namespace != nil && b.Namespace != nil {
		if *a.Namespace != *b.Namespace {
			return false
		}
	} else if a.Namespace != nil || b.Namespace != nil {
		return false
	}

	// Compare Name
	if a.Name != b.Name {
		return false
	}

	// Compare SectionName (if specified)
	if a.SectionName != nil && b.SectionName != nil {
		if *a.SectionName != *b.SectionName {
			return false
		}
	} else if a.SectionName != nil || b.SectionName != nil {
		return false
	}

	return true
}
