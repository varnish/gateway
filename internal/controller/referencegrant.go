package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// CrossNamespaceRef describes a cross-namespace reference that needs
// ReferenceGrant validation.
type CrossNamespaceRef struct {
	// From fields describe the referrer.
	FromGroup     string
	FromKind      string
	FromNamespace string

	// To fields describe the target.
	ToGroup     string
	ToKind      string
	ToNamespace string
	ToName      string
}

// IsReferenceAllowed checks whether a cross-namespace reference is permitted
// by a ReferenceGrant in the target namespace. Per the Gateway API spec,
// From and To entries within a single ReferenceGrant are OR-combined:
// any matching From + any matching To in the same grant is sufficient.
func IsReferenceAllowed(ctx context.Context, c client.Client, ref CrossNamespaceRef) (bool, error) {
	var grants gatewayv1beta1.ReferenceGrantList
	if err := c.List(ctx, &grants, client.InNamespace(ref.ToNamespace)); err != nil {
		return false, err
	}

	for _, grant := range grants.Items {
		if grantAllows(&grant, ref) {
			return true, nil
		}
	}
	return false, nil
}

// grantAllows returns true if the given ReferenceGrant permits the reference.
func grantAllows(grant *gatewayv1beta1.ReferenceGrant, ref CrossNamespaceRef) bool {
	fromMatch := false
	for _, from := range grant.Spec.From {
		if string(from.Group) == ref.FromGroup &&
			string(from.Kind) == ref.FromKind &&
			string(from.Namespace) == ref.FromNamespace {
			fromMatch = true
			break
		}
	}
	if !fromMatch {
		return false
	}

	for _, to := range grant.Spec.To {
		if string(to.Group) == ref.ToGroup && string(to.Kind) == ref.ToKind {
			// Name is optional — nil means "all resources of this kind"
			if to.Name == nil || string(*to.Name) == ref.ToName {
				return true
			}
		}
	}
	return false
}
