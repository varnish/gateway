package controller

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/varnish/gateway/internal/ghost"
	"github.com/varnish/gateway/internal/vcl"
)

func slotTestRoute(name, host string) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Name: "gw"}}},
			Hostnames:       []gatewayv1.Hostname{gatewayv1.Hostname(host)},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: ptr.To(gatewayv1.PortNumber(8080))},
				}}},
			}},
		},
	}
}

func routeVCLManifestCM(manifest string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gw-route-vcl", Namespace: "default",
			Labels: map[string]string{LabelManagedBy: ManagedByValue},
		},
		Data: map[string]string{vcl.ManifestKey: manifest},
	}
}

// vclSlotsByRoute parses routing.json and returns route name -> vcl_slot
// (nil when absent) for every route entry.
func vclSlotsByRoute(t *testing.T, routingJSON string) map[string]*int {
	t.Helper()
	var cfg ghost.RoutingConfig
	if err := json.Unmarshal([]byte(routingJSON), &cfg); err != nil {
		t.Fatalf("routing.json: %v", err)
	}
	out := map[string]*int{}
	for _, vh := range cfg.VHosts {
		for _, r := range vh.Routes {
			out[r.RouteName] = r.VCLSlot
		}
	}
	return out
}

func TestUpdateConfigMap_VCLSlotFromManifest(t *testing.T) {
	ctx := context.Background()
	gw := vvpGateway("gw", "varnish")
	r1, r2 := slotTestRoute("r1", "one.example.com"), slotTestRoute("r2", "two.example.com")
	objs := []runtime.Object{
		gw,
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-vcl", Namespace: "default"},
			Data:       map[string]string{"main.vcl": "vcl 4.1;", "routing.json": `{"version": 2, "vhosts": {}}`},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}},
		},
		routeVCLManifestCM(`{"slots":{"3":{"policy":"default/p1","route":"default/r1","generation":1,"hash":"h"}}}`),
	}
	r := newHTTPRouteTestReconciler(newTestScheme(), objs...)
	routes := []gatewayv1.HTTPRoute{*r1, *r2}

	if err := r.updateConfigMap(ctx, gw, routes); err != nil {
		t.Fatalf("updateConfigMap: %v", err)
	}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: "gw-vcl", Namespace: "default"}, &cm); err != nil {
		t.Fatal(err)
	}
	slots := vclSlotsByRoute(t, cm.Data["routing.json"])
	if s := slots["default/r1"]; s == nil || *s != 3 {
		t.Errorf("r1 vcl_slot = %v, want 3", s)
	}
	if s, ok := slots["default/r2"]; !ok || s != nil {
		t.Errorf("r2 vcl_slot = %v (present %v), want no slot", s, ok)
	}

	// The slot disappears from the manifest: routing.json loses vcl_slot.
	var routeCM corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: "gw-route-vcl", Namespace: "default"}, &routeCM); err != nil {
		t.Fatal(err)
	}
	routeCM.Data[vcl.ManifestKey] = `{"slots":{},"unassigned":[{"policy":"default/p1","route":"default/r1","reason":"NoSlotAvailable"}]}`
	if err := r.Update(ctx, &routeCM); err != nil {
		t.Fatal(err)
	}
	if err := r.updateConfigMap(ctx, gw, routes); err != nil {
		t.Fatalf("updateConfigMap: %v", err)
	}
	if err := r.Get(ctx, types.NamespacedName{Name: "gw-vcl", Namespace: "default"}, &cm); err != nil {
		t.Fatal(err)
	}
	if s := vclSlotsByRoute(t, cm.Data["routing.json"])["default/r1"]; s != nil {
		t.Errorf("r1 vcl_slot = %d after the slot was removed, want none", *s)
	}

	// No route VCL ConfigMap at all: no slots, no error.
	if err := r.Delete(ctx, &routeCM); err != nil {
		t.Fatal(err)
	}
	if err := r.updateConfigMap(ctx, gw, routes); err != nil {
		t.Fatalf("updateConfigMap without route VCL ConfigMap: %v", err)
	}
}

func TestUpdateConfigMap_BadManifestIsAnError(t *testing.T) {
	gw := vvpGateway("gw", "varnish")
	r := newHTTPRouteTestReconciler(newTestScheme(),
		gw,
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-vcl", Namespace: "default"},
			Data:       map[string]string{"main.vcl": "vcl 4.1;", "routing.json": `{"version": 2, "vhosts": {}}`},
		},
		routeVCLManifestCM("{not json"),
	)
	if err := r.updateConfigMap(context.Background(), gw, []gatewayv1.HTTPRoute{*slotTestRoute("r1", "one.example.com")}); err == nil {
		t.Error("updateConfigMap accepted an unparseable manifest")
	}
}

func TestFindHTTPRoutesForRouteVCLConfigMap(t *testing.T) {
	r := newHTTPRouteTestReconciler(newTestScheme())
	cm := routeVCLManifestCM(`{
  "slots": {"0": {"policy":"default/p1","route":"default/r1","generation":1,"hash":"h"},
            "1": {"policy":"team/p2","route":"team/r2","generation":1,"hash":"h"}},
  "unassigned": [{"policy":"default/p3","route":"default/r3","reason":"NoSlotAvailable"},
                 {"policy":"default/p4","route":"default/r1","reason":"NoSlotAvailable"}]
}`)
	got := map[string]bool{}
	for _, req := range r.findHTTPRoutesForRouteVCLConfigMap(context.Background(), cm) {
		got[req.String()] = true
	}
	want := []string{"default/r1", "team/r2", "default/r3"}
	if len(got) != len(want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing request %s (got %v)", w, got)
		}
	}

	// Other ConfigMaps map to nothing.
	other := routeVCLManifestCM(`{"slots":{"0":{"route":"default/r1"}}}`)
	other.Name = "gw-vcl"
	if reqs := r.findHTTPRoutesForRouteVCLConfigMap(context.Background(), other); len(reqs) != 0 {
		t.Errorf("non route-vcl ConfigMap mapped to %v", reqs)
	}
	if isRouteVCLConfigMap(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "user-route-vcl"}}) {
		t.Error("unlabelled ConfigMap treated as a route VCL ConfigMap")
	}
}
