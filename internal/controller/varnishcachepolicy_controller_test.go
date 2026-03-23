package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
)

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
