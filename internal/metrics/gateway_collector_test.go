package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const testController = "varnish-software.com/gateway"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("gatewayv1.Install: %v", err)
	}
	return scheme
}

func acceptedCond(condType string, status metav1.ConditionStatus) metav1.Condition {
	return metav1.Condition{Type: condType, Status: status, Reason: "Test", LastTransitionTime: metav1.Now()}
}

func TestGatewayCollector(t *testing.T) {
	scheme := testScheme(t)

	objs := []client.Object{
		// Our GatewayClass and a foreign one.
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
			Spec:       gatewayv1.GatewayClassSpec{ControllerName: testController},
		},
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "other"},
			Spec:       gatewayv1.GatewayClassSpec{ControllerName: "someone/else"},
		},
		// gw1: ours, Accepted+Programmed, 2 listeners, 3 attached routes.
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: "varnish",
				Listeners: []gatewayv1.Listener{
					{Name: "l1", Protocol: gatewayv1.HTTPProtocolType, Port: 80},
					{Name: "l2", Protocol: gatewayv1.HTTPSProtocolType, Port: 443},
				},
			},
			Status: gatewayv1.GatewayStatus{
				Conditions: []metav1.Condition{
					acceptedCond(string(gatewayv1.GatewayConditionAccepted), metav1.ConditionTrue),
					acceptedCond(string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionTrue),
				},
				Listeners: []gatewayv1.ListenerStatus{
					{Name: "l1", AttachedRoutes: 3},
					{Name: "l2", AttachedRoutes: 0},
				},
			},
		},
		// gw2: ours, Accepted but not Programmed, 1 listener, 1 attached route.
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw2", Namespace: "prod"},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: "varnish",
				Listeners:        []gatewayv1.Listener{{Name: "l1", Protocol: gatewayv1.HTTPProtocolType, Port: 80}},
			},
			Status: gatewayv1.GatewayStatus{
				Conditions: []metav1.Condition{
					acceptedCond(string(gatewayv1.GatewayConditionAccepted), metav1.ConditionTrue),
					acceptedCond(string(gatewayv1.GatewayConditionProgrammed), metav1.ConditionFalse),
				},
				Listeners: []gatewayv1.ListenerStatus{{Name: "l1", AttachedRoutes: 1}},
			},
		},
		// gw3: foreign GatewayClass — must be excluded.
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw3", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "other"},
		},
		// r1: ours, Accepted.
		&gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "default"},
			Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
				ParentRef:      gatewayv1.ParentReference{Name: "gw1"},
				ControllerName: testController,
				Conditions:     []metav1.Condition{acceptedCond(string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue)},
			}}}},
		},
		// r2: ours, not Accepted.
		&gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: "default"},
			Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
				ParentRef:      gatewayv1.ParentReference{Name: "gw1"},
				ControllerName: testController,
				Conditions:     []metav1.Condition{acceptedCond(string(gatewayv1.RouteConditionAccepted), metav1.ConditionFalse)},
			}}}},
		},
		// r3: reconciled by a foreign controller — must be excluded.
		&gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "r3", Namespace: "default"},
			Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
				ParentRef:      gatewayv1.ParentReference{Name: "somegw"},
				ControllerName: "someone/else",
				Conditions:     []metav1.Condition{acceptedCond(string(gatewayv1.RouteConditionAccepted), metav1.ConditionTrue)},
			}}}},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	reg := prometheus.NewRegistry()
	RegisterGatewayMetrics(reg, c, testController, "v1.2.3", nil)

	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	checks := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"varnish_gateway_info", map[string]string{"version": "v1.2.3"}, 1},
		{"varnish_gateway_gateways", map[string]string{"gatewayclass": "varnish"}, 2},
		{"varnish_gateway_gateway_accepted", map[string]string{"namespace": "default", "name": "gw1"}, 1},
		{"varnish_gateway_gateway_programmed", map[string]string{"namespace": "default", "name": "gw1"}, 1},
		{"varnish_gateway_gateway_programmed", map[string]string{"namespace": "prod", "name": "gw2"}, 0},
		{"varnish_gateway_gateway_listeners", map[string]string{"namespace": "default", "name": "gw1"}, 2},
		{"varnish_gateway_gateway_attached_routes", map[string]string{"namespace": "default", "name": "gw1"}, 3},
		{"varnish_gateway_gateway_attached_routes", map[string]string{"namespace": "prod", "name": "gw2"}, 1},
		{"varnish_gateway_httproutes", map[string]string{}, 2},
		{"varnish_gateway_httproutes_accepted", map[string]string{}, 1},
	}

	for _, tc := range checks {
		got, ok := findGauge(fams, tc.name, tc.labels)
		if !ok {
			t.Errorf("%s%v: metric not found", tc.name, tc.labels)
			continue
		}
		if got != tc.want {
			t.Errorf("%s%v = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}

	// The foreign GatewayClass must not appear.
	if _, ok := findGauge(fams, "varnish_gateway_gateways", map[string]string{"gatewayclass": "other"}); ok {
		t.Error("varnish_gateway_gateways emitted a series for a foreign GatewayClass")
	}
	// gw3 (foreign class) must not produce per-Gateway series.
	if _, ok := findGauge(fams, "varnish_gateway_gateway_accepted", map[string]string{"namespace": "default", "name": "gw3"}); ok {
		t.Error("gw3 (foreign GatewayClass) produced a per-Gateway series")
	}
}

// findGauge returns the value of the gauge named name whose label set exactly
// equals labels, and whether such a series exists.
func findGauge(fams []*dto.MetricFamily, name string, labels map[string]string) (float64, bool) {
	for _, fam := range fams {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			if labelsEqual(m, labels) {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func labelsEqual(m *dto.Metric, want map[string]string) bool {
	if len(m.GetLabel()) != len(want) {
		return false
	}
	for _, lp := range m.GetLabel() {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}
