package controller

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
)

const vvpTestNS = "default"

func newVVPTestReconciler(objs ...runtime.Object) *VarnishVCLPolicyReconciler {
	scheme := newTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&gatewayparamsv1alpha1.VarnishVCLPolicy{}).
		Build()
	return &VarnishVCLPolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Logger: slog.Default(),
	}
}

func newVVP(name, routeName, cmName string, created time.Time) *gatewayparamsv1alpha1.VarnishVCLPolicy {
	return &gatewayparamsv1alpha1.VarnishVCLPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         vvpTestNS,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: gatewayparamsv1alpha1.VarnishVCLPolicySpec{
			TargetRef: gatewayparamsv1alpha1.PolicyTargetReference{
				Group: gatewayv1.GroupName,
				Kind:  "HTTPRoute",
				Name:  routeName,
			},
			VCLConfigMapRef: gatewayparamsv1alpha1.LocalConfigMapReference{Name: cmName},
		},
	}
}

// vvpGatewayClass returns a GatewayClass managed by us pointing at paramsName
// (no parametersRef when paramsName is empty).
func vvpGatewayClass(name, paramsName string) *gatewayv1.GatewayClass {
	gc := newTestGatewayClass(name)
	if paramsName != "" {
		gc.Spec.ParametersRef = &gatewayv1.ParametersReference{
			Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
			Kind:  "GatewayClassParameters",
			Name:  paramsName,
		}
	}
	return gc
}

func vvpParams(name string, routeVCL *gatewayparamsv1alpha1.RouteVCLConfig) *gatewayparamsv1alpha1.GatewayClassParameters {
	return &gatewayparamsv1alpha1.GatewayClassParameters{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       gatewayparamsv1alpha1.GatewayClassParametersSpec{RouteVCL: routeVCL},
	}
}

func vvpGateway(name, className string) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: vvpTestNS},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(className),
			Listeners: []gatewayv1.Listener{{
				Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
			}},
		},
	}
}

func vvpRoute(name string, parents ...string) *gatewayv1.HTTPRoute {
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: vvpTestNS},
	}
	for _, p := range parents {
		route.Spec.ParentRefs = append(route.Spec.ParentRefs, gatewayv1.ParentReference{Name: gatewayv1.ObjectName(p)})
	}
	return route
}

func vvpConfigMap(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: vvpTestNS},
		Data:       data,
	}
}

// vvpEnabledWorld returns a Gateway "gw" of class "varnish" whose parameters
// enable route VCL, a route "my-route" attached to it, and a ConfigMap "vcl"
// holding a document under the default key.
func vvpEnabledWorld() []runtime.Object {
	return []runtime.Object{
		vvpGatewayClass("varnish", "params"),
		vvpParams("params", &gatewayparamsv1alpha1.RouteVCLConfig{Enabled: true}),
		vvpGateway("gw", "varnish"),
		vvpRoute("my-route", "gw"),
		vvpConfigMap("vcl", map[string]string{"route.vcl": "sub vcl_recv { }"}),
	}
}

func reconcileVVP(t *testing.T, r *VarnishVCLPolicyReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: vvpTestNS},
	})
	if err != nil {
		t.Fatalf("Reconcile(%s): %v", name, err)
	}
	return res
}

func getVVPAccepted(t *testing.T, r *VarnishVCLPolicyReconciler, name string) metav1.Condition {
	t.Helper()
	var vvp gatewayparamsv1alpha1.VarnishVCLPolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: vvpTestNS}, &vvp); err != nil {
		t.Fatalf("Get(%s): %v", name, err)
	}
	if len(vvp.Status.Ancestors) != 1 {
		t.Fatalf("%s: got %d ancestors, want 1", name, len(vvp.Status.Ancestors))
	}
	anc := vvp.Status.Ancestors[0]
	if anc.ControllerName != ControllerName {
		t.Errorf("%s: controllerName = %q, want %q", name, anc.ControllerName, ControllerName)
	}
	for _, c := range anc.Conditions {
		if c.Type == gatewayparamsv1alpha1.VCLPolicyConditionProgrammed {
			t.Errorf("%s: Programmed must not be set in this phase", name)
		}
	}
	for _, c := range anc.Conditions {
		if c.Type == gatewayparamsv1alpha1.VCLPolicyConditionAccepted {
			return c
		}
	}
	t.Fatalf("%s: no Accepted condition", name)
	return metav1.Condition{}
}

func assertVVPAccepted(t *testing.T, r *VarnishVCLPolicyReconciler, name string, want metav1.ConditionStatus, reason string) metav1.Condition {
	t.Helper()
	c := getVVPAccepted(t, r, name)
	if c.Status != want || c.Reason != reason {
		t.Errorf("%s: Accepted = %s/%s (%q), want %s/%s", name, c.Status, c.Reason, c.Message, want, reason)
	}
	return c
}

func TestVVPReconcile_Accepted(t *testing.T) {
	now := time.Now()
	objs := append(vvpEnabledWorld(), newVVP("p", "my-route", "vcl", now))
	r := newVVPTestReconciler(objs...)

	res := reconcileVVP(t, r, "p")
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0", res.RequeueAfter)
	}
	c := assertVVPAccepted(t, r, "p", metav1.ConditionTrue, gatewayparamsv1alpha1.VCLPolicyReasonAccepted)
	if c.Message != "Policy accepted" {
		t.Errorf("message = %q, want plain acceptance", c.Message)
	}
}

func TestVVPReconcile_CustomKey(t *testing.T) {
	vvp := newVVP("p", "my-route", "vcl", time.Now())
	vvp.Spec.VCLConfigMapRef.Key = "custom.vcl"
	objs := append(vvpEnabledWorld()[:4],
		vvpConfigMap("vcl", map[string]string{"custom.vcl": "sub vcl_recv { }"}),
		vvp)
	r := newVVPTestReconciler(objs...)

	reconcileVVP(t, r, "p")
	assertVVPAccepted(t, r, "p", metav1.ConditionTrue, gatewayparamsv1alpha1.VCLPolicyReasonAccepted)
}

func TestVVPReconcile_Disabled(t *testing.T) {
	tests := []struct {
		name     string
		gc       *gatewayv1.GatewayClass
		params   *gatewayparamsv1alpha1.GatewayClassParameters
		wantFrag string
	}{
		{
			name:     "class without parametersRef",
			gc:       vvpGatewayClass("varnish", ""),
			wantFrag: `GatewayClass "varnish" does not set routeVCL.enabled=true`,
		},
		{
			name:     "parameters without routeVCL",
			gc:       vvpGatewayClass("varnish", "params"),
			params:   vvpParams("params", nil),
			wantFrag: `GatewayClass "varnish" does not set routeVCL.enabled=true`,
		},
		{
			name:     "routeVCL.enabled=false",
			gc:       vvpGatewayClass("varnish", "params"),
			params:   vvpParams("params", &gatewayparamsv1alpha1.RouteVCLConfig{Enabled: false}),
			wantFrag: `GatewayClass "varnish" does not set routeVCL.enabled=true`,
		},
		{
			name:     "parametersRef points at missing object",
			gc:       vvpGatewayClass("varnish", "missing"),
			wantFrag: `GatewayClass "varnish" does not set routeVCL.enabled=true`,
		},
		{
			name: "class managed by another controller",
			gc: func() *gatewayv1.GatewayClass {
				gc := vvpGatewayClass("varnish", "params")
				gc.Spec.ControllerName = "example.com/other"
				return gc
			}(),
			params:   vvpParams("params", &gatewayparamsv1alpha1.RouteVCLConfig{Enabled: true}),
			wantFrag: "is not managed by",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []runtime.Object{
				tt.gc,
				vvpGateway("gw", "varnish"),
				vvpRoute("my-route", "gw"),
				vvpConfigMap("vcl", map[string]string{"route.vcl": "sub vcl_recv { }"}),
				newVVP("p", "my-route", "vcl", time.Now()),
			}
			if tt.params != nil {
				objs = append(objs, tt.params)
			}
			r := newVVPTestReconciler(objs...)
			reconcileVVP(t, r, "p")
			c := assertVVPAccepted(t, r, "p", metav1.ConditionFalse, gatewayparamsv1alpha1.VCLPolicyReasonDisabled)
			if !strings.Contains(c.Message, tt.wantFrag) {
				t.Errorf("message %q does not contain %q", c.Message, tt.wantFrag)
			}
		})
	}
}

func TestVVPReconcile_DisabledEdgeCases(t *testing.T) {
	t.Run("route with no parents", func(t *testing.T) {
		objs := append(vvpEnabledWorld()[:3], vvpRoute("my-route"),
			vvpConfigMap("vcl", map[string]string{"route.vcl": "x"}),
			newVVP("p", "my-route", "vcl", time.Now()))
		r := newVVPTestReconciler(objs...)
		reconcileVVP(t, r, "p")
		c := assertVVPAccepted(t, r, "p", metav1.ConditionFalse, gatewayparamsv1alpha1.VCLPolicyReasonDisabled)
		if !strings.Contains(c.Message, "no parent Gateway") {
			t.Errorf("message = %q", c.Message)
		}
	})
	t.Run("parent Gateway missing", func(t *testing.T) {
		objs := append(vvpEnabledWorld()[:2], vvpRoute("my-route", "nope"),
			vvpConfigMap("vcl", map[string]string{"route.vcl": "x"}),
			newVVP("p", "my-route", "vcl", time.Now()))
		r := newVVPTestReconciler(objs...)
		reconcileVVP(t, r, "p")
		c := assertVVPAccepted(t, r, "p", metav1.ConditionFalse, gatewayparamsv1alpha1.VCLPolicyReasonDisabled)
		if !strings.Contains(c.Message, "Gateway default/nope: Gateway not found") {
			t.Errorf("message = %q", c.Message)
		}
	})
}

// A route with one enabled and one disabled parent is accepted, and the
// message names the disabled one.
func TestVVPReconcile_MixedParents(t *testing.T) {
	objs := []runtime.Object{
		vvpGatewayClass("varnish", "params"),
		vvpParams("params", &gatewayparamsv1alpha1.RouteVCLConfig{Enabled: true}),
		vvpGatewayClass("locked", "locked-params"),
		vvpParams("locked-params", nil),
		vvpGateway("gw", "varnish"),
		vvpGateway("gw-locked", "locked"),
		vvpRoute("my-route", "gw-locked", "gw"),
		vvpConfigMap("vcl", map[string]string{"route.vcl": "x"}),
		newVVP("p", "my-route", "vcl", time.Now()),
	}
	r := newVVPTestReconciler(objs...)
	reconcileVVP(t, r, "p")
	c := assertVVPAccepted(t, r, "p", metav1.ConditionTrue, gatewayparamsv1alpha1.VCLPolicyReasonAccepted)
	if !strings.Contains(c.Message, `Gateway default/gw-locked: GatewayClass "locked"`) {
		t.Errorf("message %q does not name the disabled parent", c.Message)
	}
}

func TestVVPReconcile_Invalid(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*gatewayparamsv1alpha1.VarnishVCLPolicy)
		cm       *corev1.ConfigMap
		wantFrag string
	}{
		{
			name:     "sectionName set",
			mutate:   func(p *gatewayparamsv1alpha1.VarnishVCLPolicy) { p.Spec.TargetRef.SectionName = stringPtr("rule-a") },
			wantFrag: "sectionName",
		},
		{
			name:     "wrong kind",
			mutate:   func(p *gatewayparamsv1alpha1.VarnishVCLPolicy) { p.Spec.TargetRef.Kind = "Gateway" },
			wantFrag: "targetRef.kind",
		},
		{
			name:     "wrong group",
			mutate:   func(p *gatewayparamsv1alpha1.VarnishVCLPolicy) { p.Spec.TargetRef.Group = "example.com" },
			wantFrag: "targetRef.group",
		},
		{
			name:     "empty configmap name",
			mutate:   func(p *gatewayparamsv1alpha1.VarnishVCLPolicy) { p.Spec.VCLConfigMapRef.Name = "" },
			wantFrag: "vclConfigMapRef.name",
		},
		{
			name: "oversize document",
			cm: vvpConfigMap("vcl", map[string]string{
				"route.vcl": strings.Repeat("#", gatewayparamsv1alpha1.MaxRouteVCLDocumentBytes+1),
			}),
			wantFrag: "larger than",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vvp := newVVP("p", "my-route", "vcl", time.Now())
			if tt.mutate != nil {
				tt.mutate(vvp)
			}
			objs := vvpEnabledWorld()
			if tt.cm != nil {
				objs = append(objs[:4], tt.cm)
			}
			r := newVVPTestReconciler(append(objs, vvp)...)
			res := reconcileVVP(t, r, "p")
			if res.RequeueAfter != 0 {
				t.Errorf("RequeueAfter = %v, want 0 for Invalid", res.RequeueAfter)
			}
			c := assertVVPAccepted(t, r, "p", metav1.ConditionFalse, gatewayparamsv1alpha1.VCLPolicyReasonInvalid)
			if !strings.Contains(c.Message, tt.wantFrag) {
				t.Errorf("message %q does not contain %q", c.Message, tt.wantFrag)
			}
		})
	}
}

// A document of exactly the limit is accepted.
func TestVVPReconcile_DocumentAtLimit(t *testing.T) {
	objs := append(vvpEnabledWorld()[:4],
		vvpConfigMap("vcl", map[string]string{
			"route.vcl": strings.Repeat("#", gatewayparamsv1alpha1.MaxRouteVCLDocumentBytes),
		}),
		newVVP("p", "my-route", "vcl", time.Now()))
	r := newVVPTestReconciler(objs...)
	reconcileVVP(t, r, "p")
	assertVVPAccepted(t, r, "p", metav1.ConditionTrue, gatewayparamsv1alpha1.VCLPolicyReasonAccepted)
}

func TestVVPReconcile_TargetNotFound(t *testing.T) {
	objs := append(vvpEnabledWorld(), newVVP("p", "missing-route", "vcl", time.Now()))
	r := newVVPTestReconciler(objs...)

	res := reconcileVVP(t, r, "p")
	if res.RequeueAfter != 10*time.Second {
		t.Errorf("RequeueAfter = %v, want 10s", res.RequeueAfter)
	}
	assertVVPAccepted(t, r, "p", metav1.ConditionFalse, gatewayparamsv1alpha1.VCLPolicyReasonTargetNotFound)
}

func TestVVPReconcile_ConfigMapNotFound(t *testing.T) {
	tests := []struct {
		name     string
		cm       *corev1.ConfigMap
		key      string
		wantFrag string
	}{
		{
			name:     "configmap missing",
			wantFrag: `ConfigMap "vcl" not found`,
		},
		{
			name:     "default key missing",
			cm:       vvpConfigMap("vcl", map[string]string{"user.vcl": "x"}),
			wantFrag: `no key "route.vcl"`,
		},
		{
			name:     "custom key missing",
			cm:       vvpConfigMap("vcl", map[string]string{"route.vcl": "x"}),
			key:      "other.vcl",
			wantFrag: `no key "other.vcl"`,
		},
		{
			name: "configmap in another namespace is not used",
			cm: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "vcl", Namespace: "elsewhere"},
				Data:       map[string]string{"route.vcl": "x"},
			},
			wantFrag: `ConfigMap "vcl" not found in namespace "default"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vvp := newVVP("p", "my-route", "vcl", time.Now())
			vvp.Spec.VCLConfigMapRef.Key = tt.key
			objs := append(vvpEnabledWorld()[:4], vvp)
			if tt.cm != nil {
				objs = append(objs, tt.cm)
			}
			r := newVVPTestReconciler(objs...)
			res := reconcileVVP(t, r, "p")
			if res.RequeueAfter != 10*time.Second {
				t.Errorf("RequeueAfter = %v, want 10s", res.RequeueAfter)
			}
			c := assertVVPAccepted(t, r, "p", metav1.ConditionFalse, gatewayparamsv1alpha1.VCLPolicyReasonConfigMapNotFound)
			if !strings.Contains(c.Message, tt.wantFrag) {
				t.Errorf("message %q does not contain %q", c.Message, tt.wantFrag)
			}
		})
	}
}

func TestVVPReconcile_ConflictOldestWinsAndPromotion(t *testing.T) {
	now := time.Now()
	older := newVVP("older", "my-route", "vcl", now.Add(-time.Minute))
	newer := newVVP("newer", "my-route", "vcl", now)
	r := newVVPTestReconciler(append(vvpEnabledWorld(), older, newer)...)
	ctx := context.Background()

	reconcileVVP(t, r, "newer")
	reconcileVVP(t, r, "older")
	assertVVPAccepted(t, r, "newer", metav1.ConditionFalse, gatewayparamsv1alpha1.VCLPolicyReasonConflicted)
	assertVVPAccepted(t, r, "older", metav1.ConditionTrue, gatewayparamsv1alpha1.VCLPolicyReasonAccepted)

	// The winner's delete event maps to its same-target siblings, whose own
	// reconcile then promotes them.
	if got := requestNames(t, r.findSiblingVVPs(ctx, older)); len(got) != 1 || got[0] != vvpTestNS+"/newer" {
		t.Fatalf("findSiblingVVPs(older) = %v, want [%s/newer]", got, vvpTestNS)
	}
	if err := r.Delete(ctx, older); err != nil {
		t.Fatalf("Delete(older): %v", err)
	}
	reconcileVVP(t, r, "older") // NotFound path is a no-op.
	reconcileVVP(t, r, "newer")
	assertVVPAccepted(t, r, "newer", metav1.ConditionTrue, gatewayparamsv1alpha1.VCLPolicyReasonAccepted)
}

// Siblings are matched on the full target (group, kind, name) within the
// namespace, never across namespaces, and never the policy itself.
func TestFindSiblingVVPs(t *testing.T) {
	now := time.Now()
	self := newVVP("self", "my-route", "vcl", now)
	same := newVVP("same", "my-route", "vcl", now)
	otherRoute := newVVP("other-route", "other", "vcl", now)
	otherNS := newVVP("other-ns", "my-route", "vcl", now)
	otherNS.Namespace = "other"
	r := newVVPTestReconciler(append(vvpEnabledWorld(), self, same, otherRoute, otherNS)...)

	got := requestNames(t, r.findSiblingVVPs(context.Background(), self))
	if len(got) != 1 || got[0] != vvpTestNS+"/same" {
		t.Errorf("findSiblingVVPs = %v, want [%s/same]", got, vvpTestNS)
	}
}

// Equal creation timestamps fall back to name order, as for VCP.
func TestVVPReconcile_ConflictTiebreakByName(t *testing.T) {
	now := time.Now()
	r := newVVPTestReconciler(append(vvpEnabledWorld(),
		newVVP("b", "my-route", "vcl", now),
		newVVP("a", "my-route", "vcl", now))...)

	reconcileVVP(t, r, "b")
	reconcileVVP(t, r, "a")
	assertVVPAccepted(t, r, "a", metav1.ConditionTrue, gatewayparamsv1alpha1.VCLPolicyReasonAccepted)
	assertVVPAccepted(t, r, "b", metav1.ConditionFalse, gatewayparamsv1alpha1.VCLPolicyReasonConflicted)
}

// A second reconcile with nothing changed must not write status again.
func TestVVPReconcile_NoOpWriteSkipped(t *testing.T) {
	r := newVVPTestReconciler(append(vvpEnabledWorld(), newVVP("p", "my-route", "vcl", time.Now()))...)
	ctx := context.Background()

	reconcileVVP(t, r, "p")
	var first gatewayparamsv1alpha1.VarnishVCLPolicy
	if err := r.Get(ctx, types.NamespacedName{Name: "p", Namespace: vvpTestNS}, &first); err != nil {
		t.Fatal(err)
	}
	reconcileVVP(t, r, "p")
	var second gatewayparamsv1alpha1.VarnishVCLPolicy
	if err := r.Get(ctx, types.NamespacedName{Name: "p", Namespace: vvpTestNS}, &second); err != nil {
		t.Fatal(err)
	}
	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("resourceVersion changed %s -> %s on a no-op reconcile", first.ResourceVersion, second.ResourceVersion)
	}
}

// Flipping enablement changes the condition on the next reconcile.
func TestVVPReconcile_EnablementFlip(t *testing.T) {
	params := vvpParams("params", nil)
	objs := []runtime.Object{
		vvpGatewayClass("varnish", "params"), params,
		vvpGateway("gw", "varnish"), vvpRoute("my-route", "gw"),
		vvpConfigMap("vcl", map[string]string{"route.vcl": "x"}),
		newVVP("p", "my-route", "vcl", time.Now()),
	}
	r := newVVPTestReconciler(objs...)
	ctx := context.Background()

	reconcileVVP(t, r, "p")
	assertVVPAccepted(t, r, "p", metav1.ConditionFalse, gatewayparamsv1alpha1.VCLPolicyReasonDisabled)

	var p gatewayparamsv1alpha1.GatewayClassParameters
	if err := r.Get(ctx, types.NamespacedName{Name: "params"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.RouteVCL = &gatewayparamsv1alpha1.RouteVCLConfig{Enabled: true}
	if err := r.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	if reqs := r.findAllVVPs(ctx, &p); len(reqs) != 1 || reqs[0].Name != "p" {
		t.Fatalf("findAllVVPs = %v, want [p]", reqs)
	}
	reconcileVVP(t, r, "p")
	assertVVPAccepted(t, r, "p", metav1.ConditionTrue, gatewayparamsv1alpha1.VCLPolicyReasonAccepted)
}

func TestVVPReconcile_NotFoundNoError(t *testing.T) {
	r := newVVPTestReconciler()
	res := reconcileVVP(t, r, "gone")
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0", res.RequeueAfter)
	}
}

func requestNames(t *testing.T, reqs []ctrl.Request) []string {
	t.Helper()
	var names []string
	for _, r := range reqs {
		names = append(names, r.Namespace+"/"+r.Name)
	}
	return names
}

func TestFindVVPsForHTTPRoute(t *testing.T) {
	now := time.Now()
	other := newVVP("in-other-ns", "my-route", "vcl", now)
	other.Namespace = "other"
	r := newVVPTestReconciler(
		newVVP("match", "my-route", "vcl", now),
		newVVP("other-route", "another-route", "vcl", now),
		other,
	)

	got := requestNames(t, r.findVVPsForHTTPRoute(context.Background(), vvpRoute("my-route")))
	if len(got) != 1 || got[0] != "default/match" {
		t.Errorf("findVVPsForHTTPRoute = %v, want [default/match]", got)
	}
	if reqs := r.findVVPsForHTTPRoute(context.Background(), vvpConfigMap("x", nil)); reqs != nil {
		t.Errorf("wrong type: got %v, want nil", reqs)
	}
}

func TestFindVVPsForConfigMap(t *testing.T) {
	now := time.Now()
	other := newVVP("in-other-ns", "my-route", "vcl", now)
	other.Namespace = "other"
	r := newVVPTestReconciler(
		newVVP("match-a", "route-a", "vcl", now),
		newVVP("match-b", "route-b", "vcl", now),
		newVVP("different-cm", "route-c", "not-vcl", now),
		other,
	)

	got := requestNames(t, r.findVVPsForConfigMap(context.Background(), vvpConfigMap("vcl", nil)))
	if len(got) != 2 || got[0] != "default/match-a" || got[1] != "default/match-b" {
		t.Errorf("findVVPsForConfigMap = %v, want [default/match-a default/match-b]", got)
	}
	if reqs := r.findVVPsForConfigMap(context.Background(), vvpRoute("x")); reqs != nil {
		t.Errorf("wrong type: got %v, want nil", reqs)
	}
}

func TestFindVVPsForGateway(t *testing.T) {
	now := time.Now()
	crossNSRoute := vvpRoute("cross")
	crossNSRoute.Namespace = "apps"
	gwNS := gatewayv1.Namespace(vvpTestNS)
	crossNSRoute.Spec.ParentRefs = []gatewayv1.ParentReference{{Name: "gw", Namespace: &gwNS}}
	crossNSPolicy := newVVP("cross", "cross", "vcl", now)
	crossNSPolicy.Namespace = "apps"

	r := newVVPTestReconciler(
		vvpRoute("attached", "gw"),
		vvpRoute("elsewhere", "other-gw"),
		crossNSRoute,
		newVVP("attached", "attached", "vcl", now),
		newVVP("elsewhere", "elsewhere", "vcl", now),
		newVVP("no-route", "missing", "vcl", now),
		crossNSPolicy,
	)

	got := requestNames(t, r.findVVPsForGateway(context.Background(), vvpGateway("gw", "varnish")))
	want := map[string]bool{"default/attached": true, "apps/cross": true}
	if len(got) != len(want) {
		t.Fatalf("findVVPsForGateway = %v, want %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected request %s", n)
		}
	}
}
