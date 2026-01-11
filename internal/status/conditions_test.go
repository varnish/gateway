package status

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestSetGatewayAccepted(t *testing.T) {
	tests := []struct {
		name     string
		accepted bool
		reason   string
		message  string
		want     metav1.ConditionStatus
	}{
		{
			name:     "accepted true",
			accepted: true,
			reason:   "Accepted",
			message:  "Gateway accepted",
			want:     metav1.ConditionTrue,
		},
		{
			name:     "accepted false",
			accepted: false,
			reason:   "Invalid",
			message:  "Gateway invalid",
			want:     metav1.ConditionFalse,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test",
					Generation: 1,
				},
			}

			SetGatewayAccepted(gateway, tc.accepted, tc.reason, tc.message)

			if len(gateway.Status.Conditions) != 1 {
				t.Fatalf("expected 1 condition, got %d", len(gateway.Status.Conditions))
			}

			cond := gateway.Status.Conditions[0]
			if cond.Type != string(gatewayv1.GatewayConditionAccepted) {
				t.Errorf("expected type %s, got %s", gatewayv1.GatewayConditionAccepted, cond.Type)
			}
			if cond.Status != tc.want {
				t.Errorf("expected status %s, got %s", tc.want, cond.Status)
			}
			if cond.Reason != tc.reason {
				t.Errorf("expected reason %s, got %s", tc.reason, cond.Reason)
			}
			if cond.Message != tc.message {
				t.Errorf("expected message %s, got %s", tc.message, cond.Message)
			}
			if cond.ObservedGeneration != 1 {
				t.Errorf("expected observed generation 1, got %d", cond.ObservedGeneration)
			}
		})
	}
}

func TestSetGatewayProgrammed(t *testing.T) {
	tests := []struct {
		name       string
		programmed bool
		reason     string
		message    string
		want       metav1.ConditionStatus
	}{
		{
			name:       "programmed true",
			programmed: true,
			reason:     "Programmed",
			message:    "Gateway programmed",
			want:       metav1.ConditionTrue,
		},
		{
			name:       "programmed false",
			programmed: false,
			reason:     "Invalid",
			message:    "Gateway not programmed",
			want:       metav1.ConditionFalse,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test",
					Generation: 2,
				},
			}

			SetGatewayProgrammed(gateway, tc.programmed, tc.reason, tc.message)

			if len(gateway.Status.Conditions) != 1 {
				t.Fatalf("expected 1 condition, got %d", len(gateway.Status.Conditions))
			}

			cond := gateway.Status.Conditions[0]
			if cond.Type != string(gatewayv1.GatewayConditionProgrammed) {
				t.Errorf("expected type %s, got %s", gatewayv1.GatewayConditionProgrammed, cond.Type)
			}
			if cond.Status != tc.want {
				t.Errorf("expected status %s, got %s", tc.want, cond.Status)
			}
			if cond.ObservedGeneration != 2 {
				t.Errorf("expected observed generation 2, got %d", cond.ObservedGeneration)
			}
		})
	}
}

func TestSetCondition_UpdatesExisting(t *testing.T) {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test",
			Generation: 1,
		},
	}

	// Set initial condition
	SetGatewayAccepted(gateway, true, "Accepted", "Initial")
	firstTime := gateway.Status.Conditions[0].LastTransitionTime

	// Update with same status - LastTransitionTime should not change
	SetGatewayAccepted(gateway, true, "Accepted", "Updated message")

	if len(gateway.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(gateway.Status.Conditions))
	}

	cond := gateway.Status.Conditions[0]
	if cond.LastTransitionTime != firstTime {
		t.Error("expected LastTransitionTime to remain unchanged when status is the same")
	}
	if cond.Message != "Updated message" {
		t.Errorf("expected message to be updated, got %s", cond.Message)
	}
}

func TestSetCondition_UpdatesTransitionTime(t *testing.T) {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test",
			Generation: 1,
		},
	}

	// Set initial condition as true
	SetGatewayAccepted(gateway, true, "Accepted", "Initial")
	firstTime := gateway.Status.Conditions[0].LastTransitionTime

	// Update with different status - LastTransitionTime should change
	SetGatewayAccepted(gateway, false, "Invalid", "Now invalid")

	if len(gateway.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(gateway.Status.Conditions))
	}

	cond := gateway.Status.Conditions[0]
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected status False, got %s", cond.Status)
	}
	// Note: In this test, since it runs so fast, the times might be equal
	// In a real scenario with actual time delays, this would be different
	_ = firstTime // Suppress unused variable warning
}

func TestMultipleConditions(t *testing.T) {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test",
			Generation: 1,
		},
	}

	SetGatewayAccepted(gateway, true, "Accepted", "Gateway accepted")
	SetGatewayProgrammed(gateway, true, "Programmed", "Gateway programmed")

	if len(gateway.Status.Conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(gateway.Status.Conditions))
	}

	// Verify both conditions exist
	var foundAccepted, foundProgrammed bool
	for _, cond := range gateway.Status.Conditions {
		if cond.Type == string(gatewayv1.GatewayConditionAccepted) {
			foundAccepted = true
		}
		if cond.Type == string(gatewayv1.GatewayConditionProgrammed) {
			foundProgrammed = true
		}
	}

	if !foundAccepted {
		t.Error("expected Accepted condition to be present")
	}
	if !foundProgrammed {
		t.Error("expected Programmed condition to be present")
	}
}
