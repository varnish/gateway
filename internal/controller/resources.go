package controller

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
)

const (
	// Volume names
	volumeVCLConfig  = "vcl-config"
	volumeVarnishRun = "varnish-run"
	volumeTLSCerts   = "tls-certs"

	// Default port for Varnish HTTP
	varnishHTTPPort = 8080

	// Default port for Varnish HTTPS
	varnishHTTPSPort = 8443

	// Chaperone health port
	chaperoneHealthPort = 8081
)

// mustParseQuantity parses a resource quantity string and panics on error.
// Used for hardcoded resource values that should never fail.
func mustParseQuantity(s string) resource.Quantity {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		panic(fmt.Sprintf("invalid resource quantity %q: %v", s, err))
	}
	return q
}

// buildVCLConfigMap creates the ConfigMap containing VCL and routing.json.
// The vclContent parameter contains the generated VCL (ghost preamble + user VCL)
func (r *GatewayReconciler) buildVCLConfigMap(gateway *gatewayv1.Gateway, vclContent string) *corev1.ConfigMap {
	// Empty routing config initially (HTTPRoute controller will populate this)
	routingJSON := `{"version": 2, "vhosts": {}}`

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-vcl", gateway.Name),
			Namespace: gateway.Namespace,
			Labels:    r.buildLabels(gateway),
		},
		Data: map[string]string{
			"main.vcl":     vclContent,
			"routing.json": routingJSON,
		},
	}
}

// buildAdminSecret creates the Secret containing the varnishadm authentication secret.
func (r *GatewayReconciler) buildAdminSecret(gateway *gatewayv1.Gateway) *corev1.Secret {
	// Generate random secret for varnishadm authentication
	secretBytes := make([]byte, 32)
	_, _ = rand.Read(secretBytes)
	secretHex := hex.EncodeToString(secretBytes)

	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-secret", gateway.Name),
			Namespace: gateway.Namespace,
			Labels:    r.buildLabels(gateway),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"secret": []byte(secretHex),
		},
	}
}

// buildTLSSecret creates a Secret containing the combined PEM files for TLS termination.
// Each entry is keyed by {secret-name}.pem containing the concatenated cert + key.
func (r *GatewayReconciler) buildTLSSecret(gateway *gatewayv1.Gateway, certData map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-tls", gateway.Name),
			Namespace: gateway.Namespace,
			Labels:    r.buildLabels(gateway),
		},
		Type: corev1.SecretTypeOpaque,
		Data: certData,
	}
}

// buildServiceAccount creates the ServiceAccount for the chaperone.
func (r *GatewayReconciler) buildServiceAccount(gateway *gatewayv1.Gateway) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-chaperone", gateway.Name),
			Namespace: gateway.Namespace,
			Labels:    r.buildLabels(gateway),
		},
	}
}

// buildClusterRoleBinding creates a ClusterRoleBinding that grants the chaperone ServiceAccount
// permissions to watch EndpointSlices and ConfigMaps across the cluster.
func (r *GatewayReconciler) buildClusterRoleBinding(gateway *gatewayv1.Gateway) *rbacv1.ClusterRoleBinding {
	saName := fmt.Sprintf("%s-chaperone", gateway.Name)
	return &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1",
			Kind:       "ClusterRoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   fmt.Sprintf("%s-%s-chaperone", gateway.Namespace, gateway.Name),
			Labels: r.buildLabels(gateway),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "varnish-gateway-chaperone",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: gateway.Namespace,
			},
		},
	}
}

// buildDeployment creates the Deployment containing the combined varnish-gateway container.
// The container runs chaperone which manages the varnishd process internally.
// If logging is configured, a sidecar container is added to stream varnish logs.
// The infraHash is added as an annotation to trigger pod restarts when infrastructure config changes.
func (r *GatewayReconciler) buildDeployment(gateway *gatewayv1.Gateway, varnishdExtraArgs []string, logging *gatewayparamsv1alpha1.VarnishLogging, infraHash string, hasTLS bool) *appsv1.Deployment {
	labels := r.buildLabels(gateway)
	replicas := int32(1) // TODO: get from GatewayClassParameters

	// Rolling update strategy for zero-downtime deployments
	maxUnavailable := intstr.FromInt(0) // Never reduce available pods during update
	maxSurge := intstr.FromInt(1)       // Create new pod before removing old

	// Termination grace period for graceful shutdown
	terminationGracePeriod := int64(30)

	// Build image pull secrets from config
	var imagePullSecrets []corev1.LocalObjectReference
	if r.Config.ImagePullSecrets != "" {
		for _, name := range strings.Split(r.Config.ImagePullSecrets, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				imagePullSecrets = append(imagePullSecrets, corev1.LocalObjectReference{Name: name})
			}
		}
	}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      gateway.Name,
			Namespace: gateway.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &maxUnavailable,
					MaxSurge:       &maxSurge,
				},
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						AnnotationInfraHash: infraHash,
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:            fmt.Sprintf("%s-chaperone", gateway.Name),
					ImagePullSecrets:              imagePullSecrets,
					TerminationGracePeriodSeconds: &terminationGracePeriod,
					Containers: r.buildContainers(gateway, varnishdExtraArgs, logging, hasTLS),
					Volumes:    r.buildVolumes(gateway, hasTLS),
				},
			},
		},
	}
}

// buildVolumes creates the pod volumes, including TLS cert volume if HTTPS is enabled.
func (r *GatewayReconciler) buildVolumes(gateway *gatewayv1.Gateway, hasTLS bool) []corev1.Volume {
	volumes := []corev1.Volume{
		{
			Name: volumeVCLConfig,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: fmt.Sprintf("%s-vcl", gateway.Name),
					},
				},
			},
		},
		{
			Name: volumeVarnishRun,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	if hasTLS {
		volumes = append(volumes, corev1.Volume{
			Name: volumeTLSCerts,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: fmt.Sprintf("%s-tls", gateway.Name),
				},
			},
		})
	}

	return volumes
}

// buildContainers creates the pod containers: main gateway container and optional logging sidecar.
func (r *GatewayReconciler) buildContainers(gateway *gatewayv1.Gateway, varnishdExtraArgs []string, logging *gatewayparamsv1alpha1.VarnishLogging, hasTLS bool) []corev1.Container {
	containers := []corev1.Container{
		r.buildGatewayContainer(gateway, varnishdExtraArgs, hasTLS),
	}

	// Add logging sidecar if configured
	if logging != nil && logging.Mode != "" {
		containers = append(containers, r.buildLoggingSidecar(gateway, logging))
	}

	return containers
}

// buildGatewayContainer creates the combined varnish-gateway container specification.
// This container runs chaperone which manages varnishd internally.
func (r *GatewayReconciler) buildGatewayContainer(gateway *gatewayv1.Gateway, varnishdExtraArgs []string, hasTLS bool) corev1.Container {
	env := []corev1.EnvVar{
		{
			Name: "NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		},
		{Name: "VARNISH_ADMIN_PORT", Value: "6082"},
		{Name: "VARNISH_HTTP_ADDR", Value: fmt.Sprintf("localhost:%d", varnishHTTPPort)},
		{Name: "VARNISH_LISTEN", Value: fmt.Sprintf(":%d,http", varnishHTTPPort)},
		{Name: "VARNISH_STORAGE", Value: "malloc,256m"},
		{Name: "VCL_PATH", Value: "/etc/varnish/main.vcl"},
		{Name: "CONFIGMAP_NAME", Value: fmt.Sprintf("%s-vcl", gateway.Name)},
		{Name: "GHOST_CONFIG_PATH", Value: "/var/run/varnish/ghost.json"},
		{Name: "WORK_DIR", Value: "/var/run/varnish"},
		{Name: "VARNISH_DIR", Value: "/var/run/varnish/vsm"},  // VSM subdirectory on shared volume
		{Name: "HEALTH_ADDR", Value: fmt.Sprintf(":%d", chaperoneHealthPort)},
	}

	// Add varnishd extra args if specified (semicolon-separated)
	if len(varnishdExtraArgs) > 0 {
		env = append(env, corev1.EnvVar{
			Name:  "VARNISHD_EXTRA_ARGS",
			Value: strings.Join(varnishdExtraArgs, ";"),
		})
	}

	// Add TLS configuration if HTTPS listeners exist
	if hasTLS {
		env = append(env,
			corev1.EnvVar{Name: "VARNISH_TLS_LISTEN", Value: fmt.Sprintf(":%d,https", varnishHTTPSPort)},
			corev1.EnvVar{Name: "TLS_CERT_DIR", Value: "/etc/varnish/tls"},
		)
	}

	ports := []corev1.ContainerPort{
		{
			Name:          "http",
			ContainerPort: int32(varnishHTTPPort),
			Protocol:      corev1.ProtocolTCP,
		},
		{
			Name:          "health",
			ContainerPort: int32(chaperoneHealthPort),
			Protocol:      corev1.ProtocolTCP,
		},
	}
	if hasTLS {
		ports = append(ports, corev1.ContainerPort{
			Name:          "https",
			ContainerPort: int32(varnishHTTPSPort),
			Protocol:      corev1.ProtocolTCP,
		})
	}

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      volumeVCLConfig,
			MountPath: "/etc/varnish",
			ReadOnly:  true,
		},
		{
			Name:      volumeVarnishRun,
			MountPath: "/var/run/varnish",
		},
	}
	if hasTLS {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      volumeTLSCerts,
			MountPath: "/etc/varnish/tls",
			ReadOnly:  true,
		})
	}

	return corev1.Container{
		Name:  "varnish-gateway",
		Image: r.Config.GatewayImage,
		Env:   env,
		SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{"IPC_LOCK"},
			},
		},
		Ports:        ports,
		VolumeMounts: volumeMounts,
		// PreStop hook triggers graceful shutdown before SIGTERM
		Lifecycle: &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/drain",
					Port:   intstr.FromInt(chaperoneHealthPort),
					Scheme: corev1.URISchemeHTTP,
				},
			},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/health",
					Port:   intstr.FromInt(chaperoneHealthPort),
					Scheme: corev1.URISchemeHTTP,
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt(varnishHTTPPort),
				},
			},
			InitialDelaySeconds: 10,
			PeriodSeconds:       15,
		},
	}
}

// buildService creates the Service for the Gateway.
func (r *GatewayReconciler) buildService(gateway *gatewayv1.Gateway) *corev1.Service {
	labels := r.buildLabels(gateway)

	// Map Gateway listeners to Service ports, deduplicating by port number.
	// Multiple listeners can share the same port (differentiated by hostname),
	// but a Service only needs one entry per unique port.
	var ports []corev1.ServicePort
	seenPorts := make(map[int32]bool)
	for _, listener := range gateway.Spec.Listeners {
		port := int32(listener.Port)
		if seenPorts[port] {
			continue
		}
		seenPorts[port] = true
		targetPort := varnishHTTPPort
		if listener.Protocol == gatewayv1.HTTPSProtocolType {
			targetPort = varnishHTTPSPort
		}
		ports = append(ports, corev1.ServicePort{
			Name:       string(listener.Name),
			Port:       port,
			TargetPort: intstr.FromInt(targetPort),
			Protocol:   corev1.ProtocolTCP,
		})
	}

	// Default to port 80 if no listeners specified
	if len(ports) == 0 {
		ports = []corev1.ServicePort{
			{
				Name:       "http",
				Port:       80,
				TargetPort: intstr.FromInt(varnishHTTPPort),
				Protocol:   corev1.ProtocolTCP,
			},
		}
	}

	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      gateway.Name,
			Namespace: gateway.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: labels,
			Ports:    ports,
		},
	}
}

// buildLoggingSidecar creates a sidecar container for varnish logging.
// The sidecar runs varnishlog or varnishncsa, streaming logs to stdout
// where they're captured by Kubernetes logging infrastructure.
func (r *GatewayReconciler) buildLoggingSidecar(gateway *gatewayv1.Gateway, logging *gatewayparamsv1alpha1.VarnishLogging) corev1.Container {
	// Use the same image as the gateway unless logging.Image is specified
	image := r.Config.GatewayImage
	if logging.Image != "" {
		image = logging.Image
	}

	// Build command arguments
	// Use -t off to wait indefinitely for varnishd to become available
	command := []string{logging.Mode}
	args := []string{"-n", "/var/run/varnish/vsm", "-t", "off"}

	// Add format for varnishncsa
	if logging.Mode == "varnishncsa" && logging.Format != "" {
		args = append(args, "-F", logging.Format)
	}

	// Add extra args
	args = append(args, logging.ExtraArgs...)

	return corev1.Container{
		Name:    "varnish-log",
		Image:   image,
		Command: command,
		Args:    args,
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      volumeVarnishRun,
				MountPath: "/var/run/varnish",
				ReadOnly:  true, // Sidecar only reads varnishd shared memory
			},
		},
		// Resource limits for the logging sidecar
		// Logging is typically lightweight but can spike with high traffic
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    mustParseQuantity("10m"),
				corev1.ResourceMemory: mustParseQuantity("32Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    mustParseQuantity("100m"),
				corev1.ResourceMemory: mustParseQuantity("128Mi"),
			},
		},
	}
}
