package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
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
				Type:                     ptr.To(corev1.ServiceTypeClusterIP),
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
				Type: ptr.To(corev1.ServiceTypeNodePort),
			},
		},
	}

	got := resolveServiceConfig(gw, params)

	if got.Type != corev1.ServiceTypeNodePort {
		t.Errorf("Type = %v, want NodePort", got.Type)
	}
}

func TestMergeWithManaged_FirstApply_NoSentinel(t *testing.T) {
	existing := map[string]string{}
	sentinel := ""
	desired := map[string]string{"a": "1", "b": "2"}

	merged, newSentinel := mergeWithManaged(desired, existing, sentinel, nil)

	if merged["a"] != "1" || merged["b"] != "2" {
		t.Errorf("merged = %v", merged)
	}
	if newSentinel != "a,b" {
		t.Errorf("sentinel = %q, want %q", newSentinel, "a,b")
	}
}

func TestMergeWithManaged_DriftAdd(t *testing.T) {
	existing := map[string]string{"a": "1"}
	sentinel := "a"
	desired := map[string]string{"a": "1", "b": "2"}

	merged, newSentinel := mergeWithManaged(desired, existing, sentinel, nil)

	if merged["a"] != "1" || merged["b"] != "2" {
		t.Errorf("merged = %v", merged)
	}
	if newSentinel != "a,b" {
		t.Errorf("sentinel = %q, want %q", newSentinel, "a,b")
	}
}

func TestMergeWithManaged_DriftRemove(t *testing.T) {
	// Key "b" was previously managed (listed in sentinel) and is no longer
	// in desired — must be pruned from the output map.
	existing := map[string]string{"a": "1", "b": "2"}
	sentinel := "a,b"
	desired := map[string]string{"a": "1"}

	merged, newSentinel := mergeWithManaged(desired, existing, sentinel, nil)

	if merged["a"] != "1" {
		t.Errorf("merged[a] = %q, want 1", merged["a"])
	}
	if _, ok := merged["b"]; ok {
		t.Errorf("merged[b] should be pruned, got %v", merged)
	}
	if newSentinel != "a" {
		t.Errorf("sentinel = %q, want %q", newSentinel, "a")
	}
}

func TestMergeWithManaged_ExternalKeyUntouched(t *testing.T) {
	// "cloud.k8s.io/foo" was never managed by us — must survive across reconciles.
	existing := map[string]string{
		"a":                "1",
		"cloud.k8s.io/foo": "bar",
	}
	sentinel := "a"
	desired := map[string]string{"a": "1"}

	merged, newSentinel := mergeWithManaged(desired, existing, sentinel, nil)

	if merged["cloud.k8s.io/foo"] != "bar" {
		t.Errorf("external key was modified: %v", merged)
	}
	if newSentinel != "a" {
		t.Errorf("sentinel = %q", newSentinel)
	}
}

func TestMergeWithManaged_EmptyDesired_PrunesAllManaged(t *testing.T) {
	existing := map[string]string{
		"a":                "1",
		"b":                "2",
		"cloud.k8s.io/foo": "bar",
	}
	sentinel := "a,b"
	desired := map[string]string{}

	merged, newSentinel := mergeWithManaged(desired, existing, sentinel, nil)

	if _, ok := merged["a"]; ok {
		t.Errorf("a not pruned: %v", merged)
	}
	if _, ok := merged["b"]; ok {
		t.Errorf("b not pruned: %v", merged)
	}
	if merged["cloud.k8s.io/foo"] != "bar" {
		t.Errorf("external key was modified: %v", merged)
	}
	if newSentinel != "" {
		t.Errorf("sentinel = %q, want empty", newSentinel)
	}
}

func TestMergeWithManaged_SentinelIsSorted(t *testing.T) {
	// Sorted sentinel keys are required for deterministic reconciliation.
	existing := map[string]string{}
	desired := map[string]string{"z": "1", "a": "2", "m": "3"}

	_, newSentinel := mergeWithManaged(desired, existing, "", nil)

	if newSentinel != "a,m,z" {
		t.Errorf("sentinel = %q, want %q", newSentinel, "a,m,z")
	}
}

func TestMergeWithManaged_ProtectedKey_DroppedFromDesired(t *testing.T) {
	// User tried to override a controller-managed label.
	existing := map[string]string{
		LabelManagedBy: ManagedByValue, // already set by buildLabels
	}
	sentinel := ""
	desired := map[string]string{
		LabelManagedBy: "evil-actor", // user trying to hijack
		"team":         "edge",       // legitimate user label
	}
	protected := map[string]struct{}{LabelManagedBy: {}}

	merged, newSentinel := mergeWithManaged(desired, existing, sentinel, protected)

	if merged[LabelManagedBy] != ManagedByValue {
		t.Errorf("protected key was overridden: %v", merged)
	}
	if merged["team"] != "edge" {
		t.Errorf("legitimate key not applied: %v", merged)
	}
	// Sentinel must NOT contain the protected key — it was never managed via spec.
	if newSentinel != "team" {
		t.Errorf("sentinel = %q, want %q", newSentinel, "team")
	}
}

func TestMergeWithManaged_NilExistingMap(t *testing.T) {
	// A freshly-built Service may have nil annotations/labels — must not panic.
	desired := map[string]string{"a": "1"}

	merged, newSentinel := mergeWithManaged(desired, nil, "", nil)

	if merged["a"] != "1" {
		t.Errorf("merged = %v", merged)
	}
	if newSentinel != "a" {
		t.Errorf("sentinel = %q", newSentinel)
	}
}

func TestMergeWithManaged_DesiredValueWinsOverExisting(t *testing.T) {
	// Pin the apply-order: desired must overwrite existing values for the
	// same key. A regression that inverted this would silently leak stale
	// annotations across reconciles.
	existing := map[string]string{"a": "old-value"}
	desired := map[string]string{"a": "new-value"}

	merged, _ := mergeWithManaged(desired, existing, "a", nil)

	if merged["a"] != "new-value" {
		t.Errorf("merged[a] = %q, want new-value (desired must win)", merged["a"])
	}
}

func TestMergeWithManaged_Idempotent(t *testing.T) {
	// The function is called on every reconcile; feeding its output back as
	// the next call's existing must yield identical results. Catches subtle
	// bugs like sentinel keys leaking into the managed set or non-deterministic
	// sentinel ordering.
	desired := map[string]string{"x": "1", "y": "2"}
	protected := map[string]struct{}{"forbidden": {}}

	merged1, sentinel1 := mergeWithManaged(desired, nil, "", protected)
	merged2, sentinel2 := mergeWithManaged(desired, merged1, sentinel1, protected)

	if sentinel1 != sentinel2 {
		t.Errorf("sentinel changed between calls: %q vs %q", sentinel1, sentinel2)
	}
	if len(merged1) != len(merged2) {
		t.Errorf("merged map size changed: %d vs %d", len(merged1), len(merged2))
	}
	for k, v := range merged1 {
		if merged2[k] != v {
			t.Errorf("merged[%q] = %q on second call, want %q", k, merged2[k], v)
		}
	}
}
