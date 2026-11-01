package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// API preview: VarnishVCLPolicy is not yet wired to the data plane. The
// operator validates policies and reports the Accepted condition, but no route
// VCL is compiled or delivered to gateway pods yet.

const (
	// DefaultRouteVCLKey is the ConfigMap key read when
	// VarnishVCLPolicy.spec.vclConfigMapRef.key is empty.
	DefaultRouteVCLKey = "route.vcl"

	// MaxRouteVCLDocumentBytes is the largest route VCL document the operator
	// accepts. All slots of a gateway are delivered in a single
	// "{gateway}-route-vcl" ConfigMap, which the API server caps at 1 MiB in
	// total, so a single document must stay well below that. 256 KiB lets a
	// handful of large documents coexist; the usual route VCL is a few KiB.
	MaxRouteVCLDocumentBytes = 256 * 1024
)

// Condition types and reasons for VarnishVCLPolicy.
const (
	// VCLPolicyConditionAccepted reports whether the policy is valid, its
	// references resolve, and route VCL is enabled for its target.
	VCLPolicyConditionAccepted = "Accepted"

	// VCLPolicyConditionProgrammed will report whether the route VCL has
	// compiled and been labelled on every gateway pod. It is not set yet; the
	// data-plane phases of the route VCL work set it.
	VCLPolicyConditionProgrammed = "Programmed"

	// VCLPolicyReasonAccepted is used with Accepted=True.
	VCLPolicyReasonAccepted = "Accepted"
	// VCLPolicyReasonInvalid is used when the spec is malformed: an
	// unsupported target group or kind, a sectionName, an empty ConfigMap
	// name, or a VCL document larger than MaxRouteVCLDocumentBytes.
	VCLPolicyReasonInvalid = "Invalid"
	// VCLPolicyReasonTargetNotFound is used when the target HTTPRoute does not
	// exist in the policy's namespace.
	VCLPolicyReasonTargetNotFound = "TargetNotFound"
	// VCLPolicyReasonConfigMapNotFound is used when the referenced ConfigMap,
	// or the key within it, does not exist.
	VCLPolicyReasonConfigMapNotFound = "ConfigMapNotFound"
	// VCLPolicyReasonConflicted is used when an older VarnishVCLPolicy
	// already targets the same HTTPRoute.
	VCLPolicyReasonConflicted = "Conflicted"
	// VCLPolicyReasonDisabled is used when none of the target HTTPRoute's
	// parent Gateways belongs to a GatewayClass whose GatewayClassParameters
	// set routeVCL.enabled=true.
	VCLPolicyReasonDisabled = "Disabled"
)

// VarnishVCLPolicy attaches a VCL document to an HTTPRoute. Requests matched
// by the route are handled by that VCL, composed with the gateway's own VCL.
//
// API preview: not yet wired to the data plane.
//
// Attaching VCL to an HTTPRoute is equivalent to granting code execution
// inside the gateway pod. The feature is off unless the GatewayClassParameters
// of the route's GatewayClass set routeVCL.enabled=true.
//
// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vvp
// +kubebuilder:printcolumn:name="Target Name",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.ancestors[0].conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type VarnishVCLPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VarnishVCLPolicySpec   `json:"spec"`
	Status VarnishVCLPolicyStatus `json:"status,omitempty"`
}

// VarnishVCLPolicySpec defines the VCL attached to an HTTPRoute.
type VarnishVCLPolicySpec struct {
	// TargetRef identifies the HTTPRoute this policy applies to. Only kind
	// HTTPRoute in group gateway.networking.k8s.io is accepted in this
	// version; the target must be in the policy's namespace. SectionName is
	// not supported and is rejected.
	TargetRef PolicyTargetReference `json:"targetRef"`

	// VCLConfigMapRef references a ConfigMap in the policy's own namespace
	// holding the VCL document. Cross-namespace references are refused.
	VCLConfigMapRef LocalConfigMapReference `json:"vclConfigMapRef"`
}

// LocalConfigMapReference references a key in a ConfigMap in the referring
// object's namespace.
type LocalConfigMapReference struct {
	// Name is the name of the ConfigMap.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key is the key in the ConfigMap data holding the VCL document.
	// Defaults to "route.vcl".
	// +optional
	Key string `json:"key,omitempty"`
}

// VarnishVCLPolicyAncestorStatus describes the status of a policy with respect
// to an ancestor.
type VarnishVCLPolicyAncestorStatus struct {
	// AncestorRef identifies the object this status entry describes.
	AncestorRef PolicyTargetReference `json:"ancestorRef"`

	// ControllerName is the name of the controller that wrote this status.
	ControllerName string `json:"controllerName"`

	// Conditions describe the status of the policy. Only Accepted is set in
	// this version; Programmed is reserved for when the VCL is delivered to
	// the data plane.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VarnishVCLPolicyStatus defines the observed state of VarnishVCLPolicy.
//
// Only conditions are reported for now. Slot assignment and per-pod compile
// results will be added here once policies are delivered to the data plane
// (route VCL phase P6).
type VarnishVCLPolicyStatus struct {
	// Ancestors is the list of ancestor objects that this policy is relevant for.
	// +optional
	Ancestors []VarnishVCLPolicyAncestorStatus `json:"ancestors,omitempty"`
}

// VarnishVCLPolicyList contains a list of VarnishVCLPolicy.
//
// +kubebuilder:object:root=true
type VarnishVCLPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VarnishVCLPolicy `json:"items"`
}
