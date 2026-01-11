package status

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// SetGatewayAccepted sets the Accepted condition on a Gateway.
func SetGatewayAccepted(gateway *gatewayv1.Gateway, accepted bool, reason, message string) {
	status := metav1.ConditionTrue
	if !accepted {
		status = metav1.ConditionFalse
	}

	condition := metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionAccepted),
		Status:             status,
		ObservedGeneration: gateway.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}

	setCondition(&gateway.Status.Conditions, condition)
}

// SetGatewayProgrammed sets the Programmed condition on a Gateway.
func SetGatewayProgrammed(gateway *gatewayv1.Gateway, programmed bool, reason, message string) {
	status := metav1.ConditionTrue
	if !programmed {
		status = metav1.ConditionFalse
	}

	condition := metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionProgrammed),
		Status:             status,
		ObservedGeneration: gateway.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}

	setCondition(&gateway.Status.Conditions, condition)
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
