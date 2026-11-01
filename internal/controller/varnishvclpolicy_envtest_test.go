//go:build integration && !race

package controller

import (
	"context"
	"log/slog"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
)

// TestVarnishVCLPolicy_CRDAndStatus_Envtest checks that the generated CRD
// installs, enforces its schema, and accepts status writes through the status
// subresource.
func TestVarnishVCLPolicy_CRDAndStatus_Envtest(t *testing.T) {
	ctx := context.Background()

	vvp := &gatewayparamsv1alpha1.VarnishVCLPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "envtest-vvp", Namespace: "default"},
		Spec: gatewayparamsv1alpha1.VarnishVCLPolicySpec{
			TargetRef: gatewayparamsv1alpha1.PolicyTargetReference{
				Group: gatewayv1.GroupName, Kind: "HTTPRoute", Name: "does-not-exist",
			},
			VCLConfigMapRef: gatewayparamsv1alpha1.LocalConfigMapReference{Name: "route-vcl"},
		},
	}
	if err := testEnv.Client.Create(ctx, vvp); err != nil {
		t.Fatalf("create VarnishVCLPolicy: %v", err)
	}
	defer func() { _ = testEnv.Client.Delete(ctx, vvp) }()

	r := &VarnishVCLPolicyReconciler{Client: testEnv.Client, Scheme: testEnv.Scheme, Logger: slog.Default()}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vvp)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got gatewayparamsv1alpha1.VarnishVCLPolicy
	if err := testEnv.Client.Get(ctx, client.ObjectKeyFromObject(vvp), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Status.Ancestors) != 1 || len(got.Status.Ancestors[0].Conditions) != 2 {
		t.Fatalf("status = %+v, want one ancestor with Accepted and Programmed", got.Status)
	}
	c := got.Status.Ancestors[0].Conditions[0]
	if c.Type != "Accepted" || c.Status != metav1.ConditionFalse || c.Reason != gatewayparamsv1alpha1.VCLPolicyReasonTargetNotFound {
		t.Errorf("condition = %+v, want Accepted=False/TargetNotFound", c)
	}
	p := got.Status.Ancestors[0].Conditions[1]
	if p.Type != "Programmed" || p.Status != metav1.ConditionFalse || p.Reason != gatewayparamsv1alpha1.VCLPolicyReasonNotAccepted {
		t.Errorf("condition = %+v, want Programmed=False/NotAccepted", p)
	}

	// Schema: an empty ConfigMap name is rejected by the API server.
	bad := vvp.DeepCopy()
	bad.ObjectMeta = metav1.ObjectMeta{Name: "envtest-vvp-bad", Namespace: "default"}
	bad.Spec.VCLConfigMapRef.Name = ""
	if err := testEnv.Client.Create(ctx, bad); err == nil {
		_ = testEnv.Client.Delete(ctx, bad)
		t.Error("expected empty vclConfigMapRef.name to be rejected")
	}

	// Schema: slotPoolSize is bounded.
	params := &gatewayparamsv1alpha1.GatewayClassParameters{
		ObjectMeta: metav1.ObjectMeta{Name: "envtest-vvp-params"},
		Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
			RouteVCL: &gatewayparamsv1alpha1.RouteVCLConfig{Enabled: true, SlotPoolSize: ptr.To(int32(gatewayparamsv1alpha1.MaxRouteVCLSlotPoolSize + 1))},
		},
	}
	if err := testEnv.Client.Create(ctx, params); err == nil {
		_ = testEnv.Client.Delete(ctx, params)
		t.Error("expected slotPoolSize above the maximum to be rejected")
	}
}

// TestRouteVCL_DoesNotChangeInfraHash_Envtest verifies that toggling
// routeVCL in GatewayClassParameters is not a pod-restart change: the
// Deployment's infra-hash annotation stays the same.
func TestRouteVCL_DoesNotChangeInfraHash_Envtest(t *testing.T) {
	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "varnish-routevcl-hash"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(ControllerName),
			ParametersRef: &gatewayv1.ParametersReference{
				Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
				Kind:  "GatewayClassParameters",
				Name:  "routevcl-hash-params",
			},
		},
	}
	if err := testEnv.Client.Create(ctx, gatewayClass); err != nil {
		t.Fatalf("create gatewayclass: %v", err)
	}
	defer func() { _ = testEnv.Client.Delete(ctx, gatewayClass) }()

	params := &gatewayparamsv1alpha1.GatewayClassParameters{
		ObjectMeta: metav1.ObjectMeta{Name: "routevcl-hash-params"},
	}
	if err := testEnv.Client.Create(ctx, params); err != nil {
		t.Fatalf("create params: %v", err)
	}
	defer func() { _ = testEnv.Client.Delete(ctx, params) }()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "routevcl-hash-gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish-routevcl-hash",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}
	if err := testEnv.Client.Create(ctx, gw); err != nil {
		t.Fatalf("create gateway: %v", err)
	}
	defer func() { _ = testEnv.Client.Delete(ctx, gw) }()

	r := NewEnvtestGatewayReconciler(testEnv)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: gw.Name, Namespace: gw.Namespace}}
	hash := func() string {
		t.Helper()
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		var dep appsv1.Deployment
		if err := testEnv.Client.Get(ctx, req.NamespacedName, &dep); err != nil {
			t.Fatalf("get deployment: %v", err)
		}
		h := dep.Spec.Template.Annotations[AnnotationInfraHash]
		if h == "" {
			t.Fatal("deployment has no infra-hash annotation")
		}
		return h
	}

	before := hash()

	var p gatewayparamsv1alpha1.GatewayClassParameters
	if err := testEnv.Client.Get(ctx, types.NamespacedName{Name: params.Name}, &p); err != nil {
		t.Fatalf("get params: %v", err)
	}
	p.Spec.RouteVCL = &gatewayparamsv1alpha1.RouteVCLConfig{Enabled: true, SlotPoolSize: ptr.To(int32(64))}
	if err := testEnv.Client.Update(ctx, &p); err != nil {
		t.Fatalf("update params: %v", err)
	}

	if after := hash(); after != before {
		t.Errorf("infra hash changed after routeVCL change: %s -> %s", before, after)
	}

	_ = testEnv.Client.DeleteAllOf(ctx, &appsv1.Deployment{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.Service{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace("default"))
	_ = testEnv.Client.DeleteAllOf(ctx, &corev1.ServiceAccount{}, client.InNamespace("default"))
}
