package status

import (
	"testing"
	"time"

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

	// Backdate the initial condition's LastTransitionTime to 1 hour ago
	// so we can reliably detect that a status change updates it.
	oldTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	gateway.Status.Conditions[0].LastTransitionTime = oldTime

	// Update with different status - LastTransitionTime should change
	SetGatewayAccepted(gateway, false, "Invalid", "Now invalid")

	if len(gateway.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition after update, got %d", len(gateway.Status.Conditions))
	}

	cond := gateway.Status.Conditions[0]
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected status False, got %s", cond.Status)
	}
	if !cond.LastTransitionTime.After(oldTime.Time) {
		t.Errorf("expected LastTransitionTime to be updated after status change, got %v (old: %v)",
			cond.LastTransitionTime.Time, oldTime.Time)
	}
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

func TestSetHTTPRouteAccepted(t *testing.T) {
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
			message:  "Route accepted",
			want:     metav1.ConditionTrue,
		},
		{
			name:     "accepted false",
			accepted: false,
			reason:   "NoMatchingParent",
			message:  "Gateway not found",
			want:     metav1.ConditionFalse,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			route := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-route",
					Namespace:  "default",
					Generation: 1,
				},
			}

			parentRef := gatewayv1.ParentReference{
				Name: "test-gateway",
			}

			SetHTTPRouteAccepted(route, parentRef, "test-controller", tc.accepted, tc.reason, tc.message)

			if len(route.Status.Parents) != 1 {
				t.Fatalf("expected 1 parent status, got %d", len(route.Status.Parents))
			}

			ps := route.Status.Parents[0]
			if ps.ParentRef.Name != "test-gateway" {
				t.Errorf("expected parent ref name test-gateway, got %s", ps.ParentRef.Name)
			}
			if string(ps.ControllerName) != "test-controller" {
				t.Errorf("expected controller name test-controller, got %s", ps.ControllerName)
			}

			if len(ps.Conditions) != 1 {
				t.Fatalf("expected 1 condition, got %d", len(ps.Conditions))
			}

			cond := ps.Conditions[0]
			if cond.Type != string(gatewayv1.RouteConditionAccepted) {
				t.Errorf("expected type %s, got %s", gatewayv1.RouteConditionAccepted, cond.Type)
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

func TestSetHTTPRouteResolvedRefs(t *testing.T) {
	tests := []struct {
		name     string
		resolved bool
		reason   string
		message  string
		want     metav1.ConditionStatus
	}{
		{
			name:     "resolved true",
			resolved: true,
			reason:   "ResolvedRefs",
			message:  "All references resolved",
			want:     metav1.ConditionTrue,
		},
		{
			name:     "resolved false",
			resolved: false,
			reason:   "BackendNotFound",
			message:  "Backend service not found",
			want:     metav1.ConditionFalse,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			route := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-route",
					Namespace:  "default",
					Generation: 2,
				},
			}

			parentRef := gatewayv1.ParentReference{
				Name: "test-gateway",
			}

			SetHTTPRouteResolvedRefs(route, parentRef, "test-controller", tc.resolved, tc.reason, tc.message)

			if len(route.Status.Parents) != 1 {
				t.Fatalf("expected 1 parent status, got %d", len(route.Status.Parents))
			}

			ps := route.Status.Parents[0]
			if len(ps.Conditions) != 1 {
				t.Fatalf("expected 1 condition, got %d", len(ps.Conditions))
			}

			cond := ps.Conditions[0]
			if cond.Type != string(gatewayv1.RouteConditionResolvedRefs) {
				t.Errorf("expected type %s, got %s", gatewayv1.RouteConditionResolvedRefs, cond.Type)
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

func TestHTTPRouteMultipleConditions(t *testing.T) {
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-route",
			Namespace:  "default",
			Generation: 1,
		},
	}

	parentRef := gatewayv1.ParentReference{
		Name: "test-gateway",
	}

	SetHTTPRouteAccepted(route, parentRef, "test-controller", true, "Accepted", "Route accepted")
	SetHTTPRouteResolvedRefs(route, parentRef, "test-controller", true, "ResolvedRefs", "All refs resolved")

	if len(route.Status.Parents) != 1 {
		t.Fatalf("expected 1 parent status, got %d", len(route.Status.Parents))
	}

	ps := route.Status.Parents[0]
	if len(ps.Conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(ps.Conditions))
	}

	var foundAccepted, foundResolvedRefs bool
	for _, cond := range ps.Conditions {
		if cond.Type == string(gatewayv1.RouteConditionAccepted) {
			foundAccepted = true
		}
		if cond.Type == string(gatewayv1.RouteConditionResolvedRefs) {
			foundResolvedRefs = true
		}
	}

	if !foundAccepted {
		t.Error("expected Accepted condition to be present")
	}
	if !foundResolvedRefs {
		t.Error("expected ResolvedRefs condition to be present")
	}
}

func TestHTTPRouteMultipleParents(t *testing.T) {
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-route",
			Namespace:  "default",
			Generation: 1,
		},
	}

	parentRef1 := gatewayv1.ParentReference{
		Name: "gateway-1",
	}
	parentRef2 := gatewayv1.ParentReference{
		Name: "gateway-2",
	}

	SetHTTPRouteAccepted(route, parentRef1, "test-controller", true, "Accepted", "Route accepted by gateway-1")
	SetHTTPRouteAccepted(route, parentRef2, "test-controller", false, "NoMatchingParent", "Gateway not found")

	if len(route.Status.Parents) != 2 {
		t.Fatalf("expected 2 parent statuses, got %d", len(route.Status.Parents))
	}

	// Find each parent status
	var ps1, ps2 *gatewayv1.RouteParentStatus
	for i := range route.Status.Parents {
		ps := &route.Status.Parents[i]
		if ps.ParentRef.Name == "gateway-1" {
			ps1 = ps
		}
		if ps.ParentRef.Name == "gateway-2" {
			ps2 = ps
		}
	}

	if ps1 == nil {
		t.Fatal("expected parent status for gateway-1")
	}
	if ps2 == nil {
		t.Fatal("expected parent status for gateway-2")
	}

	// Check gateway-1 status
	if len(ps1.Conditions) != 1 {
		t.Fatalf("expected 1 condition for gateway-1, got %d", len(ps1.Conditions))
	}
	if ps1.Conditions[0].Status != metav1.ConditionTrue {
		t.Errorf("expected gateway-1 accepted, got %s", ps1.Conditions[0].Status)
	}

	// Check gateway-2 status
	if len(ps2.Conditions) != 1 {
		t.Fatalf("expected 1 condition for gateway-2, got %d", len(ps2.Conditions))
	}
	if ps2.Conditions[0].Status != metav1.ConditionFalse {
		t.Errorf("expected gateway-2 not accepted, got %s", ps2.Conditions[0].Status)
	}
}

func TestParentRefMatches(t *testing.T) {
	ns := gatewayv1.Namespace("test-ns")
	section := gatewayv1.SectionName("http")
	group := gatewayv1.Group(gatewayv1.GroupName)
	kind := gatewayv1.Kind("Gateway")

	tests := []struct {
		name  string
		a     gatewayv1.ParentReference
		b     gatewayv1.ParentReference
		match bool
	}{
		{
			name:  "same name only",
			a:     gatewayv1.ParentReference{Name: "gateway"},
			b:     gatewayv1.ParentReference{Name: "gateway"},
			match: true,
		},
		{
			name:  "different names",
			a:     gatewayv1.ParentReference{Name: "gateway-1"},
			b:     gatewayv1.ParentReference{Name: "gateway-2"},
			match: false,
		},
		{
			name:  "same with namespace",
			a:     gatewayv1.ParentReference{Name: "gateway", Namespace: &ns},
			b:     gatewayv1.ParentReference{Name: "gateway", Namespace: &ns},
			match: true,
		},
		{
			name:  "one with namespace one without",
			a:     gatewayv1.ParentReference{Name: "gateway", Namespace: &ns},
			b:     gatewayv1.ParentReference{Name: "gateway"},
			match: false,
		},
		{
			name:  "same with section name",
			a:     gatewayv1.ParentReference{Name: "gateway", SectionName: &section},
			b:     gatewayv1.ParentReference{Name: "gateway", SectionName: &section},
			match: true,
		},
		{
			name:  "explicit group and kind matches default",
			a:     gatewayv1.ParentReference{Name: "gateway", Group: &group, Kind: &kind},
			b:     gatewayv1.ParentReference{Name: "gateway"},
			match: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parentRefMatches(tc.a, tc.b)
			if got != tc.match {
				t.Errorf("expected match=%v, got %v", tc.match, got)
			}
		})
	}
}
