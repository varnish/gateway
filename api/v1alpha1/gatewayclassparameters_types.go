package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
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

	// Logging configures varnish log output via a sidecar container.
	// When enabled, a sidecar container runs varnishlog or varnishncsa,
	// streaming logs to stdout where they're captured by Kubernetes.
	// +optional
	Logging *VarnishLogging `json:"logging,omitempty"`

	// ExtraVolumes specifies additional volumes to add to the varnish pod.
	// +optional
	ExtraVolumes []corev1.Volume `json:"extraVolumes,omitempty"`

	// ExtraVolumeMounts specifies additional volume mounts for the main varnish-gateway container.
	// +optional
	ExtraVolumeMounts []corev1.VolumeMount `json:"extraVolumeMounts,omitempty"`

	// ExtraInitContainers specifies additional init containers to run before the main container.
	// +optional
	ExtraInitContainers []corev1.Container `json:"extraInitContainers,omitempty"`
}

// VarnishLogging configures varnish logging via a sidecar container.
type VarnishLogging struct {
	// Mode determines which varnish logging tool to use.
	// Valid values: "varnishlog", "varnishncsa"
	// Future: "varnishlog-json" when available
	// +kubebuilder:validation:Enum=varnishlog;varnishncsa
	Mode string `json:"mode"`

	// Format specifies the output format for varnishncsa.
	// Only used when mode is "varnishncsa".
	// Example: "%h %l %u %t \"%r\" %s %b"
	// See varnishncsa(1) for format specification.
	// +optional
	Format string `json:"format,omitempty"`

	// ExtraArgs specifies additional arguments to pass to the logging tool.
	// Each element is a separate argument (e.g., ["-g", "request", "-q", "ReqURL ~ \"/api\""])
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`

	// Image specifies the container image containing varnish logging tools.
	// Defaults to the same image as the gateway if not specified.
	// +optional
	Image string `json:"image,omitempty"`
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
