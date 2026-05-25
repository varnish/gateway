package controller

import (
	corev1 "k8s.io/api/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
)

// Sentinel annotation keys recording which annotation and label keys the
// operator manages on the Service. Pruning on update consults these to avoid
// stomping on annotations added by cloud controllers, service-mesh sidecar
// injectors, observability tooling, etc.
const (
	AnnotationManagedAnnotations = "varnish.io/managed-annotations"
	AnnotationManagedLabels      = "varnish.io/managed-labels"
)

// ResolvedServiceConfig is the fully-defaulted, fully-merged Service shape
// the controller will apply to the Service object. Type is always non-nil
// (defaults to LoadBalancer); other pointer fields stay nil when omitted.
//
// Annotations and Labels are the merged result of GatewayClassParameters
// defaults + Gateway.spec.infrastructure overlay. The maps are never nil
// after resolution — they may be empty.
type ResolvedServiceConfig struct {
	Type                     corev1.ServiceType
	Annotations              map[string]string
	Labels                   map[string]string
	LoadBalancerClass        *string
	LoadBalancerSourceRanges []string
	ExternalTrafficPolicy    *corev1.ServiceExternalTrafficPolicy
}

// resolveServiceConfig computes the desired Service shape from the
// GatewayClass-level parameters and Gateway-level infrastructure overlay.
// Both inputs may be nil; the function always returns a usable config.
func resolveServiceConfig(gateway *gatewayv1.Gateway, params *gatewayparamsv1alpha1.GatewayClassParameters) ResolvedServiceConfig {
	out := ResolvedServiceConfig{
		Type:        corev1.ServiceTypeLoadBalancer,
		Annotations: map[string]string{},
		Labels:      map[string]string{},
	}

	if params != nil && params.Spec.Service != nil {
		svc := params.Spec.Service
		if svc.Type != nil {
			out.Type = *svc.Type
		}
		for k, v := range svc.Annotations {
			out.Annotations[k] = v
		}
		for k, v := range svc.Labels {
			out.Labels[k] = v
		}
		if svc.LoadBalancerClass != nil {
			lb := *svc.LoadBalancerClass
			out.LoadBalancerClass = &lb
		}
		if len(svc.LoadBalancerSourceRanges) > 0 {
			out.LoadBalancerSourceRanges = append([]string{}, svc.LoadBalancerSourceRanges...)
		}
		if svc.ExternalTrafficPolicy != nil {
			etp := *svc.ExternalTrafficPolicy
			out.ExternalTrafficPolicy = &etp
		}
	}

	// Gateway.spec.infrastructure overlay applies only to annotations and labels.
	if gateway != nil && gateway.Spec.Infrastructure != nil {
		for k, v := range gateway.Spec.Infrastructure.Annotations {
			out.Annotations[string(k)] = string(v)
		}
		for k, v := range gateway.Spec.Infrastructure.Labels {
			out.Labels[string(k)] = string(v)
		}
	}

	return out
}
