//go:build integration && !race

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestHTTPRouteReconcile_GatewayControllerUpdatesAttachedRoutes_Envtest tests that the
// Gateway controller correctly computes AttachedRoutes when triggered by HTTPRoute changes.
// This validates the consolidated status management: the Gateway controller owns all
// listener status fields (conditions, SupportedKinds, AttachedRoutes), eliminating
// SSA conflicts between controllers.
func TestHTTPRouteReconcile_GatewayControllerUpdatesAttachedRoutes_Envtest(t *testing.T) {
	ctx := context.Background()

	// Create GatewayClass
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "varnish",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "varnish-software.com/gateway",
		},
	}
	if err := testEnv.Client.Create(ctx, gatewayClass); err != nil {
		t.Fatalf("failed to create gatewayclass: %v", err)
	}
	defer func() {
		_ = testEnv.Client.Delete(ctx, gatewayClass)
	}()

	// Create Gateway with listener
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway-httproute",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}
	if err := testEnv.Client.Create(ctx, gateway); err != nil {
		t.Fatalf("failed to create gateway: %v", err)
	}
	defer func() {
		_ = testEnv.Client.Delete(ctx, gateway)
	}()

	// Create Gateway reconciler
	gwReconciler := NewEnvtestGatewayReconciler(testEnv)

	// Reconcile Gateway to add finalizer
	_, err := gwReconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-gateway-httproute", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("gateway reconcile (finalizer) failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Reconcile Gateway to create resources and set initial status
	_, err = gwReconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-gateway-httproute", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("gateway reconcile (resources) failed: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Verify Gateway has listener status with conditions and AttachedRoutes=0
	var updatedGateway gatewayv1.Gateway
	if err := testEnv.Client.Get(ctx,
		types.NamespacedName{Name: "test-gateway-httproute", Namespace: "default"},
		&updatedGateway); err != nil {
		t.Fatalf("failed to get gateway: %v", err)
	}

	if len(updatedGateway.Status.Listeners) == 0 {
		t.Fatal("gateway should have listener status")
	}

	if updatedGateway.Status.Listeners[0].AttachedRoutes != 0 {
		t.Errorf("expected initial AttachedRoutes=0, got %d", updatedGateway.Status.Listeners[0].AttachedRoutes)
	}
	if len(updatedGateway.Status.Listeners[0].Conditions) == 0 {
		t.Fatal("expected initial listener conditions")
	}
	t.Logf("Initial state: AttachedRoutes=%d, Conditions=%d",
		updatedGateway.Status.Listeners[0].AttachedRoutes,
		len(updatedGateway.Status.Listeners[0].Conditions))

	// Create a Service for the HTTPRoute to reference
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Port: 8080, Name: "http"},
			},
		},
	}
	if err := testEnv.Client.Create(ctx, service); err != nil {
		t.Fatalf("failed to create service: %v", err)
	}
	defer func() {
		_ = testEnv.Client.Delete(ctx, service)
	}()

	// Create HTTPRoute attached to Gateway
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name: "test-gateway-httproute",
					},
				},
			},
			Hostnames: []gatewayv1.Hostname{"test.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "test-service",
									Port: ptrTo(gatewayv1.PortNumber(8080)),
								},
							},
						},
					},
				},
			},
		},
	}
	if err := testEnv.Client.Create(ctx, route); err != nil {
		t.Fatalf("failed to create httproute: %v", err)
	}
	defer func() {
		_ = testEnv.Client.Delete(ctx, route)
	}()

	// Create HTTPRoute reconciler and reconcile (sets route status only, no Gateway status)
	httpRouteReconciler := NewEnvtestHTTPRouteReconciler(testEnv)
	_, err = httpRouteReconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-route", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("httproute reconcile failed: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Re-reconcile with Gateway controller (simulates HTTPRoute watch trigger)
	// This is where AttachedRoutes gets computed.
	_, err = gwReconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-gateway-httproute", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("gateway re-reconcile failed: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Verify Gateway listener status
	if err := testEnv.Client.Get(ctx,
		types.NamespacedName{Name: "test-gateway-httproute", Namespace: "default"},
		&updatedGateway); err != nil {
		t.Fatalf("failed to get updated gateway: %v", err)
	}

	if len(updatedGateway.Status.Listeners) == 0 {
		t.Fatal("gateway should still have listener status")
	}

	// Verify AttachedRoutes was updated by Gateway controller
	if updatedGateway.Status.Listeners[0].AttachedRoutes != 1 {
		t.Errorf("expected AttachedRoutes=1, got %d", updatedGateway.Status.Listeners[0].AttachedRoutes)
	} else {
		t.Log("AttachedRoutes updated correctly by Gateway controller")
	}

	// Verify SupportedKinds is present
	if updatedGateway.Status.Listeners[0].SupportedKinds == nil ||
		len(updatedGateway.Status.Listeners[0].SupportedKinds) == 0 {
		t.Error("SupportedKinds should not be nil or empty")
	} else {
		t.Log("SupportedKinds preserved")
	}

	// Verify Conditions are present (no SSA conflict wiping them)
	if len(updatedGateway.Status.Listeners[0].Conditions) == 0 {
		t.Error("listener conditions should be preserved")
	} else {
		t.Logf("Listener has %d conditions (no SSA conflict)",
			len(updatedGateway.Status.Listeners[0].Conditions))
		// Verify conditions have correct observedGeneration
		for _, c := range updatedGateway.Status.Listeners[0].Conditions {
			if c.ObservedGeneration != updatedGateway.Generation {
				t.Errorf("condition %s has stale observedGeneration %d, expected %d",
					c.Type, c.ObservedGeneration, updatedGateway.Generation)
			}
		}
	}

	// Verify HTTPRoute status was updated
	var updatedRoute gatewayv1.HTTPRoute
	if err := testEnv.Client.Get(ctx,
		types.NamespacedName{Name: "test-route", Namespace: "default"},
		&updatedRoute); err != nil {
		t.Fatalf("failed to get updated route: %v", err)
	}

	if len(updatedRoute.Status.Parents) == 0 {
		t.Error("route should have parent status")
	} else {
		t.Logf("HTTPRoute has parent status with %d conditions",
			len(updatedRoute.Status.Parents[0].Conditions))
	}
}

func ptrTo[T any](v T) *T {
	return &v
}
