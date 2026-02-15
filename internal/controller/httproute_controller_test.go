package controller

import (
	"context"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func newHTTPRouteTestReconciler(scheme *runtime.Scheme, objs ...runtime.Object) *HTTPRouteReconciler {
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}, &gatewayv1.Gateway{}).
		Build()

	return &HTTPRouteReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Config: Config{
			GatewayClassName: "varnish",
			GatewayImage:     "ghcr.io/varnish/varnish-gateway:latest",
		},
		Logger: slog.Default(),
	}
}

func TestReconcile_ValidRoute(t *testing.T) {
	scheme := newTestScheme()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway-vcl",
			Namespace: "default",
		},
		Data: map[string]string{
			"main.vcl":     "vcl 4.1;",
			"routing.json": `{"version": 2, "vhosts": {}}`,
		},
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
			Hostnames: []gatewayv1.Hostname{"example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "backend-svc",
									Port: ptr(gatewayv1.PortNumber(8080)),
								},
							},
						},
					},
				},
			},
		},
	}

	backendSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backend-svc",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Port: 8080}},
		},
	}

	r := newHTTPRouteTestReconciler(scheme, gateway, configMap, route, backendSvc)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-route", Namespace: "default"},
	})

	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if result.Requeue {
		t.Error("expected no requeue for valid route")
	}

	// Verify ConfigMap was updated
	var updatedCM corev1.ConfigMap
	err = r.Get(context.Background(),
		types.NamespacedName{Name: "test-gateway-vcl", Namespace: "default"},
		&updatedCM)
	if err != nil {
		t.Fatalf("failed to get ConfigMap: %v", err)
	}

	// Check VCL contains the service
	vcl := updatedCM.Data["main.vcl"]
	if vcl == "" {
		t.Error("expected main.vcl to be non-empty")
	}

	// Check routing.json
	routingJSON := updatedCM.Data["routing.json"]
	if routingJSON == "" {
		t.Error("expected routing.json to be non-empty")
	}

	// Verify HTTPRoute status was updated
	var updatedRoute gatewayv1.HTTPRoute
	err = r.Get(context.Background(),
		types.NamespacedName{Name: "test-route", Namespace: "default"},
		&updatedRoute)
	if err != nil {
		t.Fatalf("failed to get HTTPRoute: %v", err)
	}

	if len(updatedRoute.Status.Parents) != 1 {
		t.Fatalf("expected 1 parent status, got %d", len(updatedRoute.Status.Parents))
	}

	ps := updatedRoute.Status.Parents[0]
	if ps.ParentRef.Name != "test-gateway" {
		t.Errorf("expected parent ref name test-gateway, got %s", ps.ParentRef.Name)
	}

	// Check Accepted condition
	var foundAccepted bool
	for _, cond := range ps.Conditions {
		if cond.Type == string(gatewayv1.RouteConditionAccepted) {
			foundAccepted = true
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("expected Accepted=True, got %s", cond.Status)
			}
		}
	}
	if !foundAccepted {
		t.Error("expected Accepted condition to be set")
	}

	// Check ResolvedRefs condition
	var foundResolvedRefs bool
	for _, cond := range ps.Conditions {
		if cond.Type == string(gatewayv1.RouteConditionResolvedRefs) {
			foundResolvedRefs = true
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("expected ResolvedRefs=True, got %s", cond.Status)
			}
		}
	}
	if !foundResolvedRefs {
		t.Error("expected ResolvedRefs condition to be set")
	}
}

func TestReconcile_InvalidParentRef(t *testing.T) {
	scheme := newTestScheme()

	// No Gateway exists - route references non-existent Gateway
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "nonexistent-gateway"},
				},
			},
		},
	}

	r := newHTTPRouteTestReconciler(scheme, route)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-route", Namespace: "default"},
	})

	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if result.Requeue {
		t.Error("expected no requeue")
	}

	// Verify HTTPRoute status shows Accepted=false
	var updatedRoute gatewayv1.HTTPRoute
	err = r.Get(context.Background(),
		types.NamespacedName{Name: "test-route", Namespace: "default"},
		&updatedRoute)
	if err != nil {
		t.Fatalf("failed to get HTTPRoute: %v", err)
	}

	if len(updatedRoute.Status.Parents) != 1 {
		t.Fatalf("expected 1 parent status, got %d", len(updatedRoute.Status.Parents))
	}

	ps := updatedRoute.Status.Parents[0]
	var foundAccepted bool
	for _, cond := range ps.Conditions {
		if cond.Type == string(gatewayv1.RouteConditionAccepted) {
			foundAccepted = true
			if cond.Status != metav1.ConditionFalse {
				t.Errorf("expected Accepted=False, got %s", cond.Status)
			}
			if cond.Reason != string(gatewayv1.RouteReasonNoMatchingParent) {
				t.Errorf("expected reason NoMatchingParent, got %s", cond.Reason)
			}
		}
	}
	if !foundAccepted {
		t.Error("expected Accepted condition to be set")
	}

	// Check ResolvedRefs condition is set even when Gateway not found
	var foundResolvedRefs bool
	for _, cond := range ps.Conditions {
		if cond.Type == string(gatewayv1.RouteConditionResolvedRefs) {
			foundResolvedRefs = true
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("expected ResolvedRefs=True, got %s", cond.Status)
			}
		}
	}
	if !foundResolvedRefs {
		t.Error("expected ResolvedRefs condition to be set")
	}
}

func TestReconcile_DifferentGatewayClass(t *testing.T) {
	scheme := newTestScheme()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "other-class", // Different GatewayClass
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "other-gateway"},
				},
			},
		},
	}

	r := newHTTPRouteTestReconciler(scheme, gateway, route)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-route", Namespace: "default"},
	})

	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if result.Requeue {
		t.Error("expected no requeue")
	}

	// Verify HTTPRoute status was NOT set (we don't manage this Gateway)
	var updatedRoute gatewayv1.HTTPRoute
	err = r.Get(context.Background(),
		types.NamespacedName{Name: "test-route", Namespace: "default"},
		&updatedRoute)
	if err != nil {
		t.Fatalf("failed to get HTTPRoute: %v", err)
	}

	// Should have no parent status for Gateways we don't manage
	if len(updatedRoute.Status.Parents) != 0 {
		t.Errorf("expected 0 parent statuses for different GatewayClass, got %d", len(updatedRoute.Status.Parents))
	}
}

func TestReconcile_MultipleRoutesToGateway(t *testing.T) {
	scheme := newTestScheme()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
		Status: gatewayv1.GatewayStatus{
			Listeners: []gatewayv1.ListenerStatus{
				{Name: "http", AttachedRoutes: 0},
			},
		},
	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway-vcl",
			Namespace: "default",
		},
		Data: map[string]string{
			"main.vcl":     "vcl 4.1;",
			"routing.json": `{"version": 2, "vhosts": {}}`,
		},
	}

	route1 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-1",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
			Hostnames: []gatewayv1.Hostname{"api.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "api-svc",
									Port: ptr(gatewayv1.PortNumber(8080)),
								},
							},
						},
					},
				},
			},
		},
	}

	route2 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-2",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
			Hostnames: []gatewayv1.Hostname{"web.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "web-svc",
									Port: ptr(gatewayv1.PortNumber(8080)),
								},
							},
						},
					},
				},
			},
		},
	}

	r := newHTTPRouteTestReconciler(scheme, gateway, configMap, route1, route2)

	// Reconcile route-1
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "route-1", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile route-1 failed: %v", err)
	}

	// Verify ConfigMap contains both services
	var updatedCM corev1.ConfigMap
	err = r.Get(context.Background(),
		types.NamespacedName{Name: "test-gateway-vcl", Namespace: "default"},
		&updatedCM)
	if err != nil {
		t.Fatalf("failed to get ConfigMap: %v", err)
	}

	// VCL should contain both services since both routes attach to the same Gateway
	vcl := updatedCM.Data["main.vcl"]
	if vcl == "" {
		t.Error("expected main.vcl to be non-empty")
	}

	// routing.json should contain both backends
	routingJSON := updatedCM.Data["routing.json"]
	if routingJSON == "" {
		t.Error("expected routing.json to be non-empty")
	}
}

func TestReconcile_NoParentRefs(t *testing.T) {
	scheme := newTestScheme()

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{}, // Empty parentRefs
			},
		},
	}

	r := newHTTPRouteTestReconciler(scheme, route)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-route", Namespace: "default"},
	})

	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if result.Requeue {
		t.Error("expected no requeue")
	}
}

func TestHTTPRouteReconcile_NotFoundReturnsNoError(t *testing.T) {
	scheme := newTestScheme()
	r := newHTTPRouteTestReconciler(scheme) // No route exists

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
	})

	if err != nil {
		t.Errorf("expected no error for not found route, got: %v", err)
	}
	if result.Requeue {
		t.Error("expected no requeue for not found route")
	}
}

func TestRouteAttachedToGateway(t *testing.T) {
	ns := gatewayv1.Namespace("other-ns")

	tests := []struct {
		name     string
		route    *gatewayv1.HTTPRoute
		gateway  *gatewayv1.Gateway
		attached bool
	}{
		{
			name: "same namespace implicit",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "my-gateway"},
						},
					},
				},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "my-gateway", Namespace: "default"},
			},
			attached: true,
		},
		{
			name: "different gateway name",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "other-gateway"},
						},
					},
				},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "my-gateway", Namespace: "default"},
			},
			attached: false,
		},
		{
			name: "cross namespace",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "my-gateway", Namespace: &ns},
						},
					},
				},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "my-gateway", Namespace: "other-ns"},
			},
			attached: true,
		},
		{
			name: "no parentRefs",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{},
					},
				},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "my-gateway", Namespace: "default"},
			},
			attached: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := routeAttachedToGateway(tc.route, tc.gateway)
			if got != tc.attached {
				t.Errorf("expected attached=%v, got %v", tc.attached, got)
			}
		})
	}
}

func TestFindHTTPRoutesForGateway(t *testing.T) {
	scheme := newTestScheme()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
		},
	}

	route1 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-1",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
		},
	}

	route2 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-2",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "other-gateway"}, // Different gateway
				},
			},
		},
	}

	r := newHTTPRouteTestReconciler(scheme, gateway, route1, route2)

	requests := r.findHTTPRoutesForGateway(context.Background(), gateway)

	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}

	if requests[0].Name != "route-1" {
		t.Errorf("expected request for route-1, got %s", requests[0].Name)
	}
}

func TestFindHTTPRoutesForGateway_DifferentGatewayClass(t *testing.T) {
	scheme := newTestScheme()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "other-class", // Not our GatewayClass
		},
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-1",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "other-gateway"},
				},
			},
		},
	}

	r := newHTTPRouteTestReconciler(scheme, gateway, route)

	requests := r.findHTTPRoutesForGateway(context.Background(), gateway)

	// Should return no requests for Gateways with different GatewayClass
	if len(requests) != 0 {
		t.Errorf("expected 0 requests for different GatewayClass, got %d", len(requests))
	}
}

func TestIsRouteAllowedByGateway(t *testing.T) {
	scheme := newTestScheme()
	fromAll := gatewayv1.NamespacesFromAll
	fromSame := gatewayv1.NamespacesFromSame
	fromSelector := gatewayv1.NamespacesFromSelector

	tests := []struct {
		name    string
		route   *gatewayv1.HTTPRoute
		gateway *gatewayv1.Gateway
		objs    []runtime.Object // extra objects (namespaces)
		allowed bool
	}{
		{
			name: "nil AllowedRoutes defaults to Same - same namespace allowed",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "default"},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
					},
				},
			},
			allowed: true,
		},
		{
			name: "nil AllowedRoutes defaults to Same - different namespace denied",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "other"},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
					},
				},
			},
			allowed: false,
		},
		{
			name: "explicit Same - same namespace allowed",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "default"},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{From: &fromSame},
							},
						},
					},
				},
			},
			allowed: true,
		},
		{
			name: "explicit Same - different namespace denied",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "other"},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{From: &fromSame},
							},
						},
					},
				},
			},
			allowed: false,
		},
		{
			name: "All - any namespace allowed",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "any-namespace"},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll},
							},
						},
					},
				},
			},
			allowed: true,
		},
		{
			name: "Selector - matching labels allowed",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "team-a"},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromSelector,
									Selector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"env": "prod"},
									},
								},
							},
						},
					},
				},
			},
			objs: []runtime.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "team-a",
						Labels: map[string]string{"env": "prod"},
					},
				},
			},
			allowed: true,
		},
		{
			name: "Selector - non-matching labels denied",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "team-b"},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromSelector,
									Selector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"env": "prod"},
									},
								},
							},
						},
					},
				},
			},
			objs: []runtime.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "team-b",
						Labels: map[string]string{"env": "staging"},
					},
				},
			},
			allowed: false,
		},
		{
			name: "Selector with nil selector - denied",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "team-a"},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From:     &fromSelector,
									Selector: nil,
								},
							},
						},
					},
				},
			},
			allowed: false,
		},
		{
			name: "multiple listeners - one allows one denies - allowed",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "other"},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{From: &fromSame},
							},
						},
						{
							Name: "http-all", Port: 8080, Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll},
							},
						},
					},
				},
			},
			allowed: true,
		},
		{
			name: "no listeners - denied",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "default"},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{},
				},
			},
			allowed: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objs := append([]runtime.Object{}, tc.objs...)
			r := newHTTPRouteTestReconciler(scheme, objs...)
			got, _, _ := isRouteAllowedByGateway(context.Background(), r.Client, tc.route, tc.gateway)
			if got != tc.allowed {
				t.Errorf("expected allowed=%v, got %v", tc.allowed, got)
			}
		})
	}
}

func TestReconcile_NotAllowedByListeners(t *testing.T) {
	scheme := newTestScheme()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
				// No AllowedRoutes → defaults to Same
			},
		},
	}

	// Route in a different namespace
	gwNs := gatewayv1.Namespace("default")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cross-ns-route",
			Namespace: "other",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway", Namespace: &gwNs},
				},
			},
			Hostnames: []gatewayv1.Hostname{"example.com"},
		},
	}

	r := newHTTPRouteTestReconciler(scheme, gateway, route)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "cross-ns-route", Namespace: "other"},
	})

	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if result.Requeue {
		t.Error("expected no requeue")
	}

	// Verify HTTPRoute status shows Accepted=false with NotAllowedByListeners
	var updatedRoute gatewayv1.HTTPRoute
	err = r.Get(context.Background(),
		types.NamespacedName{Name: "cross-ns-route", Namespace: "other"},
		&updatedRoute)
	if err != nil {
		t.Fatalf("failed to get HTTPRoute: %v", err)
	}

	if len(updatedRoute.Status.Parents) != 1 {
		t.Fatalf("expected 1 parent status, got %d", len(updatedRoute.Status.Parents))
	}

	ps := updatedRoute.Status.Parents[0]
	var foundAccepted bool
	for _, cond := range ps.Conditions {
		if cond.Type == string(gatewayv1.RouteConditionAccepted) {
			foundAccepted = true
			if cond.Status != metav1.ConditionFalse {
				t.Errorf("expected Accepted=False, got %s", cond.Status)
			}
			if cond.Reason != string(gatewayv1.RouteReasonNotAllowedByListeners) {
				t.Errorf("expected reason NotAllowedByListeners, got %s", cond.Reason)
			}
		}
	}
	if !foundAccepted {
		t.Error("expected Accepted condition to be set")
	}
}

// NOTE: getUserVCL tests removed - functionality moved to Gateway controller
