package v1alpha1

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGatewayClassParametersDeepCopy_RouteVCL(t *testing.T) {
	size := int32(32)
	in := &GatewayClassParameters{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: GatewayClassParametersSpec{
			RouteVCL: &RouteVCLConfig{Enabled: true, SlotPoolSize: &size},
		},
	}

	out := in.DeepCopy()
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("DeepCopy() = %+v, want %+v", out, in)
	}
	if out.Spec.RouteVCL == in.Spec.RouteVCL {
		t.Fatal("RouteVCL pointer shared between copies")
	}
	if out.Spec.RouteVCL.SlotPoolSize == in.Spec.RouteVCL.SlotPoolSize {
		t.Fatal("SlotPoolSize pointer shared between copies")
	}

	*out.Spec.RouteVCL.SlotPoolSize = 64
	out.Spec.RouteVCL.Enabled = false
	if *in.Spec.RouteVCL.SlotPoolSize != 32 || !in.Spec.RouteVCL.Enabled {
		t.Errorf("mutating the copy changed the original: %+v", in.Spec.RouteVCL)
	}
}

func TestVarnishVCLPolicyDeepCopy(t *testing.T) {
	section := "rule"
	in := &VarnishVCLPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		Spec: VarnishVCLPolicySpec{
			TargetRef:       PolicyTargetReference{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Name: "r", SectionName: &section},
			VCLConfigMapRef: LocalConfigMapReference{Name: "cm", Key: "k"},
		},
		Status: VarnishVCLPolicyStatus{Ancestors: []VarnishVCLPolicyAncestorStatus{{
			AncestorRef:    PolicyTargetReference{Kind: "HTTPRoute", Name: "r", SectionName: &section},
			ControllerName: "c",
			Conditions:     []metav1.Condition{{Type: VCLPolicyConditionAccepted, Status: metav1.ConditionTrue}},
		}}},
	}

	out, ok := in.DeepCopyObject().(*VarnishVCLPolicy)
	if !ok {
		t.Fatalf("DeepCopyObject() returned %T", in.DeepCopyObject())
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("DeepCopyObject() = %+v, want %+v", out, in)
	}
	if out.Spec.TargetRef.SectionName == in.Spec.TargetRef.SectionName {
		t.Error("spec.targetRef.sectionName pointer shared between copies")
	}
	if out.Status.Ancestors[0].AncestorRef.SectionName == in.Status.Ancestors[0].AncestorRef.SectionName {
		t.Error("status ancestorRef.sectionName pointer shared between copies")
	}
	out.Status.Ancestors[0].Conditions[0].Status = metav1.ConditionFalse
	if in.Status.Ancestors[0].Conditions[0].Status != metav1.ConditionTrue {
		t.Error("mutating copied conditions changed the original")
	}
}
