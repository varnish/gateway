//go:build integration && !race

package controller

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
)

var testEnv *EnvtestEnvironment

// TestMain provides suite-level setup and teardown for envtest
func TestMain(m *testing.M) {
	var err error

	// Setup envtest environment (kube-apiserver + etcd)
	testEnv, err = SetupEnvtest()
	if err != nil {
		panic("failed to setup envtest: " + err.Error())
	}

	// Run tests
	code := m.Run()

	// Teardown
	if err := TeardownEnvtest(testEnv); err != nil {
		panic("failed to teardown envtest: " + err.Error())
	}

	os.Exit(code)
}

// TestReconcile_CreatesResources_Envtest tests full resource creation using envtest
// This test was previously skipped due to fake client SSA limitations.
// With envtest, we get a real API server that properly handles SSA.
func TestReconcile_CreatesResources_Envtest(t *testing.T) {
	ctx := context.Background()

	// Create GatewayClass first
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "varnish",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(ControllerName),
		},
	}
	if err := testEnv.Client.Create(ctx, gatewayClass); err != nil {
		t.Fatalf("failed to create gatewayclass: %v", err)
	}
	defer func() {
		_ = testEnv.Client.Delete(ctx, gatewayClass)
	}()

	// Create Gateway
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway-envtest",
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

	// Create reconciler with envtest client
	r := NewEnvtestGatewayReconciler(testEnv)

	// Reconcile creates resources (using SSA)
	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-gateway-envtest", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}

	// Give API server time to persist resources
	time.Sleep(200 * time.Millisecond)

	// Verify Deployment was created
	var deployment appsv1.Deployment
	err = testEnv.Client.Get(ctx,
		types.NamespacedName{Name: "test-gateway-envtest", Namespace: "default"},
		&deployment)
	if err != nil {
		t.Errorf("expected deployment to be created: %v", err)
	} else {
		t.Logf("✓ Deployment created successfully")
	}

	// Verify Service was created
	var service corev1.Service
	err = testEnv.Client.Get(ctx,
		types.NamespacedName{Name: "test-gateway-envtest", Namespace: "default"},
		&service)
	if err != nil {
		t.Errorf("expected service to be created: %v", err)
	} else {
		t.Logf("✓ Service created successfully")
	}

	// Verify ConfigMap was created
	var configMap corev1.ConfigMap
	err = testEnv.Client.Get(ctx,
		types.NamespacedName{Name: "test-gateway-envtest-vcl", Namespace: "default"},
		&configMap)
	if err != nil {
		t.Errorf("expected configmap to be created: %v", err)
	} else {
		t.Logf("✓ ConfigMap created successfully")
	}

	// Verify Secret was created
	var secret corev1.Secret
	err = testEnv.Client.Get(ctx,
		types.NamespacedName{Name: "test-gateway-envtest-secret", Namespace: "default"},
		&secret)
	if err != nil {
		t.Errorf("expected secret to be created: %v", err)
	} else {
		t.Logf("✓ Secret created successfully")
	}

	// Verify ServiceAccount was created
	var sa corev1.ServiceAccount
	err = testEnv.Client.Get(ctx,
		types.NamespacedName{Name: "test-gateway-envtest-chaperone", Namespace: "default"},
		&sa)
	if err != nil {
		t.Errorf("expected service account to be created: %v", err)
	} else {
		t.Logf("✓ ServiceAccount created successfully")
	}

	// Verify Gateway status was updated
	var updatedGateway gatewayv1.Gateway
	err = testEnv.Client.Get(ctx,
		types.NamespacedName{Name: "test-gateway-envtest", Namespace: "default"},
		&updatedGateway)
	if err != nil {
		t.Errorf("failed to get updated gateway: %v", err)
	} else {
		// Check if status conditions were set
		if len(updatedGateway.Status.Conditions) > 0 {
			t.Logf("✓ Gateway status updated with %d conditions", len(updatedGateway.Status.Conditions))
		}

		// Verify HTTP listener has ResolvedRefs condition
		if len(updatedGateway.Status.Listeners) > 0 {
			listener := updatedGateway.Status.Listeners[0]
			foundResolvedRefs := false
			for _, c := range listener.Conditions {
				if c.Type == string(gatewayv1.ListenerConditionResolvedRefs) {
					foundResolvedRefs = true
					if c.Status != metav1.ConditionTrue {
						t.Errorf("expected ResolvedRefs=True on HTTP listener, got %s", c.Status)
					}
				}
			}
			if !foundResolvedRefs {
				t.Error("expected ResolvedRefs condition on HTTP listener")
			} else {
				t.Log("✓ HTTP listener has ResolvedRefs condition")
			}
		}
	}

	// Cleanup resources
	_ = testEnv.Client.Delete(ctx, &deployment)
	_ = testEnv.Client.Delete(ctx, &service)
	_ = testEnv.Client.Delete(ctx, &configMap)
	_ = testEnv.Client.Delete(ctx, &secret)
	_ = testEnv.Client.Delete(ctx, &sa)
}

// TestHTTPSListener_MissingSecret_ProgrammedFalse tests that an HTTPS listener
// referencing a non-existent Secret gets Programmed: False and ResolvedRefs: False.
func TestHTTPSListener_MissingSecret_ProgrammedFalse(t *testing.T) {
	ctx := context.Background()

	// Create GatewayClass
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "varnish-tls-test",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(ControllerName),
		},
	}
	if err := testEnv.Client.Create(ctx, gatewayClass); err != nil {
		t.Fatalf("failed to create gatewayclass: %v", err)
	}
	defer func() {
		_ = testEnv.Client.Delete(ctx, gatewayClass)
	}()

	// Create Gateway with HTTPS listener referencing non-existent Secret
	tlsMode := gatewayv1.TLSModeTerminate
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw-tls-missing",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish-tls-test",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https",
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
					TLS: &gatewayv1.ListenerTLSConfig{
						Mode: &tlsMode,
						CertificateRefs: []gatewayv1.SecretObjectReference{
							{Name: "nonexistent-secret"},
						},
					},
				},
			},
		},
	}
	if err := testEnv.Client.Create(ctx, gateway); err != nil {
		t.Fatalf("failed to create gateway: %v", err)
	}
	defer func() {
		_ = testEnv.Client.Delete(ctx, gateway)
	}()

	// Create reconciler
	r := &GatewayReconciler{
		Client: testEnv.Client,
		Scheme: testEnv.Scheme,
		Config: Config{
			GatewayImage: "ghcr.io/varnish/gateway-chaperone:latest",
		},
		Logger: slog.Default(),
	}

	// Reconcile creates resources and sets status
	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-gw-tls-missing", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Fetch the updated gateway
	var updatedGateway gatewayv1.Gateway
	if err := testEnv.Client.Get(ctx, types.NamespacedName{Name: "test-gw-tls-missing", Namespace: "default"}, &updatedGateway); err != nil {
		t.Fatalf("failed to get updated gateway: %v", err)
	}

	if len(updatedGateway.Status.Listeners) == 0 {
		t.Fatal("expected listener statuses to be set")
	}

	listener := updatedGateway.Status.Listeners[0]

	// Check ResolvedRefs: False
	foundResolvedRefs := false
	for _, c := range listener.Conditions {
		if c.Type == string(gatewayv1.ListenerConditionResolvedRefs) {
			foundResolvedRefs = true
			if c.Status != metav1.ConditionFalse {
				t.Errorf("expected ResolvedRefs=False, got %s (reason: %s)", c.Status, c.Reason)
			}
		}
	}
	if !foundResolvedRefs {
		t.Error("expected ResolvedRefs condition on HTTPS listener")
	}

	// Check Programmed: False
	foundProgrammed := false
	for _, c := range listener.Conditions {
		if c.Type == string(gatewayv1.ListenerConditionProgrammed) {
			foundProgrammed = true
			if c.Status != metav1.ConditionFalse {
				t.Errorf("expected Programmed=False when ResolvedRefs=False, got %s (reason: %s)", c.Status, c.Reason)
			}
			if c.Reason != string(gatewayv1.ListenerReasonInvalid) {
				t.Errorf("expected Programmed reason=Invalid, got %s", c.Reason)
			}
		}
	}
	if !foundProgrammed {
		t.Error("expected Programmed condition on HTTPS listener")
	}

	// Cleanup
	_ = testEnv.Client.DeleteAllOf(ctx, &appsv1.Deployment{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.Service{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.ServiceAccount{}, client.InNamespace("default"))
}

// TestServiceShape_DefaultsToLoadBalancer_Envtest verifies that a Gateway
// with no GatewayClassParameters.spec.service config still gets a
// Type: LoadBalancer Service (backwards compatibility).
func TestServiceShape_DefaultsToLoadBalancer_Envtest(t *testing.T) {
	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "varnish-svc-default"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(ControllerName),
		},
	}
	if err := testEnv.Client.Create(ctx, gatewayClass); err != nil {
		t.Fatalf("create gatewayclass: %v", err)
	}
	defer func() { _ = testEnv.Client.Delete(ctx, gatewayClass) }()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-default-gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish-svc-default",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}
	if err := testEnv.Client.Create(ctx, gw); err != nil {
		t.Fatalf("create gateway: %v", err)
	}
	defer func() { _ = testEnv.Client.Delete(ctx, gw) }()

	r := NewEnvtestGatewayReconciler(testEnv)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	var svc corev1.Service
	if err := testEnv.Client.Get(ctx, types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}, &svc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("Type = %v, want LoadBalancer", svc.Spec.Type)
	}

	// Clean up reconciler-created resources in default namespace so they
	// don't bleed into subsequent tests in the suite. Matches the pattern
	// in TestHTTPSListener_MissingSecret_ProgrammedFalse.
	_ = testEnv.Client.DeleteAllOf(ctx, &appsv1.Deployment{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.Service{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.ServiceAccount{}, client.InNamespace("default"))
}

// TestServiceShape_TypeTransition_Envtest verifies LoadBalancer -> ClusterIP
// transitions persist correctly and don't lose other Service fields.
func TestServiceShape_TypeTransition_Envtest(t *testing.T) {
	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "varnish-svc-transition"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(ControllerName),
			ParametersRef: &gatewayv1.ParametersReference{
				Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
				Kind:  "GatewayClassParameters",
				Name:  "svc-transition-params",
			},
		},
	}
	if err := testEnv.Client.Create(ctx, gatewayClass); err != nil {
		t.Fatalf("create gatewayclass: %v", err)
	}
	defer func() { _ = testEnv.Client.Delete(ctx, gatewayClass) }()

	params := &gatewayparamsv1alpha1.GatewayClassParameters{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-transition-params"},
		Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
			Service: &gatewayparamsv1alpha1.ServiceConfig{Type: ptr(corev1.ServiceTypeLoadBalancer)},
		},
	}
	if err := testEnv.Client.Create(ctx, params); err != nil {
		t.Fatalf("create params: %v", err)
	}
	defer func() { _ = testEnv.Client.Delete(ctx, params) }()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-transition-gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish-svc-transition",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}
	if err := testEnv.Client.Create(ctx, gw); err != nil {
		t.Fatalf("create gateway: %v", err)
	}
	defer func() { _ = testEnv.Client.Delete(ctx, gw) }()

	r := NewEnvtestGatewayReconciler(testEnv)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	var svc corev1.Service
	if err := testEnv.Client.Get(ctx, types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}, &svc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Fatalf("initial Type = %v, want LoadBalancer", svc.Spec.Type)
	}
	originalPorts := append([]corev1.ServicePort{}, svc.Spec.Ports...)

	// Flip to ClusterIP.
	var p gatewayparamsv1alpha1.GatewayClassParameters
	if err := testEnv.Client.Get(ctx, types.NamespacedName{Name: "svc-transition-params"}, &p); err != nil {
		t.Fatalf("get params: %v", err)
	}
	p.Spec.Service.Type = ptr(corev1.ServiceTypeClusterIP)
	if err := testEnv.Client.Update(ctx, &p); err != nil {
		t.Fatalf("update params: %v", err)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if err := testEnv.Client.Get(ctx, types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}, &svc); err != nil {
		t.Fatalf("get service after transition: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("transitioned Type = %v, want ClusterIP", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != len(originalPorts) {
		t.Errorf("ports lost during transition: was %d, now %d", len(originalPorts), len(svc.Spec.Ports))
	}

	_ = testEnv.Client.DeleteAllOf(ctx, &appsv1.Deployment{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.Service{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.ServiceAccount{}, client.InNamespace("default"))
}

// TestServiceShape_CloudControllerAnnotationPreserved_Envtest verifies that
// annotations added directly to the Service (simulating a cloud controller)
// are not pruned by subsequent reconciles.
func TestServiceShape_CloudControllerAnnotationPreserved_Envtest(t *testing.T) {
	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "varnish-svc-cloud"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(ControllerName),
			ParametersRef: &gatewayv1.ParametersReference{
				Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
				Kind:  "GatewayClassParameters",
				Name:  "svc-cloud-params",
			},
		},
	}
	if err := testEnv.Client.Create(ctx, gatewayClass); err != nil {
		t.Fatalf("create gatewayclass: %v", err)
	}
	defer func() { _ = testEnv.Client.Delete(ctx, gatewayClass) }()

	params := &gatewayparamsv1alpha1.GatewayClassParameters{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-cloud-params"},
		Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
			Service: &gatewayparamsv1alpha1.ServiceConfig{
				Type:        ptr(corev1.ServiceTypeLoadBalancer),
				Annotations: map[string]string{"operator-managed": "v1"},
			},
		},
	}
	if err := testEnv.Client.Create(ctx, params); err != nil {
		t.Fatalf("create params: %v", err)
	}
	defer func() { _ = testEnv.Client.Delete(ctx, params) }()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-cloud-gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish-svc-cloud",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}
	if err := testEnv.Client.Create(ctx, gw); err != nil {
		t.Fatalf("create gateway: %v", err)
	}
	defer func() { _ = testEnv.Client.Delete(ctx, gw) }()

	r := NewEnvtestGatewayReconciler(testEnv)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// Simulate cloud controller adding an annotation directly.
	var svc corev1.Service
	if err := testEnv.Client.Get(ctx, types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}, &svc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations["cloud.example.com/lb-id"] = "lb-12345"
	if err := testEnv.Client.Update(ctx, &svc); err != nil {
		t.Fatalf("update svc with cloud annotation: %v", err)
	}

	// Trigger another reconcile (e.g. by touching the params).
	var p gatewayparamsv1alpha1.GatewayClassParameters
	if err := testEnv.Client.Get(ctx, types.NamespacedName{Name: "svc-cloud-params"}, &p); err != nil {
		t.Fatalf("get params: %v", err)
	}
	p.Spec.Service.Annotations["operator-managed"] = "v2" // force drift
	if err := testEnv.Client.Update(ctx, &p); err != nil {
		t.Fatalf("update params: %v", err)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if err := testEnv.Client.Get(ctx, types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}, &svc); err != nil {
		t.Fatalf("get service after second reconcile: %v", err)
	}

	if svc.Annotations["cloud.example.com/lb-id"] != "lb-12345" {
		t.Errorf("cloud-controller annotation pruned: %v", svc.Annotations)
	}
	if svc.Annotations["operator-managed"] != "v2" {
		t.Errorf("operator annotation not updated: %v", svc.Annotations)
	}

	_ = testEnv.Client.DeleteAllOf(ctx, &appsv1.Deployment{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.Service{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.ServiceAccount{}, client.InNamespace("default"))
}
