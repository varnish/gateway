package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
)

func TestResolveServiceConfig_NoParams_DefaultsToLoadBalancer(t *testing.T) {
	gw := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"}}

	got := resolveServiceConfig(gw, nil)

	if got.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("Type = %v, want LoadBalancer", got.Type)
	}
	if len(got.Annotations) != 0 {
		t.Errorf("Annotations = %v, want empty", got.Annotations)
	}
	if len(got.Labels) != 0 {
		t.Errorf("Labels = %v, want empty", got.Labels)
	}
	if got.LoadBalancerClass != nil {
		t.Errorf("LoadBalancerClass = %v, want nil", *got.LoadBalancerClass)
	}
	if got.ExternalTrafficPolicy != nil {
		t.Errorf("ExternalTrafficPolicy = %v, want nil", *got.ExternalTrafficPolicy)
	}
}

func TestResolveServiceConfig_NilServiceField_DefaultsToLoadBalancer(t *testing.T) {
	gw := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"}}
	params := &gatewayparamsv1alpha1.GatewayClassParameters{}

	got := resolveServiceConfig(gw, params)

	if got.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("Type = %v, want LoadBalancer", got.Type)
	}
}

func TestResolveServiceConfig_NilTypeInService_DefaultsToLoadBalancer(t *testing.T) {
	gw := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"}}
	params := &gatewayparamsv1alpha1.GatewayClassParameters{
		Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
			Service: &gatewayparamsv1alpha1.ServiceConfig{
				Annotations: map[string]string{"foo": "bar"},
			},
		},
	}

	got := resolveServiceConfig(gw, params)

	if got.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("Type = %v, want LoadBalancer", got.Type)
	}
	if got.Annotations["foo"] != "bar" {
		t.Errorf("Annotations[foo] = %q, want bar", got.Annotations["foo"])
	}
}

func TestResolveServiceConfig_ClassOnly(t *testing.T) {
	gw := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"}}
	etp := corev1.ServiceExternalTrafficPolicyLocal
	lbClass := "service.k8s.io/cloud-provider-mock"
	params := &gatewayparamsv1alpha1.GatewayClassParameters{
		Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
			Service: &gatewayparamsv1alpha1.ServiceConfig{
				Type:                     ptr(corev1.ServiceTypeClusterIP),
				Annotations:              map[string]string{"a": "1", "b": "2"},
				Labels:                   map[string]string{"team": "edge"},
				LoadBalancerClass:        &lbClass,
				LoadBalancerSourceRanges: []string{"10.0.0.0/8"},
				ExternalTrafficPolicy:    &etp,
			},
		},
	}

	got := resolveServiceConfig(gw, params)

	if got.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Type = %v, want ClusterIP", got.Type)
	}
	if got.Annotations["a"] != "1" || got.Annotations["b"] != "2" {
		t.Errorf("Annotations = %v", got.Annotations)
	}
	if got.Labels["team"] != "edge" {
		t.Errorf("Labels = %v", got.Labels)
	}
	if got.LoadBalancerClass == nil || *got.LoadBalancerClass != lbClass {
		t.Errorf("LoadBalancerClass = %v, want %q", got.LoadBalancerClass, lbClass)
	}
	if len(got.LoadBalancerSourceRanges) != 1 || got.LoadBalancerSourceRanges[0] != "10.0.0.0/8" {
		t.Errorf("LoadBalancerSourceRanges = %v", got.LoadBalancerSourceRanges)
	}
	if got.ExternalTrafficPolicy == nil || *got.ExternalTrafficPolicy != etp {
		t.Errorf("ExternalTrafficPolicy = %v", got.ExternalTrafficPolicy)
	}
}

func TestResolveServiceConfig_GatewayInfraOverlay_AnnotationsAndLabels(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				Annotations: map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue{
					"a":   "overridden", // collides with class
					"new": "value",      // new key
				},
				Labels: map[gatewayv1.LabelKey]gatewayv1.LabelValue{
					"team": "core", // overrides class
				},
			},
		},
	}
	params := &gatewayparamsv1alpha1.GatewayClassParameters{
		Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
			Service: &gatewayparamsv1alpha1.ServiceConfig{
				Annotations: map[string]string{"a": "class", "kept": "yes"},
				Labels:      map[string]string{"team": "edge", "tier": "cache"},
			},
		},
	}

	got := resolveServiceConfig(gw, params)

	if got.Annotations["a"] != "overridden" {
		t.Errorf("Annotations[a] = %q, want overridden", got.Annotations["a"])
	}
	if got.Annotations["new"] != "value" {
		t.Errorf("Annotations[new] = %q, want value", got.Annotations["new"])
	}
	if got.Annotations["kept"] != "yes" {
		t.Errorf("Annotations[kept] = %q, want yes", got.Annotations["kept"])
	}
	if got.Labels["team"] != "core" {
		t.Errorf("Labels[team] = %q, want core", got.Labels["team"])
	}
	if got.Labels["tier"] != "cache" {
		t.Errorf("Labels[tier] = %q, want cache", got.Labels["tier"])
	}
}

func TestResolveServiceConfig_GatewayInfraOnly_NoClassConfig(t *testing.T) {
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				Annotations: map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue{
					"per-gw": "ok",
				},
			},
		},
	}

	got := resolveServiceConfig(gw, nil)

	if got.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("Type = %v, want LoadBalancer", got.Type)
	}
	if got.Annotations["per-gw"] != "ok" {
		t.Errorf("Annotations[per-gw] = %q, want ok", got.Annotations["per-gw"])
	}
}

func TestResolveServiceConfig_NilGateway(t *testing.T) {
	// The resolver guards on nil gateway. This pins the contract so a
	// future refactor cannot remove the guard without a test failure.
	got := resolveServiceConfig(nil, nil)

	if got.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("Type = %v, want LoadBalancer", got.Type)
	}
	if got.Annotations == nil {
		t.Errorf("Annotations should be non-nil empty map, got nil")
	}
	if got.Labels == nil {
		t.Errorf("Labels should be non-nil empty map, got nil")
	}
}

func TestResolveServiceConfig_DefensiveCopy_SourceRanges(t *testing.T) {
	// Mutating the input slice after resolution must not affect the output.
	// Pins the defensive-copy behavior against future regressions.
	gw := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"}}
	ranges := []string{"10.0.0.0/8"}
	params := &gatewayparamsv1alpha1.GatewayClassParameters{
		Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
			Service: &gatewayparamsv1alpha1.ServiceConfig{
				LoadBalancerSourceRanges: ranges,
			},
		},
	}

	got := resolveServiceConfig(gw, params)
	ranges[0] = "mutated"

	if got.LoadBalancerSourceRanges[0] != "10.0.0.0/8" {
		t.Errorf("LoadBalancerSourceRanges[0] = %q, want 10.0.0.0/8 (input mutation leaked into output)", got.LoadBalancerSourceRanges[0])
	}
}

func TestResolveServiceConfig_GatewayInfraDoesNotOverrideType(t *testing.T) {
	// Only labels/annotations layer from Gateway.spec.infrastructure.
	// Type, LBClass, SourceRanges, ETP stay class-level.
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			Infrastructure: &gatewayv1.GatewayInfrastructure{},
		},
	}
	params := &gatewayparamsv1alpha1.GatewayClassParameters{
		Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
			Service: &gatewayparamsv1alpha1.ServiceConfig{
				Type: ptr(corev1.ServiceTypeNodePort),
			},
		},
	}

	got := resolveServiceConfig(gw, params)

	if got.Type != corev1.ServiceTypeNodePort {
		t.Errorf("Type = %v, want NodePort", got.Type)
	}
}
