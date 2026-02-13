package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

func newRefGrantScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = gatewayv1.Install(scheme)
	_ = gatewayv1beta1.Install(scheme)
	return scheme
}

func TestIsReferenceAllowed(t *testing.T) {
	secretName := gatewayv1beta1.ObjectName("my-cert")

	tests := []struct {
		name    string
		grants  []gatewayv1beta1.ReferenceGrant
		ref     CrossNamespaceRef
		want    bool
		wantErr bool
	}{
		{
			name:   "no grants returns false",
			grants: nil,
			ref: CrossNamespaceRef{
				FromGroup: "gateway.networking.k8s.io", FromKind: "Gateway", FromNamespace: "ns-a",
				ToGroup: "", ToKind: "Secret", ToNamespace: "ns-b", ToName: "my-cert",
			},
			want: false,
		},
		{
			name: "matching grant with wildcard name returns true",
			grants: []gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "allow-gw", Namespace: "ns-b"},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{Group: "gateway.networking.k8s.io", Kind: "Gateway", Namespace: "ns-a"},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{Group: "", Kind: "Secret"},
						},
					},
				},
			},
			ref: CrossNamespaceRef{
				FromGroup: "gateway.networking.k8s.io", FromKind: "Gateway", FromNamespace: "ns-a",
				ToGroup: "", ToKind: "Secret", ToNamespace: "ns-b", ToName: "my-cert",
			},
			want: true,
		},
		{
			name: "matching grant with specific name returns true",
			grants: []gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "allow-specific", Namespace: "ns-b"},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{Group: "gateway.networking.k8s.io", Kind: "Gateway", Namespace: "ns-a"},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{Group: "", Kind: "Secret", Name: &secretName},
						},
					},
				},
			},
			ref: CrossNamespaceRef{
				FromGroup: "gateway.networking.k8s.io", FromKind: "Gateway", FromNamespace: "ns-a",
				ToGroup: "", ToKind: "Secret", ToNamespace: "ns-b", ToName: "my-cert",
			},
			want: true,
		},
		{
			name: "grant with wrong name returns false",
			grants: []gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "allow-other", Namespace: "ns-b"},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{Group: "gateway.networking.k8s.io", Kind: "Gateway", Namespace: "ns-a"},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{Group: "", Kind: "Secret", Name: ptr(gatewayv1beta1.ObjectName("other-cert"))},
						},
					},
				},
			},
			ref: CrossNamespaceRef{
				FromGroup: "gateway.networking.k8s.io", FromKind: "Gateway", FromNamespace: "ns-a",
				ToGroup: "", ToKind: "Secret", ToNamespace: "ns-b", ToName: "my-cert",
			},
			want: false,
		},
		{
			name: "grant for wrong from-namespace returns false",
			grants: []gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "allow-wrong-ns", Namespace: "ns-b"},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{Group: "gateway.networking.k8s.io", Kind: "Gateway", Namespace: "ns-c"},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{Group: "", Kind: "Secret"},
						},
					},
				},
			},
			ref: CrossNamespaceRef{
				FromGroup: "gateway.networking.k8s.io", FromKind: "Gateway", FromNamespace: "ns-a",
				ToGroup: "", ToKind: "Secret", ToNamespace: "ns-b", ToName: "my-cert",
			},
			want: false,
		},
		{
			name: "grant for wrong from-kind returns false",
			grants: []gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "allow-httproute", Namespace: "ns-b"},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Namespace: "ns-a"},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{Group: "", Kind: "Secret"},
						},
					},
				},
			},
			ref: CrossNamespaceRef{
				FromGroup: "gateway.networking.k8s.io", FromKind: "Gateway", FromNamespace: "ns-a",
				ToGroup: "", ToKind: "Secret", ToNamespace: "ns-b", ToName: "my-cert",
			},
			want: false,
		},
		{
			name: "multiple from/to entries with OR semantics",
			grants: []gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "multi-entry", Namespace: "ns-b"},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{Group: "gateway.networking.k8s.io", Kind: "HTTPRoute", Namespace: "ns-c"},
							{Group: "gateway.networking.k8s.io", Kind: "Gateway", Namespace: "ns-a"},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{Group: "", Kind: "Service"},
							{Group: "", Kind: "Secret"},
						},
					},
				},
			},
			ref: CrossNamespaceRef{
				FromGroup: "gateway.networking.k8s.io", FromKind: "Gateway", FromNamespace: "ns-a",
				ToGroup: "", ToKind: "Secret", ToNamespace: "ns-b", ToName: "my-cert",
			},
			want: true,
		},
		{
			name: "grant in wrong namespace is not matched",
			grants: []gatewayv1beta1.ReferenceGrant{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "wrong-ns-grant", Namespace: "ns-c"},
					Spec: gatewayv1beta1.ReferenceGrantSpec{
						From: []gatewayv1beta1.ReferenceGrantFrom{
							{Group: "gateway.networking.k8s.io", Kind: "Gateway", Namespace: "ns-a"},
						},
						To: []gatewayv1beta1.ReferenceGrantTo{
							{Group: "", Kind: "Secret"},
						},
					},
				},
			},
			ref: CrossNamespaceRef{
				FromGroup: "gateway.networking.k8s.io", FromKind: "Gateway", FromNamespace: "ns-a",
				ToGroup: "", ToKind: "Secret", ToNamespace: "ns-b", ToName: "my-cert",
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newRefGrantScheme()
			var objs []runtime.Object
			for i := range tc.grants {
				objs = append(objs, &tc.grants[i])
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithRuntimeObjects(objs...).
				Build()

			got, err := IsReferenceAllowed(context.Background(), c, tc.ref)
			if (err != nil) != tc.wantErr {
				t.Fatalf("IsReferenceAllowed() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("IsReferenceAllowed() = %v, want %v", got, tc.want)
			}
		})
	}
}
