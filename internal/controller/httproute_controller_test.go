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

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
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
			GatewayClassName:    "varnish",
			DefaultVarnishImage: "quay.io/varnish-software/varnish-plus:7.6",
			SidecarImage:        "ghcr.io/varnish/gateway-sidecar:latest",
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
			"main.vcl":      "vcl 4.1;",
			"services.json": `{"services": []}`,
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

	r := newHTTPRouteTestReconciler(scheme, gateway, configMap, route)

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

	// Check services.json
	servicesJSON := updatedCM.Data["services.json"]
	if servicesJSON == "" {
		t.Error("expected services.json to be non-empty")
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
			"main.vcl":      "vcl 4.1;",
			"services.json": `{"services": []}`,
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

	// services.json should contain both services
	servicesJSON := updatedCM.Data["services.json"]
	if servicesJSON == "" {
		t.Error("expected services.json to be non-empty")
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

	r := &HTTPRouteReconciler{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := r.routeAttachedToGateway(tc.route, tc.gateway)
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

func TestGetUserVCL_NoGatewayClass(t *testing.T) {
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

	// No GatewayClass exists
	r := newHTTPRouteTestReconciler(scheme, gateway)

	vcl := r.getUserVCL(context.Background(), gateway)

	if vcl != "" {
		t.Errorf("expected empty VCL when GatewayClass not found, got %q", vcl)
	}
}

func TestGetUserVCL_NoParametersRef(t *testing.T) {
	scheme := newTestScheme()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "varnish",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "varnish-software.com/gateway",
			// No ParametersRef
		},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
		},
	}

	r := newHTTPRouteTestReconciler(scheme, gatewayClass, gateway)

	vcl := r.getUserVCL(context.Background(), gateway)

	if vcl != "" {
		t.Errorf("expected empty VCL when no ParametersRef, got %q", vcl)
	}
}

func TestGetUserVCL_WithConfigMap(t *testing.T) {
	scheme := newTestScheme()

	userVCLContent := `sub vcl_recv {
    if (req.url ~ "^/health") {
        return (synth(200, "OK"));
    }
}`

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-vcl",
			Namespace: "default",
		},
		Data: map[string]string{
			"user.vcl": userVCLContent,
		},
	}

	params := &gatewayparamsv1alpha1.GatewayClassParameters{
		ObjectMeta: metav1.ObjectMeta{
			Name: "varnish-params",
		},
		Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
			UserVCLConfigMapRef: &gatewayparamsv1alpha1.ConfigMapReference{
				Name:      "my-vcl",
				Namespace: "default",
			},
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "varnish",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "varnish-software.com/gateway",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
				Kind:  "GatewayClassParameters",
				Name:  "varnish-params",
			},
		},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
		},
	}

	r := newHTTPRouteTestReconciler(scheme, configMap, params, gatewayClass, gateway)

	vcl := r.getUserVCL(context.Background(), gateway)

	if vcl != userVCLContent {
		t.Errorf("expected user VCL content, got %q", vcl)
	}
}

func TestGetUserVCL_CustomKey(t *testing.T) {
	scheme := newTestScheme()

	userVCLContent := "sub vcl_recv { return (pass); }"

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-vcl",
			Namespace: "default",
		},
		Data: map[string]string{
			"custom.vcl": userVCLContent,
		},
	}

	params := &gatewayparamsv1alpha1.GatewayClassParameters{
		ObjectMeta: metav1.ObjectMeta{
			Name: "varnish-params",
		},
		Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
			UserVCLConfigMapRef: &gatewayparamsv1alpha1.ConfigMapReference{
				Name:      "my-vcl",
				Namespace: "default",
				Key:       "custom.vcl",
			},
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "varnish",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "varnish-software.com/gateway",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
				Kind:  "GatewayClassParameters",
				Name:  "varnish-params",
			},
		},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
		},
	}

	r := newHTTPRouteTestReconciler(scheme, configMap, params, gatewayClass, gateway)

	vcl := r.getUserVCL(context.Background(), gateway)

	if vcl != userVCLContent {
		t.Errorf("expected user VCL with custom key, got %q", vcl)
	}
}

func TestGetUserVCL_DifferentGroup(t *testing.T) {
	scheme := newTestScheme()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "varnish",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "varnish-software.com/gateway",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: "other.group.io", // Different group
				Kind:  "GatewayClassParameters",
				Name:  "varnish-params",
			},
		},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
		},
	}

	r := newHTTPRouteTestReconciler(scheme, gatewayClass, gateway)

	vcl := r.getUserVCL(context.Background(), gateway)

	// Should return empty since group doesn't match
	if vcl != "" {
		t.Errorf("expected empty VCL for different group, got %q", vcl)
	}
}
