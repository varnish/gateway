package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// API preview: VarnishVCLPolicy is wired end to end (operator assigns slots,
// gateway pods compile and label the VCL, the operator reports Programmed),
// but it has not yet been load-tested or conformance-verified with the
// feature on.

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

	// VCLPolicyConditionProgrammed reports whether the route VCL holds a
	// slot on every parent Gateway with route VCL enabled, and has compiled
	// and been labelled on every live pod of those Gateways.
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

	// VCLPolicyReasonProgrammed is used with Programmed=True: every live
	// pod of every route-VCL-enabled parent Gateway reports the current
	// document compiled and labelled.
	VCLPolicyReasonProgrammed = "Programmed"
	// VCLPolicyReasonPending is used with Programmed=False while some live
	// pods have not reported the current document yet.
	VCLPolicyReasonPending = "Pending"
	// VCLPolicyReasonCompileFailed is used with Programmed=False when a live
	// pod reports that the current document failed to compile or label.
	VCLPolicyReasonCompileFailed = "CompileFailed"
	// VCLPolicyReasonNoSlotAvailable is used with Programmed=False when a
	// parent Gateway could not give the policy a slot (pool exhausted, pool
	// shrunk below the policy's slot, or the route VCL ConfigMap is full).
	VCLPolicyReasonNoSlotAvailable = "NoSlotAvailable"
	// VCLPolicyReasonNotProgrammed is used with Programmed=Unknown when no
	// parent Gateway lists the policy in its route VCL manifest yet.
	VCLPolicyReasonNotProgrammed = "NotProgrammed"
	// VCLPolicyReasonNotAccepted is used with Programmed=False when the
	// policy is not Accepted.
	VCLPolicyReasonNotAccepted = "NotAccepted"
)

// VarnishVCLPolicy attaches a VCL document to an HTTPRoute. Requests matched
// by the route are handled by that VCL, composed with the gateway's own VCL.
//
// API preview: wired end to end, not yet load-tested or conformance-verified
// with the feature on.
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
// +kubebuilder:printcolumn:name="Programmed",type=string,JSONPath=`.status.ancestors[0].conditions[?(@.type=="Programmed")].status`
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

	// Conditions describe the status of the policy: Accepted (the policy is
	// valid and route VCL is enabled) and Programmed (the VCL runs on every
	// gateway pod).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VarnishVCLPolicyStatus defines the observed state of VarnishVCLPolicy.
type VarnishVCLPolicyStatus struct {
	// Ancestors is the list of ancestor objects that this policy is relevant for.
	// +optional
	Ancestors []VarnishVCLPolicyAncestorStatus `json:"ancestors,omitempty"`

	// Slots lists the VCL slot the policy occupies on each parent Gateway
	// that assigned it one. A slot is stable: it is kept while the policy
	// stays eligible, even when its document changes.
	// +optional
	// +listType=map
	// +listMapKey=gateway
	Slots []VarnishVCLPolicySlot `json:"slots,omitempty"`

	// PodResults holds one entry per gateway pod that has the policy's VCL
	// in its route VCL manifest, written by that pod's chaperone after each
	// attempt to compile and label the VCL.
	// +optional
	// +listType=map
	// +listMapKey=podName
	PodResults []VarnishVCLPolicyPodResult `json:"podResults,omitempty"`
}

// VarnishVCLPolicySlot is the slot a policy occupies on one Gateway.
type VarnishVCLPolicySlot struct {
	// Gateway is the Gateway as "<namespace>/<name>".
	Gateway string `json:"gateway"`

	// Slot is the slot number on that Gateway (0-based).
	Slot int32 `json:"slot"`
}

// VarnishVCLPolicyPodResult is one gateway pod's result for a policy's VCL.
type VarnishVCLPolicyPodResult struct {
	// PodName is the gateway pod that reported this result.
	PodName string `json:"podName"`

	// Slot is the VCL slot the policy occupies on the pod's gateway, or -1
	// when the slot in the route VCL manifest was not a number.
	Slot int32 `json:"slot"`

	// Hash is the SHA-256 (hex) of the route VCL document this result refers
	// to, as listed in the route VCL manifest.
	Hash string `json:"hash"`

	// Programmed is true when the document compiled and its slot label points
	// at it, so matching requests run it.
	Programmed bool `json:"programmed"`

	// Message explains a false Programmed: the VCL compiler output, or why
	// the manifest entry was skipped.
	// +optional
	Message string `json:"message,omitempty"`

	// LastUpdate is when the pod last changed this entry.
	LastUpdate metav1.Time `json:"lastUpdate"`
}

// VarnishVCLPolicyList contains a list of VarnishVCLPolicy.
//
// +kubebuilder:object:root=true
type VarnishVCLPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VarnishVCLPolicy `json:"items"`
}
