package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GatewayClassParameters contains configuration for a GatewayClass.
// This is a cluster-scoped resource referenced by GatewayClass.Spec.ParametersRef.
//
// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
type GatewayClassParameters struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GatewayClassParametersSpec `json:"spec,omitempty"`
}

// GatewayClassParametersSpec defines the desired state of GatewayClassParameters.
type GatewayClassParametersSpec struct {
	// UserVCLConfigMapRef references a ConfigMap containing user VCL.
	// The ConfigMap should have the VCL content in a key (default "user.vcl").
	// The user VCL will be appended to the generated VCL using VCL's
	// subroutine concatenation feature.
	// +optional
	UserVCLConfigMapRef *ConfigMapReference `json:"userVCLConfigMapRef,omitempty"`

	// VarnishdExtraArgs specifies additional command-line arguments to pass to varnishd.
	// Each element is a separate argument (e.g., ["-p", "thread_pools=4"]).
	// Protected arguments (-M, -S, -F, -f, -n) cannot be overridden as they are
	// controlled by the operator.
	// +optional
	VarnishdExtraArgs []string `json:"varnishdExtraArgs,omitempty"`
}

// ConfigMapReference is a reference to a ConfigMap in a specific namespace.
type ConfigMapReference struct {
	// Name is the name of the ConfigMap.
	Name string `json:"name"`

	// Namespace is the namespace of the ConfigMap.
	// Required since GatewayClassParameters is cluster-scoped.
	Namespace string `json:"namespace"`

	// Key is the key in the ConfigMap data containing the VCL.
	// Defaults to "user.vcl" if not specified.
	// +optional
	Key string `json:"key,omitempty"`
}

// GatewayClassParametersList contains a list of GatewayClassParameters.
//
// +kubebuilder:object:root=true
type GatewayClassParametersList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GatewayClassParameters `json:"items"`
}
