package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CacheInvalidation is a one-shot resource that triggers cache purge or ban
// on a Varnish Gateway. Each chaperone pod serving the target gateway processes
// the invalidation independently and reports its result. The phase transitions
// to Complete only when all pods have reported success.
//
// PURGE removes a single cached object by exact URL.
// BAN invalidates objects matching a URL pattern (regex) using Varnish's ban lurker.
//
// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.spec.hostname`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type CacheInvalidation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CacheInvalidationSpec   `json:"spec"`
	Status CacheInvalidationStatus `json:"status,omitempty"`
}

// CacheInvalidationType defines the type of cache invalidation.
// +kubebuilder:validation:Enum=Purge;Ban
type CacheInvalidationType string

const (
	// CacheInvalidationPurge removes a single cached object by exact URL.
	// Varnish looks up the object by Host + URL and removes it from cache.
	CacheInvalidationPurge CacheInvalidationType = "Purge"

	// CacheInvalidationBan invalidates objects matching a URL regex pattern.
	// Uses Varnish's ban lurker for efficient background invalidation.
	CacheInvalidationBan CacheInvalidationType = "Ban"
)

// CacheInvalidationSpec defines the desired cache invalidation.
type CacheInvalidationSpec struct {
	// GatewayRef identifies which Gateway's cache to invalidate.
	GatewayRef GatewayReference `json:"gatewayRef"`

	// Type is the invalidation method: Purge (exact URL) or Ban (URL pattern).
	Type CacheInvalidationType `json:"type"`

	// Hostname is the Host header value for the invalidation request.
	Hostname string `json:"hostname"`

	// Path is the URL path for the invalidation.
	// For Purge: exact path of the cached object (e.g., "/api/users/123").
	// For Ban: regex pattern matching cached URLs (e.g., "/api/.*").
	Path string `json:"path"`

	// TTL is how long to keep this resource after all pods have completed.
	// The resource is eligible for garbage collection after this duration.
	// Default: 1h
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`
}

// GatewayReference identifies a Gateway resource.
type GatewayReference struct {
	// Name is the name of the Gateway.
	Name string `json:"name"`

	// Namespace is the namespace of the Gateway.
	// Defaults to the CacheInvalidation's namespace if not specified.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// CacheInvalidationPhase describes the current state of the invalidation.
type CacheInvalidationPhase string

const (
	// CacheInvalidationPending means no pod has processed the invalidation yet.
	CacheInvalidationPending CacheInvalidationPhase = "Pending"

	// CacheInvalidationInProgress means at least one pod has processed it, but not all.
	CacheInvalidationInProgress CacheInvalidationPhase = "InProgress"

	// CacheInvalidationComplete means all pods have successfully processed the invalidation.
	CacheInvalidationComplete CacheInvalidationPhase = "Complete"

	// CacheInvalidationFailed means one or more pods failed to process the invalidation.
	CacheInvalidationFailed CacheInvalidationPhase = "Failed"
)

// PodResult records the outcome of an invalidation on a single pod.
type PodResult struct {
	// PodName is the name of the chaperone pod that processed this invalidation.
	PodName string `json:"podName"`

	// Success indicates whether the invalidation succeeded on this pod.
	Success bool `json:"success"`

	// Message provides details about the result (error message on failure).
	// +optional
	Message string `json:"message,omitempty"`

	// CompletedAt is when this pod completed processing.
	CompletedAt metav1.Time `json:"completedAt"`
}

// CacheInvalidationStatus defines the observed state of a CacheInvalidation.
type CacheInvalidationStatus struct {
	// Phase is the aggregate state across all pods.
	// +optional
	Phase CacheInvalidationPhase `json:"phase,omitempty"`

	// CompletedAt is when the last pod reported, completing the invalidation.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// PodResults contains per-pod outcomes.
	// +optional
	PodResults []PodResult `json:"podResults,omitempty"`
}

// CacheInvalidationList contains a list of CacheInvalidation.
//
// +kubebuilder:object:root=true
type CacheInvalidationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CacheInvalidation `json:"items"`
}
