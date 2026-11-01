//go:build integration && !race

package controller

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
	"github.com/varnish/gateway/internal/vcl"
)

// TestRouteVCL_EndToEnd_Envtest drives the operator side of route VCL
// against a real API server: VVP accepted -> Gateway reconcile writes the
// route VCL ConfigMap and a dispatching master -> HTTPRoute reconcile puts
// vcl_slot into routing.json -> a pod's podResults write flips Programmed.
func TestRouteVCL_EndToEnd_Envtest(t *testing.T) {
	ctx := context.Background()
	c := testEnv.Client
	ns := "default"

	create := func(obj client.Object) {
		t.Helper()
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
		}
		t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })
	}

	create(&gatewayparamsv1alpha1.GatewayClassParameters{
		ObjectMeta: metav1.ObjectMeta{Name: "rvcl-params"},
		Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
			RouteVCL: &gatewayparamsv1alpha1.RouteVCLConfig{Enabled: true, SlotPoolSize: ptr.To(int32(4))},
		},
	})
	create(&gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "varnish-rvcl"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(ControllerName),
			ParametersRef: &gatewayv1.ParametersReference{
				Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
				Kind:  "GatewayClassParameters",
				Name:  "rvcl-params",
			},
		},
	})
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "rvcl-gw", Namespace: ns},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish-rvcl",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}
	create(gw)
	create(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "rvcl-svc", Namespace: ns},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
	})
	create(&gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "rvcl-route", Namespace: ns},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "rvcl-gw"}}},
			Hostnames:       []gatewayv1.Hostname{"rvcl.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: "rvcl-svc", Port: ptr.To(gatewayv1.PortNumber(8080))},
				}}},
			}},
		},
	})
	routeVCL := "vcl 4.1;\nsub vcl_recv { set req.http.X-Route-VCL = \"yes\"; }\n"
	create(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "rvcl-doc", Namespace: ns},
		Data:       map[string]string{"route.vcl": routeVCL},
	})
	vvp := &gatewayparamsv1alpha1.VarnishVCLPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "rvcl-vvp", Namespace: ns},
		Spec: gatewayparamsv1alpha1.VarnishVCLPolicySpec{
			TargetRef: gatewayparamsv1alpha1.PolicyTargetReference{
				Group: gatewayv1.GroupName, Kind: "HTTPRoute", Name: "rvcl-route",
			},
			VCLConfigMapRef: gatewayparamsv1alpha1.LocalConfigMapReference{Name: "rvcl-doc"},
		},
	}
	create(vvp)
	t.Cleanup(func() {
		bg := context.Background()
		for _, name := range []string{"rvcl-gw-vcl", "rvcl-gw-route-vcl"} {
			_ = c.Delete(bg, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		}
	})

	vvpR := &VarnishVCLPolicyReconciler{Client: c, Scheme: testEnv.Scheme, Logger: slog.Default()}
	vvpReq := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(vvp)}
	gwR := NewEnvtestGatewayReconciler(testEnv)
	gwReq := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gw)}
	routeR := NewEnvtestHTTPRouteReconciler(testEnv)
	routeReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "rvcl-route", Namespace: ns}}

	// 1. VVP: Accepted.
	if _, err := vvpR.Reconcile(ctx, vvpReq); err != nil {
		t.Fatalf("vvp reconcile: %v", err)
	}
	cur := getEnvtestVVP(t, vvp)
	if !vvpAcceptedCurrent(cur) {
		t.Fatalf("policy not accepted: %+v", cur.Status)
	}

	// 2. Gateway: master with pool + route VCL ConfigMap.
	if _, err := gwR.Reconcile(ctx, gwReq); err != nil {
		t.Fatalf("gateway reconcile: %v", err)
	}
	var vclCM, routeCM corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Name: "rvcl-gw-vcl", Namespace: ns}, &vclCM); err != nil {
		t.Fatalf("get rvcl-gw-vcl: %v", err)
	}
	if vclCM.Data[vcl.PoolSizeKey] != "4" {
		t.Errorf("pool size = %q, want 4", vclCM.Data[vcl.PoolSizeKey])
	}
	if !strings.Contains(vclCM.Data[vcl.MainVCLKey], "return (vcl(gwvcl-0))") {
		t.Error("main.vcl does not dispatch to gwvcl-0")
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "rvcl-gw-route-vcl", Namespace: ns}, &routeCM); err != nil {
		t.Fatalf("get rvcl-gw-route-vcl: %v", err)
	}
	if ref := metav1.GetControllerOf(&routeCM); ref == nil || ref.Name != "rvcl-gw" {
		t.Errorf("route VCL ConfigMap controller = %+v", ref)
	}
	manifest, err := vcl.ParseRouteVCLManifest(routeCM.Data[vcl.ManifestKey])
	if err != nil {
		t.Fatal(err)
	}
	slot0, ok := manifest.Slots["0"]
	if !ok || slot0.Policy != "default/rvcl-vvp" || slot0.Route != "default/rvcl-route" {
		t.Fatalf("manifest = %+v", manifest)
	}
	doc := routeCM.Data[vcl.SlotKey(0)]
	if !strings.Contains(doc, "X-Route-VCL") || slot0.Hash != vcl.RouteVCLHash(doc) {
		t.Error("slot 0 document/hash mismatch")
	}

	// 3. HTTPRoute: routing.json carries the slot.
	if _, err := routeR.Reconcile(ctx, routeReq); err != nil {
		t.Fatalf("httproute reconcile: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "rvcl-gw-vcl", Namespace: ns}, &vclCM); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(vclCM.Data["routing.json"], `"vcl_slot": 0`) {
		t.Errorf("routing.json has no \"vcl_slot\": 0:\n%s", vclCM.Data["routing.json"])
	}
	if vclCM.Data[vcl.PoolSizeKey] != "4" || !strings.Contains(vclCM.Data[vcl.MainVCLKey], "gwvcl-0") {
		t.Error("HTTPRoute controller clobbered main.vcl or the pool size")
	}

	// 4. A gateway pod exists but has not reported: Pending, slot recorded.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "rvcl-gw-pod-a", Namespace: ns, Labels: gatewayLabels(gw)},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "varnish-gateway", Image: "example/test:1"}}},
	}
	create(pod)
	if _, err := vvpR.Reconcile(ctx, vvpReq); err != nil {
		t.Fatalf("vvp reconcile: %v", err)
	}
	cur = getEnvtestVVP(t, vvp)
	if p := envtestProgrammed(t, cur); p.Status != metav1.ConditionFalse || p.Reason != gatewayparamsv1alpha1.VCLPolicyReasonPending {
		t.Errorf("Programmed = %s/%s %q, want False/Pending", p.Status, p.Reason, p.Message)
	}
	if len(cur.Status.Slots) != 1 || cur.Status.Slots[0].Gateway != "default/rvcl-gw" || cur.Status.Slots[0].Slot != 0 {
		t.Errorf("status.slots = %+v", cur.Status.Slots)
	}

	// 5. The pod's chaperone reports success (plus a stale entry from a pod
	// that no longer exists): Programmed=True and the stale entry is GCed.
	cur.Status.PodResults = []gatewayparamsv1alpha1.VarnishVCLPolicyPodResult{
		{PodName: "rvcl-gw-pod-a", Slot: 0, Hash: slot0.Hash, Programmed: true, LastUpdate: metav1.Now()},
		{PodName: "rvcl-gw-pod-gone", Slot: 0, Hash: slot0.Hash, Programmed: true, LastUpdate: metav1.Now()},
	}
	if err := c.Status().Update(ctx, cur); err != nil {
		t.Fatalf("write podResults: %v", err)
	}
	if _, err := vvpR.Reconcile(ctx, vvpReq); err != nil {
		t.Fatalf("vvp reconcile: %v", err)
	}
	cur = getEnvtestVVP(t, vvp)
	if p := envtestProgrammed(t, cur); p.Status != metav1.ConditionTrue || p.Reason != gatewayparamsv1alpha1.VCLPolicyReasonProgrammed {
		t.Errorf("Programmed = %s/%s %q, want True/Programmed", p.Status, p.Reason, p.Message)
	}
	if len(cur.Status.PodResults) != 1 || cur.Status.PodResults[0].PodName != "rvcl-gw-pod-a" {
		t.Errorf("podResults = %+v, want only the live pod", cur.Status.PodResults)
	}
	if a := meta.FindStatusCondition(cur.Status.Ancestors[0].Conditions, gatewayparamsv1alpha1.VCLPolicyConditionAccepted); a == nil || a.Status != metav1.ConditionTrue {
		t.Errorf("Accepted lost: %+v", a)
	}
}

func getEnvtestVVP(t *testing.T, vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) *gatewayparamsv1alpha1.VarnishVCLPolicy {
	t.Helper()
	var out gatewayparamsv1alpha1.VarnishVCLPolicy
	if err := testEnv.Client.Get(context.Background(), client.ObjectKeyFromObject(vvp), &out); err != nil {
		t.Fatalf("get vvp: %v", err)
	}
	return &out
}

func envtestProgrammed(t *testing.T, vvp *gatewayparamsv1alpha1.VarnishVCLPolicy) metav1.Condition {
	t.Helper()
	if len(vvp.Status.Ancestors) == 0 {
		t.Fatal("no ancestors")
	}
	p := meta.FindStatusCondition(vvp.Status.Ancestors[0].Conditions, gatewayparamsv1alpha1.VCLPolicyConditionProgrammed)
	if p == nil {
		t.Fatal("no Programmed condition")
	}
	return *p
}
