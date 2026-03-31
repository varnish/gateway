package controller

import (
	"context"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// testPEM is a minimal valid PEM block for testing.
var testPEM = `-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABLU3
jRJN1NWgh1MJxnSK+tWjfRwSTaOGkI4bHmSreA6SE0IbKPl2WPfJjDzpNqkSsOCd
ShNzgBRMMA71IwaciUyjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2wpSek9nBDE0
HKRXRfbUE6v5gLP8HBgFGKMo0mIRn8oCIHyjk+aIKEjVJGSGFDt2MqXVpvGjj+xB
3HT5LiaoOKsm
-----END CERTIFICATE-----
`

func newBackendTLSPolicyReconciler(scheme *runtime.Scheme, objs ...runtime.Object) *BackendTLSPolicyReconciler {
	allObjs := append([]runtime.Object{newTestGatewayClass("varnish")}, objs...)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(allObjs...).
		WithStatusSubresource(&gatewayv1.BackendTLSPolicy{}).
		Build()

	return &BackendTLSPolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Logger: slog.Default(),
	}
}

// --- serviceNamesFromPolicy ---

func TestServiceNamesFromPolicy(t *testing.T) {
	policy := &gatewayv1.BackendTLSPolicy{
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{
					LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{
						Group: "",
						Kind:  "Service",
						Name:  "svc-a",
					},
				},
				{
					LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{
						Group: "",
						Kind:  "Service",
						Name:  "svc-b",
					},
				},
				{
					// Non-service ref should be skipped
					LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{
						Group: "example.com",
						Kind:  "Other",
						Name:  "not-a-service",
					},
				},
			},
		},
	}

	names := serviceNamesFromPolicy(policy)
	if len(names) != 2 {
		t.Fatalf("expected 2 service names, got %d", len(names))
	}
	if _, ok := names["svc-a"]; !ok {
		t.Error("expected svc-a in names")
	}
	if _, ok := names["svc-b"]; !ok {
		t.Error("expected svc-b in names")
	}
}

func TestServiceNamesFromPolicy_Empty(t *testing.T) {
	policy := &gatewayv1.BackendTLSPolicy{}
	names := serviceNamesFromPolicy(policy)
	if len(names) != 0 {
		t.Fatalf("expected 0 service names, got %d", len(names))
	}
}

// --- targetRefsConflict ---

func TestTargetRefsConflict(t *testing.T) {
	tests := []struct {
		name     string
		a, b     gatewayv1.LocalPolicyTargetReferenceWithSectionName
		conflict bool
	}{
		{
			name: "different services, no conflict",
			a: gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Name: "svc-a"},
			},
			b: gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Name: "svc-b"},
			},
			conflict: false,
		},
		{
			name: "same service, no section names, conflict",
			a: gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Name: "svc-a"},
			},
			b: gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Name: "svc-a"},
			},
			conflict: true,
		},
		{
			name: "same service, same section names, conflict",
			a: gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Name: "svc-a"},
				SectionName:                new(gatewayv1.SectionName("port-80")),
			},
			b: gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Name: "svc-a"},
				SectionName:                new(gatewayv1.SectionName("port-80")),
			},
			conflict: true,
		},
		{
			name: "same service, different section names, no conflict",
			a: gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Name: "svc-a"},
				SectionName:                new(gatewayv1.SectionName("port-80")),
			},
			b: gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Name: "svc-a"},
				SectionName:                new(gatewayv1.SectionName("port-443")),
			},
			conflict: false,
		},
		{
			name: "same service, one has section name, no conflict",
			a: gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Name: "svc-a"},
				SectionName:                new(gatewayv1.SectionName("port-80")),
			},
			b: gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Name: "svc-a"},
			},
			conflict: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := targetRefsConflict(tt.a, tt.b)
			if got != tt.conflict {
				t.Errorf("targetRefsConflict() = %v, want %v", got, tt.conflict)
			}
		})
	}
}

// --- policyHasPrecedence ---

func TestPolicyHasPrecedence(t *testing.T) {
	now := metav1.Now()
	later := metav1.NewTime(now.Add(time.Second))

	tests := []struct {
		name       string
		a, b       *gatewayv1.BackendTLSPolicy
		aPrecedes  bool
	}{
		{
			name: "older policy wins",
			a: &gatewayv1.BackendTLSPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "policy-b", Namespace: "default", CreationTimestamp: now},
			},
			b: &gatewayv1.BackendTLSPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "policy-a", Namespace: "default", CreationTimestamp: later},
			},
			aPrecedes: true,
		},
		{
			name: "newer policy loses",
			a: &gatewayv1.BackendTLSPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "policy-a", Namespace: "default", CreationTimestamp: later},
			},
			b: &gatewayv1.BackendTLSPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "policy-b", Namespace: "default", CreationTimestamp: now},
			},
			aPrecedes: false,
		},
		{
			name: "same time, alphabetical wins",
			a: &gatewayv1.BackendTLSPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "default", CreationTimestamp: now},
			},
			b: &gatewayv1.BackendTLSPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "beta", Namespace: "default", CreationTimestamp: now},
			},
			aPrecedes: true,
		},
		{
			name: "same time, reverse alphabetical loses",
			a: &gatewayv1.BackendTLSPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "beta", Namespace: "default", CreationTimestamp: now},
			},
			b: &gatewayv1.BackendTLSPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "default", CreationTimestamp: now},
			},
			aPrecedes: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := policyHasPrecedence(tt.a, tt.b)
			if got != tt.aPrecedes {
				t.Errorf("policyHasPrecedence() = %v, want %v", got, tt.aPrecedes)
			}
		})
	}
}

// --- validateCACertificateRefs ---

func TestValidateCACertificateRefs_WellKnownSystem(t *testing.T) {
	scheme := newTestScheme()
	r := newBackendTLSPolicyReconciler(scheme)

	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			Validation: gatewayv1.BackendTLSPolicyValidation{
				WellKnownCACertificates: new(gatewayv1.WellKnownCACertificatesSystem),
				Hostname:                "example.com",
			},
		},
	}

	resolved, _, _ := r.validateCACertificateRefs(context.Background(), policy)
	if !resolved {
		t.Error("expected WellKnownCACertificates System to be resolved")
	}
}

func TestValidateCACertificateRefs_NoCerts(t *testing.T) {
	scheme := newTestScheme()
	r := newBackendTLSPolicyReconciler(scheme)

	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			Validation: gatewayv1.BackendTLSPolicyValidation{
				Hostname: "example.com",
			},
		},
	}

	resolved, reason, _ := r.validateCACertificateRefs(context.Background(), policy)
	if resolved {
		t.Error("expected no certs to fail validation")
	}
	if reason != string(gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef) {
		t.Errorf("unexpected reason: %s", reason)
	}
}

func TestValidateCACertificateRefs_UnsupportedKind(t *testing.T) {
	scheme := newTestScheme()
	r := newBackendTLSPolicyReconciler(scheme)

	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{Group: "", Kind: "Secret", Name: "my-secret"},
				},
				Hostname: "example.com",
			},
		},
	}

	resolved, reason, _ := r.validateCACertificateRefs(context.Background(), policy)
	if resolved {
		t.Error("expected unsupported kind to fail")
	}
	if reason != string(gatewayv1.BackendTLSPolicyReasonInvalidKind) {
		t.Errorf("unexpected reason: %s", reason)
	}
}

func TestValidateCACertificateRefs_MissingConfigMap(t *testing.T) {
	scheme := newTestScheme()
	r := newBackendTLSPolicyReconciler(scheme)

	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{Group: "", Kind: "ConfigMap", Name: "nonexistent"},
				},
				Hostname: "example.com",
			},
		},
	}

	resolved, reason, msg := r.validateCACertificateRefs(context.Background(), policy)
	if resolved {
		t.Error("expected missing ConfigMap to fail")
	}
	if reason != string(gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef) {
		t.Errorf("unexpected reason: %s", reason)
	}
	if msg == "" {
		t.Error("expected non-empty message")
	}
}

func TestValidateCACertificateRefs_MissingCACertKey(t *testing.T) {
	scheme := newTestScheme()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ca", Namespace: "default"},
		Data:       map[string]string{"other-key": "data"},
	}
	r := newBackendTLSPolicyReconciler(scheme, cm)

	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{Group: "", Kind: "ConfigMap", Name: "my-ca"},
				},
				Hostname: "example.com",
			},
		},
	}

	resolved, _, msg := r.validateCACertificateRefs(context.Background(), policy)
	if resolved {
		t.Error("expected missing ca.crt key to fail")
	}
	if msg == "" {
		t.Error("expected non-empty message")
	}
}

func TestValidateCACertificateRefs_InvalidPEM(t *testing.T) {
	scheme := newTestScheme()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ca", Namespace: "default"},
		Data:       map[string]string{caCertKey: "not-valid-pem"},
	}
	r := newBackendTLSPolicyReconciler(scheme, cm)

	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{Group: "", Kind: "ConfigMap", Name: "my-ca"},
				},
				Hostname: "example.com",
			},
		},
	}

	resolved, _, msg := r.validateCACertificateRefs(context.Background(), policy)
	if resolved {
		t.Error("expected invalid PEM to fail")
	}
	if msg == "" {
		t.Error("expected non-empty message")
	}
}

func TestValidateCACertificateRefs_ValidPEM(t *testing.T) {
	scheme := newTestScheme()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ca", Namespace: "default"},
		Data:       map[string]string{caCertKey: testPEM},
	}
	r := newBackendTLSPolicyReconciler(scheme, cm)

	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{Group: "", Kind: "ConfigMap", Name: "my-ca"},
				},
				Hostname: "example.com",
			},
		},
	}

	resolved, reason, _ := r.validateCACertificateRefs(context.Background(), policy)
	if !resolved {
		t.Error("expected valid PEM to pass")
	}
	if reason != string(gatewayv1.BackendTLSPolicyReasonResolvedRefs) {
		t.Errorf("unexpected reason: %s", reason)
	}
}

// --- isConflicted ---

func TestIsConflicted_NoConflict(t *testing.T) {
	scheme := newTestScheme()
	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "policy-a", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: "svc-a"}},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{Hostname: "example.com"},
		},
	}
	other := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "policy-b", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: "svc-b"}},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{Hostname: "other.com"},
		},
	}
	r := newBackendTLSPolicyReconciler(scheme, policy, other)

	if r.isConflicted(context.Background(), policy) {
		t.Error("expected no conflict when targeting different services")
	}
}

func TestIsConflicted_OlderPolicyWins(t *testing.T) {
	scheme := newTestScheme()
	now := metav1.Now()
	later := metav1.NewTime(now.Add(time.Second))

	older := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "older", Namespace: "default", CreationTimestamp: now},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: "svc-a"}},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{Hostname: "example.com"},
		},
	}
	newer := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "newer", Namespace: "default", CreationTimestamp: later},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: "svc-a"}},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{Hostname: "example.com"},
		},
	}
	r := newBackendTLSPolicyReconciler(scheme, older, newer)

	// The newer policy should be conflicted (older wins)
	if !r.isConflicted(context.Background(), newer) {
		t.Error("expected newer policy to be conflicted")
	}
	// The older policy should NOT be conflicted
	if r.isConflicted(context.Background(), older) {
		t.Error("expected older policy to NOT be conflicted")
	}
}

// --- clearOurStatus ---

func TestClearOurStatus(t *testing.T) {
	r := &BackendTLSPolicyReconciler{Logger: slog.Default()}
	policy := &gatewayv1.BackendTLSPolicy{
		Status: gatewayv1.PolicyStatus{
			Ancestors: []gatewayv1.PolicyAncestorStatus{
				{ControllerName: gatewayv1.GatewayController(ControllerName)},
				{ControllerName: "other-controller"},
			},
		},
	}

	r.clearOurStatus(policy)

	if len(policy.Status.Ancestors) != 1 {
		t.Fatalf("expected 1 ancestor status, got %d", len(policy.Status.Ancestors))
	}
	if string(policy.Status.Ancestors[0].ControllerName) != "other-controller" {
		t.Error("expected other-controller status to be preserved")
	}
}

// --- Reconcile (full flow with fake client) ---

func TestReconcile_NotFound(t *testing.T) {
	scheme := newTestScheme()
	r := newBackendTLSPolicyReconciler(scheme)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue {
		t.Error("expected no requeue")
	}
}

func TestReconcile_NoAncestorGateways_ClearsStatus(t *testing.T) {
	scheme := newTestScheme()

	// Policy exists but no HTTPRoutes reference its target service, so no ancestor gateways
	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: "svc-a"}},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{
				WellKnownCACertificates: new(gatewayv1.WellKnownCACertificatesSystem),
				Hostname:                "example.com",
			},
		},
		Status: gatewayv1.PolicyStatus{
			Ancestors: []gatewayv1.PolicyAncestorStatus{
				{ControllerName: gatewayv1.GatewayController(ControllerName)},
			},
		},
	}

	r := newBackendTLSPolicyReconciler(scheme, policy)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify status was cleared
	var updated gatewayv1.BackendTLSPolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "test-policy", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get policy: %v", err)
	}
	if len(updated.Status.Ancestors) != 0 {
		t.Errorf("expected ancestor status to be cleared, got %d entries", len(updated.Status.Ancestors))
	}
}

func TestReconcile_AcceptedWithValidCACert(t *testing.T) {
	scheme := newTestScheme()
	gwGroup := gatewayv1.Group(gatewayv1.GroupName)
	gwKind := gatewayv1.Kind("Gateway")

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ca", Namespace: "default"},
		Data:       map[string]string{caCertKey: testPEM},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "my-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Group: &gwGroup, Kind: &gwKind, Name: "my-gw"},
				},
			},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-a", Port: new(gatewayv1.PortNumber(8080))},
						}},
					},
				},
			},
		},
	}
	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: "svc-a"}},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{Group: "", Kind: "ConfigMap", Name: "my-ca"},
				},
				Hostname: "example.com",
			},
		},
	}

	r := newBackendTLSPolicyReconciler(scheme, cm, gateway, route, policy)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated gatewayv1.BackendTLSPolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "test-policy", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get policy: %v", err)
	}
	if len(updated.Status.Ancestors) != 1 {
		t.Fatalf("expected 1 ancestor status, got %d", len(updated.Status.Ancestors))
	}

	ancestor := updated.Status.Ancestors[0]
	if string(ancestor.AncestorRef.Name) != "my-gw" {
		t.Errorf("expected ancestor ref to my-gw, got %s", ancestor.AncestorRef.Name)
	}

	// Check Accepted condition
	var accepted, resolvedRefs *metav1.Condition
	for i := range ancestor.Conditions {
		switch ancestor.Conditions[i].Type {
		case string(gatewayv1.PolicyConditionAccepted):
			accepted = &ancestor.Conditions[i]
		case string(gatewayv1.BackendTLSPolicyConditionResolvedRefs):
			resolvedRefs = &ancestor.Conditions[i]
		}
	}

	if accepted == nil {
		t.Fatal("missing Accepted condition")
	}
	if accepted.Status != metav1.ConditionTrue {
		t.Errorf("expected Accepted=True, got %s (reason: %s, message: %s)", accepted.Status, accepted.Reason, accepted.Message)
	}
	if resolvedRefs == nil {
		t.Fatal("missing ResolvedRefs condition")
	}
	if resolvedRefs.Status != metav1.ConditionTrue {
		t.Errorf("expected ResolvedRefs=True, got %s", resolvedRefs.Status)
	}
}

func TestReconcile_RejectedInvalidCACert(t *testing.T) {
	scheme := newTestScheme()
	gwGroup := gatewayv1.Group(gatewayv1.GroupName)
	gwKind := gatewayv1.Kind("Gateway")

	// ConfigMap with invalid PEM
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-ca", Namespace: "default"},
		Data:       map[string]string{caCertKey: "not-valid-pem"},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
			Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "my-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Group: &gwGroup, Kind: &gwKind, Name: "my-gw"}},
			},
			Rules: []gatewayv1.HTTPRouteRule{
				{BackendRefs: []gatewayv1.HTTPBackendRef{
					{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-a", Port: new(gatewayv1.PortNumber(8080))}}},
				}},
			},
		},
	}
	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: "svc-a"}},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{Group: "", Kind: "ConfigMap", Name: "bad-ca"},
				},
				Hostname: "example.com",
			},
		},
	}

	r := newBackendTLSPolicyReconciler(scheme, cm, gateway, route, policy)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-policy", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated gatewayv1.BackendTLSPolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "test-policy", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get policy: %v", err)
	}
	if len(updated.Status.Ancestors) != 1 {
		t.Fatalf("expected 1 ancestor, got %d", len(updated.Status.Ancestors))
	}

	var accepted *metav1.Condition
	for i := range updated.Status.Ancestors[0].Conditions {
		if updated.Status.Ancestors[0].Conditions[i].Type == string(gatewayv1.PolicyConditionAccepted) {
			accepted = &updated.Status.Ancestors[0].Conditions[i]
		}
	}
	if accepted == nil {
		t.Fatal("missing Accepted condition")
	}
	if accepted.Status != metav1.ConditionFalse {
		t.Errorf("expected Accepted=False, got %s", accepted.Status)
	}
}

// --- findBackendTLSPoliciesForHTTPRoute ---

func TestFindBackendTLSPoliciesForHTTPRoute(t *testing.T) {
	scheme := newTestScheme()
	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "my-policy", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: "svc-a"}},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{Hostname: "example.com"},
		},
	}
	r := newBackendTLSPolicyReconciler(scheme, policy)

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "my-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{BackendRefs: []gatewayv1.HTTPBackendRef{
					{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-a"}}},
				}},
			},
		},
	}

	requests := r.findBackendTLSPoliciesForHTTPRoute(context.Background(), route)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "my-policy" {
		t.Errorf("expected request for my-policy, got %s", requests[0].Name)
	}
}

func TestFindBackendTLSPoliciesForHTTPRoute_NoMatch(t *testing.T) {
	scheme := newTestScheme()
	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "my-policy", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: "svc-a"}},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{Hostname: "example.com"},
		},
	}
	r := newBackendTLSPolicyReconciler(scheme, policy)

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "my-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{BackendRefs: []gatewayv1.HTTPBackendRef{
					{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "different-svc"}}},
				}},
			},
		},
	}

	requests := r.findBackendTLSPoliciesForHTTPRoute(context.Background(), route)
	if len(requests) != 0 {
		t.Fatalf("expected 0 requests, got %d", len(requests))
	}
}

// --- findBackendTLSPoliciesForConfigMap ---

func TestFindBackendTLSPoliciesForConfigMap(t *testing.T) {
	scheme := newTestScheme()
	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "my-policy", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: "svc-a"}},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{Kind: "ConfigMap", Name: "my-ca"},
				},
				Hostname: "example.com",
			},
		},
	}
	r := newBackendTLSPolicyReconciler(scheme, policy)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ca", Namespace: "default"},
	}
	requests := r.findBackendTLSPoliciesForConfigMap(context.Background(), cm)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "my-policy" {
		t.Errorf("expected my-policy, got %s", requests[0].Name)
	}
}

func TestFindBackendTLSPoliciesForConfigMap_NoMatch(t *testing.T) {
	scheme := newTestScheme()
	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "my-policy", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: "svc-a"}},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{Kind: "ConfigMap", Name: "my-ca"},
				},
				Hostname: "example.com",
			},
		},
	}
	r := newBackendTLSPolicyReconciler(scheme, policy)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-cm", Namespace: "default"},
	}
	requests := r.findBackendTLSPoliciesForConfigMap(context.Background(), cm)
	if len(requests) != 0 {
		t.Fatalf("expected 0 requests, got %d", len(requests))
	}
}

// --- findBackendTLSPoliciesForGateway ---

func TestFindBackendTLSPoliciesForGateway_OurClass(t *testing.T) {
	scheme := newTestScheme()
	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "my-policy", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: "svc-a"}},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{Hostname: "example.com"},
		},
	}
	r := newBackendTLSPolicyReconciler(scheme, policy)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
	}

	requests := r.findBackendTLSPoliciesForGateway(context.Background(), gw)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
}

func TestFindBackendTLSPoliciesForGateway_NotOurClass(t *testing.T) {
	scheme := newTestScheme()
	otherGC := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "other"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "other-controller"},
	}
	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "my-policy", Namespace: "default"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: "svc-a"}},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{Hostname: "example.com"},
		},
	}
	r := newBackendTLSPolicyReconciler(scheme, otherGC, policy)

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "other"},
	}

	requests := r.findBackendTLSPoliciesForGateway(context.Background(), gw)
	if len(requests) != 0 {
		t.Fatalf("expected 0 requests for non-our gateway class, got %d", len(requests))
	}
}
