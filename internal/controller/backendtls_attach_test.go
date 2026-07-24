package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/varnish/gateway/internal/ghost"
)

// backendTLSPolicy is a small helper to build a BackendTLSPolicy targeting a
// Service (optionally scoped to a Service port via sectionName) with a hostname.
func backendTLSPolicy(name, ns, service, section, hostname string, created metav1.Time, validation gatewayv1.BackendTLSPolicyValidation) *gatewayv1.BackendTLSPolicy {
	targetRef := gatewayv1.LocalPolicyTargetReferenceWithSectionName{
		LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{Kind: "Service", Name: gatewayv1.ObjectName(service)},
	}
	if section != "" {
		s := gatewayv1.SectionName(section)
		targetRef.SectionName = &s
	}
	validation.Hostname = gatewayv1.PreciseHostname(hostname)
	return &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: created},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{targetRef},
			Validation: validation,
		},
	}
}

// systemValidation returns a validation block that always resolves.
func systemValidation() gatewayv1.BackendTLSPolicyValidation {
	wk := gatewayv1.WellKnownCACertificatesSystem
	return gatewayv1.BackendTLSPolicyValidation{WellKnownCACertificates: &wk}
}

// M-5: the data-plane winner must match the GEP-713 precedence winner (oldest
// creationTimestamp), not depend on List iteration order.
func TestAttachBackendTLS_PrecedenceOldestWins(t *testing.T) {
	scheme := newTestScheme()
	now := metav1.Now()
	later := metav1.NewTime(now.Add(time.Second))

	// Two policies target the same Service with no sectionName. The older one wins.
	older := backendTLSPolicy("aaa-newer-name-but-older", "default", "svc", "", "older.example.com", now, systemValidation())
	newer := backendTLSPolicy("zzz-older-name-but-newer", "default", "svc", "", "newer.example.com", later, systemValidation())

	// Provide them in the "wrong" order (newer first) to prove ordering is applied.
	r := newHTTPRouteTestReconciler(scheme, newer, older)

	routes := []ghost.Route{{Namespace: "default", Service: "svc", Port: 8080}}
	r.attachBackendTLS(context.Background(), routes)

	if routes[0].BackendTLS == nil {
		t.Fatal("expected BackendTLS to be attached")
	}
	if routes[0].BackendTLS.Hostname != "older.example.com" {
		t.Errorf("expected older policy to win, got hostname %q", routes[0].BackendTLS.Hostname)
	}
}

// M-5: with equal timestamps, the alphabetically-first namespace/name wins.
func TestAttachBackendTLS_PrecedenceAlphabeticalTiebreak(t *testing.T) {
	scheme := newTestScheme()
	ts := metav1.Now()

	a := backendTLSPolicy("policy-a", "default", "svc", "", "a.example.com", ts, systemValidation())
	b := backendTLSPolicy("policy-b", "default", "svc", "", "b.example.com", ts, systemValidation())

	r := newHTTPRouteTestReconciler(scheme, b, a)

	routes := []ghost.Route{{Namespace: "default", Service: "svc", Port: 8080}}
	r.attachBackendTLS(context.Background(), routes)

	if routes[0].BackendTLS == nil || routes[0].BackendTLS.Hostname != "a.example.com" {
		t.Errorf("expected policy-a to win alphabetical tiebreak, got %+v", routes[0].BackendTLS)
	}
}

// M-6: a section-scoped policy applies only to the route hitting that Service
// port; a section-less policy applies to the other port.
func TestAttachBackendTLS_SectionNameScoping(t *testing.T) {
	scheme := newTestScheme()
	ts := metav1.Now()

	// Service with two named ports mapping to distinct target ports.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "web", Port: 80, TargetPort: intstr.FromInt(8080)},
				{Name: "admin", Port: 9090, TargetPort: intstr.FromInt(9091)},
			},
		},
	}

	// Section-scoped to "web" and a service-wide policy.
	webPolicy := backendTLSPolicy("web-policy", "default", "svc", "web", "web.example.com", ts, systemValidation())
	widePolicy := backendTLSPolicy("wide-policy", "default", "svc", "", "wide.example.com", ts, systemValidation())

	r := newHTTPRouteTestReconciler(scheme, svc, webPolicy, widePolicy)

	// Route hitting target port 8080 ("web") and one hitting 9091 ("admin").
	routes := []ghost.Route{
		{Namespace: "default", Service: "svc", Port: 8080}, // web
		{Namespace: "default", Service: "svc", Port: 9091}, // admin
	}
	r.attachBackendTLS(context.Background(), routes)

	if routes[0].BackendTLS == nil || routes[0].BackendTLS.Hostname != "web.example.com" {
		t.Errorf("web route: expected section-scoped policy, got %+v", routes[0].BackendTLS)
	}
	// admin port has no section-scoped policy, so the service-wide one applies.
	if routes[1].BackendTLS == nil || routes[1].BackendTLS.Hostname != "wide.example.com" {
		t.Errorf("admin route: expected service-wide policy, got %+v", routes[1].BackendTLS)
	}
}

// M-6: a port-specific policy must NOT leak onto other ports when no service-wide
// policy exists.
func TestAttachBackendTLS_SectionScopedDoesNotLeak(t *testing.T) {
	scheme := newTestScheme()
	ts := metav1.Now()

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "web", Port: 80, TargetPort: intstr.FromInt(8080)},
				{Name: "admin", Port: 9090, TargetPort: intstr.FromInt(9091)},
			},
		},
	}
	webPolicy := backendTLSPolicy("web-policy", "default", "svc", "web", "web.example.com", ts, systemValidation())

	r := newHTTPRouteTestReconciler(scheme, svc, webPolicy)

	routes := []ghost.Route{
		{Namespace: "default", Service: "svc", Port: 9091}, // admin — no matching policy
	}
	r.attachBackendTLS(context.Background(), routes)

	if routes[0].BackendTLS != nil {
		t.Errorf("admin route should not get the web-scoped policy, got %+v", routes[0].BackendTLS)
	}
}

// M-6: a route with a named targetPort carries PortName directly; the section
// match should use it without needing a Service lookup.
func TestAttachBackendTLS_SectionNameFromRoutePortName(t *testing.T) {
	scheme := newTestScheme()
	ts := metav1.Now()

	webPolicy := backendTLSPolicy("web-policy", "default", "svc", "web", "web.example.com", ts, systemValidation())
	r := newHTTPRouteTestReconciler(scheme, webPolicy)

	routes := []ghost.Route{
		{Namespace: "default", Service: "svc", Port: 0, PortName: "web"},
	}
	r.attachBackendTLS(context.Background(), routes)

	if routes[0].BackendTLS == nil || routes[0].BackendTLS.Hostname != "web.example.com" {
		t.Errorf("expected section match via PortName, got %+v", routes[0].BackendTLS)
	}
}

// M-7: a policy whose caCertificateRefs don't resolve must be skipped — backend
// TLS must not be enabled for a policy that can't verify.
func TestAttachBackendTLS_UnresolvableCARefSkipped(t *testing.T) {
	scheme := newTestScheme()
	ts := metav1.Now()

	// Policy references a ConfigMap that does not exist.
	badPolicy := backendTLSPolicy("bad", "default", "svc", "", "bad.example.com", ts,
		gatewayv1.BackendTLSPolicyValidation{
			CACertificateRefs: []gatewayv1.LocalObjectReference{{Kind: "ConfigMap", Name: "missing-ca"}},
		})

	r := newHTTPRouteTestReconciler(scheme, badPolicy)

	routes := []ghost.Route{{Namespace: "default", Service: "svc", Port: 8080}}
	r.attachBackendTLS(context.Background(), routes)

	if routes[0].BackendTLS != nil {
		t.Errorf("expected unresolvable policy to be skipped, got %+v", routes[0].BackendTLS)
	}
}

// M-7 + M-5: when an unresolvable policy has precedence over a resolvable one,
// the resolvable one must still be programmed (the unresolvable never enters the
// candidate set).
func TestAttachBackendTLS_ResolvableWinsOverUnresolvable(t *testing.T) {
	scheme := newTestScheme()
	now := metav1.Now()
	later := metav1.NewTime(now.Add(time.Second))

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "good-ca", Namespace: "default"},
		Data:       map[string]string{caCertKey: testPEM},
	}
	// Older policy is unresolvable (missing CM); newer is resolvable.
	bad := backendTLSPolicy("bad-older", "default", "svc", "", "bad.example.com", now,
		gatewayv1.BackendTLSPolicyValidation{
			CACertificateRefs: []gatewayv1.LocalObjectReference{{Kind: "ConfigMap", Name: "missing-ca"}},
		})
	good := backendTLSPolicy("good-newer", "default", "svc", "", "good.example.com", later,
		gatewayv1.BackendTLSPolicyValidation{
			CACertificateRefs: []gatewayv1.LocalObjectReference{{Kind: "ConfigMap", Name: "good-ca"}},
		})

	r := newHTTPRouteTestReconciler(scheme, cm, bad, good)

	routes := []ghost.Route{{Namespace: "default", Service: "svc", Port: 8080}}
	r.attachBackendTLS(context.Background(), routes)

	if routes[0].BackendTLS == nil || routes[0].BackendTLS.Hostname != "good.example.com" {
		t.Errorf("expected resolvable policy to be programmed, got %+v", routes[0].BackendTLS)
	}
}

// A route whose Service is not targeted by any policy gets no BackendTLS.
func TestAttachBackendTLS_NoMatchingPolicy(t *testing.T) {
	scheme := newTestScheme()
	ts := metav1.Now()

	policy := backendTLSPolicy("p", "default", "other-svc", "", "example.com", ts, systemValidation())
	r := newHTTPRouteTestReconciler(scheme, policy)

	routes := []ghost.Route{{Namespace: "default", Service: "svc", Port: 8080}}
	r.attachBackendTLS(context.Background(), routes)

	if routes[0].BackendTLS != nil {
		t.Errorf("expected no BackendTLS for unmatched service, got %+v", routes[0].BackendTLS)
	}
}

// resolveServicePortName maps a numeric target port back to the Service port name.
func TestResolveServicePortName(t *testing.T) {
	scheme := newTestScheme()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "web", Port: 80, TargetPort: intstr.FromInt(8080)},
				{Name: "plain", Port: 8081}, // targetPort defaults to Port
			},
		},
	}
	r := newHTTPRouteTestReconciler(scheme, svc)
	cache := make(map[types.NamespacedName]map[int]string)

	tests := []struct {
		name  string
		route ghost.Route
		want  string
	}{
		{"named-targetport-uses-portname", ghost.Route{Namespace: "default", Service: "svc", PortName: "web"}, "web"},
		{"numeric-targetport-maps-to-name", ghost.Route{Namespace: "default", Service: "svc", Port: 8080}, "web"},
		{"default-targetport-maps-to-name", ghost.Route{Namespace: "default", Service: "svc", Port: 8081}, "plain"},
		{"unknown-port-empty", ghost.Route{Namespace: "default", Service: "svc", Port: 9999}, ""},
		{"missing-service-empty", ghost.Route{Namespace: "default", Service: "nope", Port: 80}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.resolveServicePortName(context.Background(), &tt.route, cache)
			if got != tt.want {
				t.Errorf("resolveServicePortName = %q, want %q", got, tt.want)
			}
		})
	}
}
