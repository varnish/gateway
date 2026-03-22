package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VarnishCachePolicy configures caching behavior for routes through a Varnish Gateway.
// Without a VCP, all routes operate in pass-through mode (no caching).
// Attaching a VCP to a Gateway or HTTPRoute enables Varnish caching for those routes.
//
// VCP is an Inherited Policy per Gateway API conventions:
// - VCP on Gateway: default for all routes through that gateway
// - VCP on HTTPRoute: overrides gateway defaults for all rules in the route
// - VCP on HTTPRoute rule (via sectionName): overrides for that specific rule
// Most specific wins. Override is complete replacement, not field-level merging.
//
// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vcp
// +kubebuilder:printcolumn:name="Target Kind",type=string,JSONPath=`.spec.targetRef.kind`
// +kubebuilder:printcolumn:name="Target Name",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type VarnishCachePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VarnishCachePolicySpec   `json:"spec"`
	Status VarnishCachePolicyStatus `json:"status,omitempty"`
}

// VarnishCachePolicySpec defines the desired caching behavior.
type VarnishCachePolicySpec struct {
	// TargetRef identifies the Gateway or HTTPRoute this policy applies to.
	// For per-rule targeting, set sectionName to the rule's name.
	TargetRef PolicyTargetReference `json:"targetRef"`

	// DefaultTTL is used when the origin does NOT send Cache-Control headers.
	// Origin Cache-Control takes precedence. Mutually exclusive with ForcedTTL.
	// +optional
	DefaultTTL *metav1.Duration `json:"defaultTTL,omitempty"`

	// ForcedTTL overrides origin Cache-Control entirely.
	// Use when the origin misbehaves or you need operator-level control.
	// Mutually exclusive with DefaultTTL.
	// +optional
	ForcedTTL *metav1.Duration `json:"forcedTTL,omitempty"`

	// Grace is how long to serve stale content while asynchronously revalidating.
	// Equivalent to stale-while-revalidate in HTTP semantics.
	// Default: 0 (disabled)
	// +optional
	Grace *metav1.Duration `json:"grace,omitempty"`

	// Keep is how long to keep stale objects for serving when all backends are sick.
	// Equivalent to stale-if-error in HTTP semantics.
	// Default: 0 (disabled)
	// +optional
	Keep *metav1.Duration `json:"keep,omitempty"`

	// RequestCoalescing enables collapsed forwarding: when multiple clients request
	// the same uncached object simultaneously, only one request goes to the backend.
	// Default: true
	// +optional
	RequestCoalescing *bool `json:"requestCoalescing,omitempty"`

	// CacheKey customizes what makes a cache entry unique.
	// +optional
	CacheKey *CacheKeySpec `json:"cacheKey,omitempty"`

	// Bypass defines conditions under which caching is bypassed even when
	// this policy is active. Matching requests get pass-through behavior.
	// +optional
	Bypass *BypassSpec `json:"bypass,omitempty"`
}

// CacheKeySpec controls cache key composition.
type CacheKeySpec struct {
	// Headers lists request headers to include in the cache key.
	// Similar to Vary, but controlled by the operator, not the origin.
	// +optional
	Headers []string `json:"headers,omitempty"`

	// QueryParameters controls which query parameters are part of the cache key.
	// +optional
	QueryParameters *QueryParameterKeySpec `json:"queryParameters,omitempty"`
}

// QueryParameterKeySpec controls query parameter inclusion in cache keys.
// Include and Exclude are mutually exclusive.
type QueryParameterKeySpec struct {
	// Include is an allowlist: only these params matter for caching.
	// +optional
	Include []string `json:"include,omitempty"`

	// Exclude is a denylist: these params are excluded from the cache key.
	// +optional
	Exclude []string `json:"exclude,omitempty"`
}

// BypassSpec defines conditions that trigger cache bypass.
type BypassSpec struct {
	// Headers lists header conditions that trigger cache bypass.
	// +optional
	Headers []HeaderBypassCondition `json:"headers,omitempty"`
}

// HeaderBypassCondition defines a header-based cache bypass rule.
type HeaderBypassCondition struct {
	// Name is the header name to check.
	Name string `json:"name"`

	// ValueRegex is an optional regex pattern to match against the header value.
	// If omitted, any value (or presence) of the header triggers bypass.
	// +optional
	ValueRegex string `json:"valueRegex,omitempty"`
}

// PolicyTargetReference identifies a Gateway or HTTPRoute resource.
type PolicyTargetReference struct {
	// Group is the group of the target resource.
	Group string `json:"group"`

	// Kind is kind of the target resource.
	Kind string `json:"kind"`

	// Name is the name of the target resource.
	Name string `json:"name"`

	// SectionName targets a specific named rule within an HTTPRoute.
	// +optional
	SectionName *string `json:"sectionName,omitempty"`
}

// VarnishCachePolicyAncestorStatus describes the status of a policy with respect to an ancestor.
type VarnishCachePolicyAncestorStatus struct {
	// AncestorRef corresponds with a ParentRef in the spec that this
	// PolicyAncestorStatus struct describes the status of.
	AncestorRef PolicyTargetReference `json:"ancestorRef"`

	// ControllerName is the name of the controller that wrote this status.
	ControllerName string `json:"controllerName"`

	// Conditions describe the status of the policy.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VarnishCachePolicyStatus defines the observed state of VarnishCachePolicy.
type VarnishCachePolicyStatus struct {
	// Ancestors is the list of ancestor objects that this policy is relevant for.
	// +optional
	Ancestors []VarnishCachePolicyAncestorStatus `json:"ancestors,omitempty"`
}

// VarnishCachePolicyList contains a list of VarnishCachePolicy.
//
// +kubebuilder:object:root=true
type VarnishCachePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VarnishCachePolicy `json:"items"`
}
