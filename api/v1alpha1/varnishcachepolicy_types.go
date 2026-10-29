package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
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
	// A negative value (e.g. -1s) means "do not cache": matching responses
	// become hit-for-miss. Zero and values under 1s in magnitude are rejected.
	// +optional
	DefaultTTL *metav1.Duration `json:"defaultTTL,omitempty"`

	// ForcedTTL overrides origin Cache-Control entirely.
	// Use when the origin misbehaves or you need operator-level control.
	// Mutually exclusive with DefaultTTL.
	// A negative value (e.g. -1s) means "do not cache": matching responses
	// become hit-for-miss. Zero and values under 1s in magnitude are rejected.
	// +optional
	ForcedTTL *metav1.Duration `json:"forcedTTL,omitempty"`

	// Grace is how long to serve stale content while asynchronously revalidating.
	// Equivalent to stale-while-revalidate in HTTP semantics.
	// When unset, Varnish's default_grace (10s) is left untouched; an explicit
	// 0s sets grace to zero. Must be between 0s and 10 years (87600h).
	// Not applied to hit-for-miss (negative TTL) responses.
	// +optional
	Grace *metav1.Duration `json:"grace,omitempty"`

	// Keep is how long to keep stale objects for serving when all backends are sick.
	// Equivalent to stale-if-error in HTTP semantics.
	// When unset, Varnish's default_keep (0s) is left untouched; an explicit
	// 0s sets keep to zero. Must be between 0s and 10 years (87600h).
	// Not applied to hit-for-miss (negative TTL) responses.
	// +optional
	Keep *metav1.Duration `json:"keep,omitempty"`

	// CacheKey customizes what makes a cache entry unique.
	// +optional
	CacheKey *CacheKeySpec `json:"cacheKey,omitempty"`

	// Bypass defines conditions under which caching is bypassed even when
	// this policy is active. Matching requests get pass-through behavior.
	// +optional
	Bypass *BypassSpec `json:"bypass,omitempty"`

	// CacheRuleOverrides adjusts TTL, grace and keep for responses matching
	// request and/or response criteria. Rules are evaluated in order and the
	// first match wins; unset fields fall back to the policy-level values.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	CacheRuleOverrides []CacheRuleOverride `json:"cacheRuleOverrides,omitempty"`
}

// CacheRuleOverride overrides caching parameters for matching requests/responses.
// At least one of defaultTTL, forcedTTL, grace or keep must be set.
// defaultTTL and forcedTTL are mutually exclusive.
type CacheRuleOverride struct {
	// Match selects the requests/responses this rule applies to.
	// All specified criteria must match. An empty match matches everything.
	// +optional
	Match CacheRuleMatch `json:"match,omitempty"`

	// DefaultTTL is used when the origin does NOT send Cache-Control headers.
	// Mutually exclusive with ForcedTTL. If neither TTL field is set on this
	// rule, the top-level defaultTTL/forcedTTL pair applies (as a unit).
	// A negative value (e.g. -1s) means "do not cache": matching responses
	// become hit-for-miss. Zero and values under 1s in magnitude are rejected.
	// +optional
	DefaultTTL *metav1.Duration `json:"defaultTTL,omitempty"`

	// ForcedTTL overrides origin Cache-Control entirely.
	// Mutually exclusive with DefaultTTL. If neither TTL field is set on this
	// rule, the top-level defaultTTL/forcedTTL pair applies (as a unit).
	// A negative value (e.g. -1s) means "do not cache": matching responses
	// become hit-for-miss. Zero and values under 1s in magnitude are rejected.
	// +optional
	ForcedTTL *metav1.Duration `json:"forcedTTL,omitempty"`

	// Grace is how long to serve stale content while revalidating.
	// If unset, the top-level grace applies. Must be between 0s and 10 years
	// (87600h). Not applied to hit-for-miss (negative TTL) responses.
	// +optional
	Grace *metav1.Duration `json:"grace,omitempty"`

	// Keep is how long to keep stale objects for serving when backends are sick.
	// If unset, the top-level keep applies. Must be between 0s and 10 years
	// (87600h). Not applied to hit-for-miss (negative TTL) responses.
	// +optional
	Keep *metav1.Duration `json:"keep,omitempty"`
}

// CacheRuleMatch describes the criteria a CacheRuleOverride matches on.
type CacheRuleMatch struct {
	// Path matches the request path.
	// +optional
	Path *gatewayv1.HTTPPathMatch `json:"path,omitempty"`

	// Method matches the request method. Unlike HTTPRoute method matching, GET and
	// HEAD match each other: a rule with method GET also matches HEAD requests and
	// vice versa, because Varnish fetches a HEAD miss as GET and stores one object.
	// +optional
	Method *gatewayv1.HTTPMethod `json:"method,omitempty"`

	// RequestHeaders matches request headers.
	// +optional
	RequestHeaders []gatewayv1.HTTPHeaderMatch `json:"requestHeaders,omitempty"`

	// QueryParams matches request query parameters.
	// +optional
	QueryParams []gatewayv1.HTTPQueryParamMatch `json:"queryParams,omitempty"`

	// Status matches the response status: an exact code ("404"), a class
	// ("5xx") or an inclusive range ("500-503"). Any entry matching suffices.
	// +kubebuilder:validation:items:Pattern=`^([1-5][0-9]{2}|[1-5]xx|[1-5][0-9]{2}-[1-5][0-9]{2})$`
	// +optional
	Status []string `json:"status,omitempty"`

	// ResponseHeaders matches response headers.
	// +optional
	ResponseHeaders []gatewayv1.HTTPHeaderMatch `json:"responseHeaders,omitempty"`
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
