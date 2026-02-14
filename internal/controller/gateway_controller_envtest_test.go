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
			ControllerName: "varnish.io/gateway-controller",
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

	// First reconcile adds finalizer
	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-gateway-envtest", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}
	if !result.Requeue {
		t.Error("expected requeue after adding finalizer")
	}

	// Wait a bit for API server to process
	time.Sleep(100 * time.Millisecond)

	// Second reconcile creates resources (using SSA)
	result, err = r.Reconcile(ctx, ctrl.Request{
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
			ControllerName: "varnish.io/gateway-controller",
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
					TLS: &gatewayv1.GatewayTLSConfig{
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

	// Create reconciler using the test GatewayClass name
	r := &GatewayReconciler{
		Client: testEnv.Client,
		Scheme: testEnv.Scheme,
		Config: Config{
			GatewayClassName: "varnish-tls-test",
			GatewayImage:     "ghcr.io/varnish/varnish-gateway:latest",
		},
		Logger: slog.Default(),
	}

	// First reconcile adds finalizer
	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-gw-tls-missing", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Second reconcile creates resources and sets status
	_, err = r.Reconcile(ctx, ctrl.Request{
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
