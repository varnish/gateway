package controller

import (
	"context"
	"log/slog"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
	"github.com/varnish/gateway/internal/ghost"
)

func newVCPTestReconciler(scheme *runtime.Scheme, objs ...runtime.Object) *VarnishCachePolicyReconciler {
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&gatewayparamsv1alpha1.VarnishCachePolicy{}).
		Build()
	return &VarnishCachePolicyReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Logger: slog.Default(),
	}
}

func newVCP(name, namespace string, spec gatewayparamsv1alpha1.VarnishCachePolicySpec) *gatewayparamsv1alpha1.VarnishCachePolicy {
	return &gatewayparamsv1alpha1.VarnishCachePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.Now(),
		},
		Spec: spec,
	}
}

func vcpTargetRef(kind, name string, sectionName *string) gatewayparamsv1alpha1.PolicyTargetReference {
	return gatewayparamsv1alpha1.PolicyTargetReference{
		Group:       "gateway.networking.k8s.io",
		Kind:        kind,
		Name:        name,
		SectionName: sectionName,
	}
}

func durationPtr(d time.Duration) *metav1.Duration {
	return &metav1.Duration{Duration: d}
}

func boolPtr(b bool) *bool {
	return &b
}

func stringPtr(s string) *string {
	return &s
}

// --- TestValidateSpec ---

func TestValidateSpec(t *testing.T) {
	r := &VarnishCachePolicyReconciler{}

	tests := []struct {
		name    string
		spec    gatewayparamsv1alpha1.VarnishCachePolicySpec
		wantErr string
	}{
		{
			name: "valid with defaultTTL only",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
				DefaultTTL: durationPtr(60 * time.Second),
			},
		},
		{
			name: "valid with forcedTTL only",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef: vcpTargetRef("HTTPRoute", "my-route", nil),
				ForcedTTL: durationPtr(300 * time.Second),
			},
		},
		{
			name: "valid with grace and keep",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
				DefaultTTL: durationPtr(60 * time.Second),
				Grace:      durationPtr(30 * time.Second),
				Keep:       durationPtr(120 * time.Second),
			},
		},
		{
			name: "valid with zero grace",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
				DefaultTTL: durationPtr(60 * time.Second),
				Grace:      durationPtr(0),
			},
		},
		{
			name: "valid with cacheKey headers",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
				DefaultTTL: durationPtr(60 * time.Second),
				CacheKey: &gatewayparamsv1alpha1.CacheKeySpec{
					Headers: []string{"Accept-Language", "X-Region"},
				},
			},
		},
		{
			name: "valid with query param include",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
				DefaultTTL: durationPtr(60 * time.Second),
				CacheKey: &gatewayparamsv1alpha1.CacheKeySpec{
					QueryParameters: &gatewayparamsv1alpha1.QueryParameterKeySpec{
						Include: []string{"page", "sort"},
					},
				},
			},
		},
		{
			name: "both defaultTTL and forcedTTL",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
				DefaultTTL: durationPtr(60 * time.Second),
				ForcedTTL:  durationPtr(300 * time.Second),
			},
			wantErr: "mutually exclusive",
		},
		{
			name: "neither defaultTTL nor forcedTTL",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef: vcpTargetRef("HTTPRoute", "my-route", nil),
			},
			wantErr: "exactly one of defaultTTL or forcedTTL must be set",
		},
		{
			name: "negative defaultTTL",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
				DefaultTTL: durationPtr(-1 * time.Second),
			},
			wantErr: "defaultTTL must be positive",
		},
		{
			name: "zero defaultTTL",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
				DefaultTTL: durationPtr(0),
			},
			wantErr: "defaultTTL must be positive",
		},
		{
			name: "negative forcedTTL",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef: vcpTargetRef("HTTPRoute", "my-route", nil),
				ForcedTTL: durationPtr(-5 * time.Second),
			},
			wantErr: "forcedTTL must be positive",
		},
		{
			name: "negative grace",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
				DefaultTTL: durationPtr(60 * time.Second),
				Grace:      durationPtr(-1 * time.Second),
			},
			wantErr: "grace must be non-negative",
		},
		{
			name: "negative keep",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
				DefaultTTL: durationPtr(60 * time.Second),
				Keep:       durationPtr(-1 * time.Second),
			},
			wantErr: "keep must be non-negative",
		},
		{
			name: "query params include and exclude both set",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
				DefaultTTL: durationPtr(60 * time.Second),
				CacheKey: &gatewayparamsv1alpha1.CacheKeySpec{
					QueryParameters: &gatewayparamsv1alpha1.QueryParameterKeySpec{
						Include: []string{"page"},
						Exclude: []string{"utm_source"},
					},
				},
			},
			wantErr: "include and exclude are mutually exclusive",
		},
		{
			name: "wrong target group",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef: gatewayparamsv1alpha1.PolicyTargetReference{
					Group: "apps",
					Kind:  "Deployment",
					Name:  "my-deploy",
				},
				DefaultTTL: durationPtr(60 * time.Second),
			},
			wantErr: "targetRef.group must be gateway.networking.k8s.io",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vcp := &gatewayparamsv1alpha1.VarnishCachePolicy{Spec: tt.spec}
			err := r.validateSpec(vcp)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if got := err.Error(); got != tt.wantErr && !contains(got, tt.wantErr) {
				t.Errorf("error = %q, want substring %q", got, tt.wantErr)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- TestSpecToCachePolicy ---

func TestSpecToCachePolicy(t *testing.T) {
	tests := []struct {
		name   string
		spec   gatewayparamsv1alpha1.VarnishCachePolicySpec
		verify func(t *testing.T, cp *ghost.CachePolicy)
	}{
		{
			name: "defaultTTL converts to seconds",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "r", nil),
				DefaultTTL: durationPtr(90 * time.Second),
			},
			verify: func(t *testing.T, cp *ghost.CachePolicy) {
				if cp.DefaultTTLSeconds == nil || *cp.DefaultTTLSeconds != 90 {
					t.Errorf("DefaultTTLSeconds = %v, want 90", cp.DefaultTTLSeconds)
				}
				if cp.ForcedTTLSeconds != nil {
					t.Errorf("ForcedTTLSeconds should be nil, got %v", *cp.ForcedTTLSeconds)
				}
			},
		},
		{
			name: "forcedTTL converts to seconds",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef: vcpTargetRef("HTTPRoute", "r", nil),
				ForcedTTL: durationPtr(5 * time.Minute),
			},
			verify: func(t *testing.T, cp *ghost.CachePolicy) {
				if cp.ForcedTTLSeconds == nil || *cp.ForcedTTLSeconds != 300 {
					t.Errorf("ForcedTTLSeconds = %v, want 300", cp.ForcedTTLSeconds)
				}
				if cp.DefaultTTLSeconds != nil {
					t.Errorf("DefaultTTLSeconds should be nil")
				}
			},
		},
		{
			name: "grace and keep",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "r", nil),
				DefaultTTL: durationPtr(60 * time.Second),
				Grace:      durationPtr(30 * time.Second),
				Keep:       durationPtr(2 * time.Hour),
			},
			verify: func(t *testing.T, cp *ghost.CachePolicy) {
				if cp.GraceSeconds != 30 {
					t.Errorf("GraceSeconds = %d, want 30", cp.GraceSeconds)
				}
				if cp.KeepSeconds != 7200 {
					t.Errorf("KeepSeconds = %d, want 7200", cp.KeepSeconds)
				}
			},
		},
		{
			name: "requestCoalescing defaults to true",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "r", nil),
				DefaultTTL: durationPtr(60 * time.Second),
			},
			verify: func(t *testing.T, cp *ghost.CachePolicy) {
				if !cp.RequestCoalescing {
					t.Error("RequestCoalescing should default to true")
				}
			},
		},
		{
			name: "requestCoalescing explicitly disabled",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:         vcpTargetRef("HTTPRoute", "r", nil),
				DefaultTTL:        durationPtr(60 * time.Second),
				RequestCoalescing: boolPtr(false),
			},
			verify: func(t *testing.T, cp *ghost.CachePolicy) {
				if cp.RequestCoalescing {
					t.Error("RequestCoalescing should be false")
				}
			},
		},
		{
			name: "cacheKey with headers",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "r", nil),
				DefaultTTL: durationPtr(60 * time.Second),
				CacheKey: &gatewayparamsv1alpha1.CacheKeySpec{
					Headers: []string{"Accept-Language", "X-Region"},
				},
			},
			verify: func(t *testing.T, cp *ghost.CachePolicy) {
				if cp.CacheKey == nil {
					t.Fatal("CacheKey should not be nil")
				}
				if len(cp.CacheKey.Headers) != 2 {
					t.Fatalf("Headers len = %d, want 2", len(cp.CacheKey.Headers))
				}
				if cp.CacheKey.Headers[0] != "Accept-Language" || cp.CacheKey.Headers[1] != "X-Region" {
					t.Errorf("Headers = %v", cp.CacheKey.Headers)
				}
			},
		},
		{
			name: "cacheKey with query params include",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "r", nil),
				DefaultTTL: durationPtr(60 * time.Second),
				CacheKey: &gatewayparamsv1alpha1.CacheKeySpec{
					QueryParameters: &gatewayparamsv1alpha1.QueryParameterKeySpec{
						Include: []string{"page", "sort"},
					},
				},
			},
			verify: func(t *testing.T, cp *ghost.CachePolicy) {
				if cp.CacheKey == nil {
					t.Fatal("CacheKey should not be nil")
				}
				if len(cp.CacheKey.QueryParamsInclude) != 2 {
					t.Fatalf("QueryParamsInclude len = %d, want 2", len(cp.CacheKey.QueryParamsInclude))
				}
			},
		},
		{
			name: "cacheKey with query params exclude",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "r", nil),
				DefaultTTL: durationPtr(60 * time.Second),
				CacheKey: &gatewayparamsv1alpha1.CacheKeySpec{
					QueryParameters: &gatewayparamsv1alpha1.QueryParameterKeySpec{
						Exclude: []string{"utm_source", "fbclid"},
					},
				},
			},
			verify: func(t *testing.T, cp *ghost.CachePolicy) {
				if cp.CacheKey == nil {
					t.Fatal("CacheKey should not be nil")
				}
				if len(cp.CacheKey.QueryParamsExclude) != 2 {
					t.Fatalf("QueryParamsExclude len = %d, want 2", len(cp.CacheKey.QueryParamsExclude))
				}
			},
		},
		{
			name: "bypass headers",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "r", nil),
				DefaultTTL: durationPtr(60 * time.Second),
				Bypass: &gatewayparamsv1alpha1.BypassSpec{
					Headers: []gatewayparamsv1alpha1.HeaderBypassCondition{
						{Name: "Authorization"},
						{Name: "Cookie", ValueRegex: "session=.*"},
					},
				},
			},
			verify: func(t *testing.T, cp *ghost.CachePolicy) {
				if len(cp.BypassHeaders) != 2 {
					t.Fatalf("BypassHeaders len = %d, want 2", len(cp.BypassHeaders))
				}
				if cp.BypassHeaders[0].Name != "Authorization" {
					t.Errorf("BypassHeaders[0].Name = %q", cp.BypassHeaders[0].Name)
				}
				if cp.BypassHeaders[1].ValueRegex != "session=.*" {
					t.Errorf("BypassHeaders[1].ValueRegex = %q", cp.BypassHeaders[1].ValueRegex)
				}
			},
		},
		{
			name: "nil cacheKey produces nil",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "r", nil),
				DefaultTTL: durationPtr(60 * time.Second),
			},
			verify: func(t *testing.T, cp *ghost.CachePolicy) {
				if cp.CacheKey != nil {
					t.Error("CacheKey should be nil when not specified")
				}
			},
		},
		{
			name: "empty cacheKey produces nil",
			spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "r", nil),
				DefaultTTL: durationPtr(60 * time.Second),
				CacheKey:   &gatewayparamsv1alpha1.CacheKeySpec{},
			},
			verify: func(t *testing.T, cp *ghost.CachePolicy) {
				if cp.CacheKey != nil {
					t.Error("empty CacheKey should produce nil")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := specToCachePolicy(&tt.spec)
			if cp == nil {
				t.Fatal("specToCachePolicy returned nil")
			}
			tt.verify(t, cp)
		})
	}
}

// --- TestIsVCPAccepted ---

func TestIsVCPAccepted(t *testing.T) {
	tests := []struct {
		name string
		vcp  *gatewayparamsv1alpha1.VarnishCachePolicy
		want bool
	}{
		{
			name: "accepted condition true",
			vcp: &gatewayparamsv1alpha1.VarnishCachePolicy{
				Status: gatewayparamsv1alpha1.VarnishCachePolicyStatus{
					Ancestors: []gatewayparamsv1alpha1.VarnishCachePolicyAncestorStatus{
						{
							ControllerName: "varnish",
							Conditions: []metav1.Condition{
								{Type: "Accepted", Status: metav1.ConditionTrue},
							},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "accepted condition false",
			vcp: &gatewayparamsv1alpha1.VarnishCachePolicy{
				Status: gatewayparamsv1alpha1.VarnishCachePolicyStatus{
					Ancestors: []gatewayparamsv1alpha1.VarnishCachePolicyAncestorStatus{
						{
							ControllerName: "varnish",
							Conditions: []metav1.Condition{
								{Type: "Accepted", Status: metav1.ConditionFalse},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "no status (never reconciled) should not be accepted",
			vcp:  &gatewayparamsv1alpha1.VarnishCachePolicy{},
			want: false,
		},
		{
			name: "ancestors present but no Accepted condition",
			vcp: &gatewayparamsv1alpha1.VarnishCachePolicy{
				Status: gatewayparamsv1alpha1.VarnishCachePolicyStatus{
					Ancestors: []gatewayparamsv1alpha1.VarnishCachePolicyAncestorStatus{
						{
							ControllerName: "varnish",
							Conditions:     []metav1.Condition{},
						},
					},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isVCPAccepted(tt.vcp)
			if got != tt.want {
				t.Errorf("isVCPAccepted() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- TestIsOlder ---

func TestIsOlder(t *testing.T) {
	now := metav1.Now()
	later := metav1.NewTime(now.Add(time.Second))

	tests := []struct {
		name string
		a, b *gatewayparamsv1alpha1.VarnishCachePolicy
		want bool
	}{
		{
			name: "a is older by timestamp",
			a:    &gatewayparamsv1alpha1.VarnishCachePolicy{ObjectMeta: metav1.ObjectMeta{Name: "b", CreationTimestamp: now}},
			b:    &gatewayparamsv1alpha1.VarnishCachePolicy{ObjectMeta: metav1.ObjectMeta{Name: "a", CreationTimestamp: later}},
			want: true,
		},
		{
			name: "b is older by timestamp",
			a:    &gatewayparamsv1alpha1.VarnishCachePolicy{ObjectMeta: metav1.ObjectMeta{Name: "a", CreationTimestamp: later}},
			b:    &gatewayparamsv1alpha1.VarnishCachePolicy{ObjectMeta: metav1.ObjectMeta{Name: "b", CreationTimestamp: now}},
			want: false,
		},
		{
			name: "same timestamp, a has smaller name",
			a:    &gatewayparamsv1alpha1.VarnishCachePolicy{ObjectMeta: metav1.ObjectMeta{Name: "alpha", CreationTimestamp: now}},
			b:    &gatewayparamsv1alpha1.VarnishCachePolicy{ObjectMeta: metav1.ObjectMeta{Name: "beta", CreationTimestamp: now}},
			want: true,
		},
		{
			name: "same timestamp, a has larger name",
			a:    &gatewayparamsv1alpha1.VarnishCachePolicy{ObjectMeta: metav1.ObjectMeta{Name: "beta", CreationTimestamp: now}},
			b:    &gatewayparamsv1alpha1.VarnishCachePolicy{ObjectMeta: metav1.ObjectMeta{Name: "alpha", CreationTimestamp: now}},
			want: false,
		},
		{
			name: "same timestamp, same name",
			a:    &gatewayparamsv1alpha1.VarnishCachePolicy{ObjectMeta: metav1.ObjectMeta{Name: "same", CreationTimestamp: now}},
			b:    &gatewayparamsv1alpha1.VarnishCachePolicy{ObjectMeta: metav1.ObjectMeta{Name: "same", CreationTimestamp: now}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOlder(tt.a, tt.b); got != tt.want {
				t.Errorf("isOlder() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- TestSortVCPsByPrecedence ---

func TestSortVCPsByPrecedence(t *testing.T) {
	now := metav1.Now()
	t1 := metav1.NewTime(now.Add(-3 * time.Second))
	t2 := metav1.NewTime(now.Add(-2 * time.Second))
	t3 := metav1.NewTime(now.Add(-1 * time.Second))

	vcps := []gatewayparamsv1alpha1.VarnishCachePolicy{
		{ObjectMeta: metav1.ObjectMeta{Name: "charlie", CreationTimestamp: t3}},
		{ObjectMeta: metav1.ObjectMeta{Name: "alpha", CreationTimestamp: t1}},
		{ObjectMeta: metav1.ObjectMeta{Name: "bravo", CreationTimestamp: t2}},
	}

	SortVCPsByPrecedence(vcps)

	expected := []string{"alpha", "bravo", "charlie"}
	for i, name := range expected {
		if vcps[i].Name != name {
			t.Errorf("position %d: got %q, want %q", i, vcps[i].Name, name)
		}
	}
}

func TestSortVCPsByPrecedence_SameTimestamp(t *testing.T) {
	ts := metav1.Now()

	vcps := []gatewayparamsv1alpha1.VarnishCachePolicy{
		{ObjectMeta: metav1.ObjectMeta{Name: "charlie", CreationTimestamp: ts}},
		{ObjectMeta: metav1.ObjectMeta{Name: "alpha", CreationTimestamp: ts}},
		{ObjectMeta: metav1.ObjectMeta{Name: "bravo", CreationTimestamp: ts}},
	}

	SortVCPsByPrecedence(vcps)

	expected := []string{"alpha", "bravo", "charlie"}
	for i, name := range expected {
		if vcps[i].Name != name {
			t.Errorf("position %d: got %q, want %q", i, vcps[i].Name, name)
		}
	}
}

// --- TestReconcile ---

func TestReconcile_TargetNotFound_HTTPRoute(t *testing.T) {
	scheme := newTestScheme()
	vcp := newVCP("my-vcp", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("HTTPRoute", "nonexistent", nil),
		DefaultTTL: durationPtr(60 * time.Second),
	})

	r := newVCPTestReconciler(scheme, vcp)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-vcp", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Errorf("RequeueAfter = %v, want 10s", result.RequeueAfter)
	}

	// Verify status condition
	var updated gatewayparamsv1alpha1.VarnishCachePolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-vcp", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get VCP: %v", err)
	}
	assertVCPCondition(t, &updated, metav1.ConditionFalse, "TargetNotFound")
}

func TestReconcile_TargetNotFound_Gateway(t *testing.T) {
	scheme := newTestScheme()
	vcp := newVCP("my-vcp", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("Gateway", "nonexistent", nil),
		DefaultTTL: durationPtr(60 * time.Second),
	})

	r := newVCPTestReconciler(scheme, vcp)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-vcp", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Errorf("RequeueAfter = %v, want 10s", result.RequeueAfter)
	}

	var updated gatewayparamsv1alpha1.VarnishCachePolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-vcp", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get VCP: %v", err)
	}
	assertVCPCondition(t, &updated, metav1.ConditionFalse, "TargetNotFound")
}

func TestReconcile_InvalidSpec(t *testing.T) {
	scheme := newTestScheme()
	// Both TTLs set — invalid
	vcp := newVCP("my-vcp", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
		DefaultTTL: durationPtr(60 * time.Second),
		ForcedTTL:  durationPtr(300 * time.Second),
	})

	r := newVCPTestReconciler(scheme, vcp)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-vcp", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("should not requeue on validation error, got RequeueAfter=%v", result.RequeueAfter)
	}

	var updated gatewayparamsv1alpha1.VarnishCachePolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-vcp", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get VCP: %v", err)
	}
	assertVCPCondition(t, &updated, metav1.ConditionFalse, "Invalid")
}

func TestReconcile_UnsupportedKind(t *testing.T) {
	scheme := newTestScheme()
	vcp := newVCP("my-vcp", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef: gatewayparamsv1alpha1.PolicyTargetReference{
			Group: "gateway.networking.k8s.io",
			Kind:  "TCPRoute",
			Name:  "my-tcp",
		},
		DefaultTTL: durationPtr(60 * time.Second),
	})

	r := newVCPTestReconciler(scheme, vcp)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-vcp", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated gatewayparamsv1alpha1.VarnishCachePolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-vcp", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get VCP: %v", err)
	}
	assertVCPCondition(t, &updated, metav1.ConditionFalse, "Invalid")
}

func TestReconcile_HappyPath_HTTPRoute(t *testing.T) {
	scheme := newTestScheme()
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "my-route", Namespace: "default"},
		Spec:       gatewayv1.HTTPRouteSpec{},
	}
	vcp := newVCP("my-vcp", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
		DefaultTTL: durationPtr(60 * time.Second),
	})

	r := newVCPTestReconciler(scheme, route, vcp)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-vcp", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("should not requeue on success, got RequeueAfter=%v", result.RequeueAfter)
	}

	var updated gatewayparamsv1alpha1.VarnishCachePolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-vcp", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get VCP: %v", err)
	}
	assertVCPCondition(t, &updated, metav1.ConditionTrue, "Accepted")
}

func TestReconcile_HappyPath_Gateway(t *testing.T) {
	scheme := newTestScheme()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
			Listeners: []gatewayv1.Listener{{
				Name:     "http",
				Port:     80,
				Protocol: gatewayv1.HTTPProtocolType,
			}},
		},
	}
	vcp := newVCP("my-vcp", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("Gateway", "my-gw", nil),
		DefaultTTL: durationPtr(60 * time.Second),
	})

	r := newVCPTestReconciler(scheme, gw, vcp)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-vcp", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("should not requeue on success")
	}

	var updated gatewayparamsv1alpha1.VarnishCachePolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-vcp", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get VCP: %v", err)
	}
	assertVCPCondition(t, &updated, metav1.ConditionTrue, "Accepted")
}

func TestReconcile_HappyPath_RuleLevel(t *testing.T) {
	scheme := newTestScheme()
	ruleName := gatewayv1.SectionName("my-rule")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "my-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{Name: &ruleName},
			},
		},
	}
	vcp := newVCP("my-vcp", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("HTTPRoute", "my-route", stringPtr("my-rule")),
		DefaultTTL: durationPtr(60 * time.Second),
	})

	r := newVCPTestReconciler(scheme, route, vcp)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-vcp", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated gatewayparamsv1alpha1.VarnishCachePolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-vcp", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get VCP: %v", err)
	}
	assertVCPCondition(t, &updated, metav1.ConditionTrue, "Accepted")
}

func TestReconcile_InvalidSectionName(t *testing.T) {
	scheme := newTestScheme()
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "my-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{}, // unnamed rule
			},
		},
	}
	vcp := newVCP("my-vcp", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("HTTPRoute", "my-route", stringPtr("nonexistent-rule")),
		DefaultTTL: durationPtr(60 * time.Second),
	})

	r := newVCPTestReconciler(scheme, route, vcp)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-vcp", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated gatewayparamsv1alpha1.VarnishCachePolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-vcp", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get VCP: %v", err)
	}
	assertVCPCondition(t, &updated, metav1.ConditionFalse, "TargetNotFound")
}

func TestReconcile_Conflict(t *testing.T) {
	scheme := newTestScheme()
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "my-route", Namespace: "default"},
	}

	now := metav1.Now()
	older := &gatewayparamsv1alpha1.VarnishCachePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "older-vcp",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(now.Add(-time.Minute)),
		},
		Spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
			TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
			DefaultTTL: durationPtr(60 * time.Second),
		},
	}
	newer := &gatewayparamsv1alpha1.VarnishCachePolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "newer-vcp",
			Namespace:         "default",
			CreationTimestamp: now,
		},
		Spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
			TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
			ForcedTTL:  durationPtr(300 * time.Second),
		},
	}

	r := newVCPTestReconciler(scheme, route, older, newer)

	// Reconcile the newer VCP — it should be conflicted
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "newer-vcp", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated gatewayparamsv1alpha1.VarnishCachePolicy
	if err := r.Get(context.Background(), types.NamespacedName{Name: "newer-vcp", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get VCP: %v", err)
	}
	assertVCPCondition(t, &updated, metav1.ConditionFalse, "Conflicted")

	// Reconcile the older VCP — it should be accepted
	_, err = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "older-vcp", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := r.Get(context.Background(), types.NamespacedName{Name: "older-vcp", Namespace: "default"}, &updated); err != nil {
		t.Fatalf("failed to get VCP: %v", err)
	}
	assertVCPCondition(t, &updated, metav1.ConditionTrue, "Accepted")
}

func TestReconcile_NoConflict_DifferentTargets(t *testing.T) {
	scheme := newTestScheme()
	route1 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route-1", Namespace: "default"},
	}
	route2 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route-2", Namespace: "default"},
	}

	vcp1 := newVCP("vcp-1", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("HTTPRoute", "route-1", nil),
		DefaultTTL: durationPtr(60 * time.Second),
	})
	vcp2 := newVCP("vcp-2", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("HTTPRoute", "route-2", nil),
		DefaultTTL: durationPtr(120 * time.Second),
	})

	r := newVCPTestReconciler(scheme, route1, route2, vcp1, vcp2)

	// Both should be accepted
	for _, name := range []string{"vcp-1", "vcp-2"} {
		_, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
		})
		if err != nil {
			t.Fatalf("unexpected error for %s: %v", name, err)
		}

		var updated gatewayparamsv1alpha1.VarnishCachePolicy
		if err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &updated); err != nil {
			t.Fatalf("failed to get VCP %s: %v", name, err)
		}
		assertVCPCondition(t, &updated, metav1.ConditionTrue, "Accepted")
	}
}

func TestReconcile_NoConflict_DifferentSectionNames(t *testing.T) {
	scheme := newTestScheme()
	rule1 := gatewayv1.SectionName("rule-1")
	rule2 := gatewayv1.SectionName("rule-2")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "my-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{Name: &rule1},
				{Name: &rule2},
			},
		},
	}

	vcp1 := newVCP("vcp-1", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("HTTPRoute", "my-route", stringPtr("rule-1")),
		DefaultTTL: durationPtr(60 * time.Second),
	})
	vcp2 := newVCP("vcp-2", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("HTTPRoute", "my-route", stringPtr("rule-2")),
		DefaultTTL: durationPtr(120 * time.Second),
	})

	r := newVCPTestReconciler(scheme, route, vcp1, vcp2)

	for _, name := range []string{"vcp-1", "vcp-2"} {
		_, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
		})
		if err != nil {
			t.Fatalf("unexpected error for %s: %v", name, err)
		}

		var updated gatewayparamsv1alpha1.VarnishCachePolicy
		if err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &updated); err != nil {
			t.Fatalf("failed to get VCP %s: %v", name, err)
		}
		assertVCPCondition(t, &updated, metav1.ConditionTrue, "Accepted")
	}
}

func TestReconcile_VCPNotFound(t *testing.T) {
	scheme := newTestScheme()
	r := newVCPTestReconciler(scheme)

	// Reconciling a VCP that doesn't exist should return no error
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gone", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("should not requeue for deleted VCP")
	}
}

// --- TestResolveCachePolicyForRoute ---

func TestResolveCachePolicyForRoute(t *testing.T) {
	scheme := newTestScheme()

	makeAcceptedVCP := func(name string, spec gatewayparamsv1alpha1.VarnishCachePolicySpec, ts metav1.Time) *gatewayparamsv1alpha1.VarnishCachePolicy {
		return &gatewayparamsv1alpha1.VarnishCachePolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         "default",
				CreationTimestamp: ts,
			},
			Spec: spec,
			Status: gatewayparamsv1alpha1.VarnishCachePolicyStatus{
				Ancestors: []gatewayparamsv1alpha1.VarnishCachePolicyAncestorStatus{
					{
						ControllerName: ControllerName,
						Conditions: []metav1.Condition{
							{Type: "Accepted", Status: metav1.ConditionTrue},
						},
					},
				},
			},
		}
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "my-route", Namespace: "default"},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
	}

	t.Run("no VCPs returns nil", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		cp := ResolveCachePolicyForRoute(context.Background(), c, route, gateway, "")
		if cp != nil {
			t.Fatal("expected nil, got policy")
		}
	})

	t.Run("gateway-level VCP applies", func(t *testing.T) {
		vcp := makeAcceptedVCP("gw-vcp", gatewayparamsv1alpha1.VarnishCachePolicySpec{
			TargetRef:  vcpTargetRef("Gateway", "my-gw", nil),
			DefaultTTL: durationPtr(120 * time.Second),
		}, metav1.Now())

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(vcp).Build()
		cp := ResolveCachePolicyForRoute(context.Background(), c, route, gateway, "")
		if cp == nil {
			t.Fatal("expected policy, got nil")
		}
		if cp.DefaultTTLSeconds == nil || *cp.DefaultTTLSeconds != 120 {
			t.Errorf("DefaultTTLSeconds = %v, want 120", cp.DefaultTTLSeconds)
		}
	})

	t.Run("route-level VCP overrides gateway-level", func(t *testing.T) {
		now := metav1.Now()
		gwVCP := makeAcceptedVCP("gw-vcp", gatewayparamsv1alpha1.VarnishCachePolicySpec{
			TargetRef:  vcpTargetRef("Gateway", "my-gw", nil),
			DefaultTTL: durationPtr(120 * time.Second),
		}, now)
		routeVCP := makeAcceptedVCP("route-vcp", gatewayparamsv1alpha1.VarnishCachePolicySpec{
			TargetRef: vcpTargetRef("HTTPRoute", "my-route", nil),
			ForcedTTL: durationPtr(60 * time.Second),
		}, now)

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(gwVCP, routeVCP).Build()
		cp := ResolveCachePolicyForRoute(context.Background(), c, route, gateway, "")
		if cp == nil {
			t.Fatal("expected policy, got nil")
		}
		if cp.ForcedTTLSeconds == nil || *cp.ForcedTTLSeconds != 60 {
			t.Errorf("ForcedTTLSeconds = %v, want 60", cp.ForcedTTLSeconds)
		}
		if cp.DefaultTTLSeconds != nil {
			t.Error("should not have DefaultTTLSeconds from gateway VCP")
		}
	})

	t.Run("rule-level VCP overrides route-level", func(t *testing.T) {
		now := metav1.Now()
		routeVCP := makeAcceptedVCP("route-vcp", gatewayparamsv1alpha1.VarnishCachePolicySpec{
			TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
			DefaultTTL: durationPtr(120 * time.Second),
		}, now)
		ruleVCP := makeAcceptedVCP("rule-vcp", gatewayparamsv1alpha1.VarnishCachePolicySpec{
			TargetRef: vcpTargetRef("HTTPRoute", "my-route", stringPtr("rule-0")),
			ForcedTTL: durationPtr(30 * time.Second),
		}, now)

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(routeVCP, ruleVCP).Build()
		cp := ResolveCachePolicyForRoute(context.Background(), c, route, gateway, "rule-0")
		if cp == nil {
			t.Fatal("expected policy, got nil")
		}
		if cp.ForcedTTLSeconds == nil || *cp.ForcedTTLSeconds != 30 {
			t.Errorf("ForcedTTLSeconds = %v, want 30", cp.ForcedTTLSeconds)
		}
	})

	t.Run("non-accepted VCP is skipped", func(t *testing.T) {
		notAccepted := &gatewayparamsv1alpha1.VarnishCachePolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "not-accepted",
				Namespace:         "default",
				CreationTimestamp: metav1.Now(),
			},
			Spec: gatewayparamsv1alpha1.VarnishCachePolicySpec{
				TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
				DefaultTTL: durationPtr(60 * time.Second),
			},
			Status: gatewayparamsv1alpha1.VarnishCachePolicyStatus{
				Ancestors: []gatewayparamsv1alpha1.VarnishCachePolicyAncestorStatus{
					{
						ControllerName: ControllerName,
						Conditions: []metav1.Condition{
							{Type: "Accepted", Status: metav1.ConditionFalse, Reason: "Conflicted"},
						},
					},
				},
			},
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(notAccepted).Build()
		cp := ResolveCachePolicyForRoute(context.Background(), c, route, gateway, "")
		if cp != nil {
			t.Fatal("expected nil for non-accepted VCP")
		}
	})

	t.Run("rule-level VCP does not match different rule name", func(t *testing.T) {
		ruleVCP := makeAcceptedVCP("rule-vcp", gatewayparamsv1alpha1.VarnishCachePolicySpec{
			TargetRef:  vcpTargetRef("HTTPRoute", "my-route", stringPtr("rule-0")),
			DefaultTTL: durationPtr(60 * time.Second),
		}, metav1.Now())

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(ruleVCP).Build()
		cp := ResolveCachePolicyForRoute(context.Background(), c, route, gateway, "rule-1")
		if cp != nil {
			t.Fatal("expected nil for mismatched rule name")
		}
	})

	t.Run("oldest VCP wins at same precedence level", func(t *testing.T) {
		now := metav1.Now()
		olderVCP := makeAcceptedVCP("aaa-vcp", gatewayparamsv1alpha1.VarnishCachePolicySpec{
			TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
			DefaultTTL: durationPtr(60 * time.Second),
		}, metav1.NewTime(now.Add(-time.Minute)))
		newerVCP := makeAcceptedVCP("zzz-vcp", gatewayparamsv1alpha1.VarnishCachePolicySpec{
			TargetRef: vcpTargetRef("HTTPRoute", "my-route", nil),
			ForcedTTL: durationPtr(300 * time.Second),
		}, now)

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(olderVCP, newerVCP).Build()
		cp := ResolveCachePolicyForRoute(context.Background(), c, route, gateway, "")
		if cp == nil {
			t.Fatal("expected policy, got nil")
		}
		if cp.DefaultTTLSeconds == nil || *cp.DefaultTTLSeconds != 60 {
			t.Errorf("should use older VCP's defaultTTL=60, got %v", cp.DefaultTTLSeconds)
		}
	})

	t.Run("nil gateway still resolves route-level VCP", func(t *testing.T) {
		routeVCP := makeAcceptedVCP("route-vcp", gatewayparamsv1alpha1.VarnishCachePolicySpec{
			TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
			DefaultTTL: durationPtr(60 * time.Second),
		}, metav1.Now())

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(routeVCP).Build()
		cp := ResolveCachePolicyForRoute(context.Background(), c, route, nil, "")
		if cp == nil {
			t.Fatal("expected policy, got nil")
		}
	})
}

// --- TestFindVCPsForHTTPRoute / TestFindVCPsForGateway ---

func TestFindVCPsForHTTPRoute(t *testing.T) {
	scheme := newTestScheme()
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "my-route", Namespace: "default"},
	}
	vcp1 := newVCP("vcp-match", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
		DefaultTTL: durationPtr(60 * time.Second),
	})
	vcp2 := newVCP("vcp-other", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("HTTPRoute", "other-route", nil),
		DefaultTTL: durationPtr(60 * time.Second),
	})
	vcp3 := newVCP("vcp-gw", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("Gateway", "my-gw", nil),
		DefaultTTL: durationPtr(60 * time.Second),
	})

	r := newVCPTestReconciler(scheme, route, vcp1, vcp2, vcp3)
	requests := r.findVCPsForHTTPRoute(context.Background(), route)

	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "vcp-match" {
		t.Errorf("expected vcp-match, got %s", requests[0].Name)
	}
}

func TestFindVCPsForGateway(t *testing.T) {
	scheme := newTestScheme()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
			Listeners: []gatewayv1.Listener{{
				Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
			}},
		},
	}
	vcp1 := newVCP("vcp-gw", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("Gateway", "my-gw", nil),
		DefaultTTL: durationPtr(60 * time.Second),
	})
	vcp2 := newVCP("vcp-route", "default", gatewayparamsv1alpha1.VarnishCachePolicySpec{
		TargetRef:  vcpTargetRef("HTTPRoute", "my-route", nil),
		DefaultTTL: durationPtr(60 * time.Second),
	})

	r := newVCPTestReconciler(scheme, gw, vcp1, vcp2)
	requests := r.findVCPsForGateway(context.Background(), gw)

	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "vcp-gw" {
		t.Errorf("expected vcp-gw, got %s", requests[0].Name)
	}
}

func TestFindVCPsForHTTPRoute_WrongType(t *testing.T) {
	scheme := newTestScheme()
	r := newVCPTestReconciler(scheme)

	// Pass a Gateway instead of HTTPRoute
	gw := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"}}
	requests := r.findVCPsForHTTPRoute(context.Background(), gw)
	if requests != nil {
		t.Errorf("expected nil for wrong type, got %v", requests)
	}
}

func TestFindVCPsForGateway_WrongType(t *testing.T) {
	scheme := newTestScheme()
	r := newVCPTestReconciler(scheme)

	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "default"}}
	requests := r.findVCPsForGateway(context.Background(), route)
	if requests != nil {
		t.Errorf("expected nil for wrong type, got %v", requests)
	}
}

// --- helpers ---

func assertVCPCondition(t *testing.T, vcp *gatewayparamsv1alpha1.VarnishCachePolicy, status metav1.ConditionStatus, reason string) {
	t.Helper()
	if len(vcp.Status.Ancestors) == 0 {
		t.Fatal("no ancestors in status")
	}
	conditions := vcp.Status.Ancestors[0].Conditions
	if len(conditions) == 0 {
		t.Fatal("no conditions in ancestor status")
	}
	cond := conditions[0]
	if cond.Type != "Accepted" {
		t.Errorf("condition type = %q, want Accepted", cond.Type)
	}
	if cond.Status != status {
		t.Errorf("condition status = %q, want %q", cond.Status, status)
	}
	if cond.Reason != reason {
		t.Errorf("condition reason = %q, want %q", cond.Reason, reason)
	}
}
