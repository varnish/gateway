package controller

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
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

	// Chaperone health port
	chaperoneHealthPort = 8081
)

// listenerSocketName returns the Varnish socket name for a Gateway listener.
// Format: {proto}-{port}, e.g. "http-80", "https-443"
func listenerSocketName(listener *gatewayv1.Listener) string {
	proto := "http"
	if listener.Protocol == gatewayv1.HTTPSProtocolType || listener.Protocol == gatewayv1.TLSProtocolType {
		proto = "https"
	}
	return fmt.Sprintf("%s-%d", proto, listener.Port)
}

// listenerSpecs returns a deterministic string representation of all listener ports and protocols.
// Format: sorted comma-separated list of socket names, e.g. "http-80,https-443"
func listenerSpecs(gateway *gatewayv1.Gateway) string {
	seen := make(map[string]bool)
	var specs []string
	for i := range gateway.Spec.Listeners {
		name := listenerSocketName(&gateway.Spec.Listeners[i])
		if !seen[name] {
			seen[name] = true
			specs = append(specs, name)
		}
	}
	sort.Strings(specs)
	return strings.Join(specs, ",")
}

// hasHTTPSListener returns true if any listener uses HTTPS or TLS protocol.
func hasHTTPSListener(gateway *gatewayv1.Gateway) bool {
	for _, l := range gateway.Spec.Listeners {
		if l.Protocol == gatewayv1.HTTPSProtocolType || l.Protocol == gatewayv1.TLSProtocolType {
			return true
		}
	}
	return false
}

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
	if _, err := rand.Read(secretBytes); err != nil {
		panic(fmt.Sprintf("crypto/rand.Read: %v", err))
	}
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
func (r *GatewayReconciler) buildDeployment(gateway *gatewayv1.Gateway, varnishdExtraArgs []string, logging *gatewayparamsv1alpha1.VarnishLogging, infraHash string, extraVolumes []corev1.Volume, extraVolumeMounts []corev1.VolumeMount, extraInitContainers []corev1.Container, resources *corev1.ResourceRequirements) *appsv1.Deployment {
	labels := r.buildLabels(gateway)
	replicas := int32(1) // TODO: get from GatewayClassParameters

	// Rolling update strategy for zero-downtime deployments
	maxUnavailable := intstr.FromInt(0) // Never reduce available pods during update
	maxSurge := intstr.FromInt(1)       // Create new pod before removing old

	// Termination grace period for graceful shutdown
	terminationGracePeriod := int64(30)

	// Build image pull secrets from shared parsing logic used for infra hashing.
	secretNames := r.parseImagePullSecrets()
	imagePullSecrets := make([]corev1.LocalObjectReference, 0, len(secretNames))
	for _, name := range secretNames {
		imagePullSecrets = append(imagePullSecrets, corev1.LocalObjectReference{Name: name})
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
					NodeSelector: map[string]string{
						"kubernetes.io/arch": "amd64",
					},
					InitContainers: extraInitContainers,
					Containers:     r.buildContainers(gateway, varnishdExtraArgs, logging, extraVolumeMounts, resources),
					Volumes:        r.buildVolumes(gateway, extraVolumes),
				},
			},
		},
	}
}

// buildVolumes creates the pod volumes, including TLS cert volume if HTTPS is enabled.
func (r *GatewayReconciler) buildVolumes(gateway *gatewayv1.Gateway, extra []corev1.Volume) []corev1.Volume {
	hasTLS := hasHTTPSListener(gateway)
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

	volumes = append(volumes, extra...)

	return volumes
}

// buildContainers creates the pod containers: main gateway container and optional logging sidecar.
func (r *GatewayReconciler) buildContainers(gateway *gatewayv1.Gateway, varnishdExtraArgs []string, logging *gatewayparamsv1alpha1.VarnishLogging, extraVolumeMounts []corev1.VolumeMount, resources *corev1.ResourceRequirements) []corev1.Container {
	containers := []corev1.Container{
		r.buildGatewayContainer(gateway, varnishdExtraArgs, extraVolumeMounts, resources),
	}

	// Add logging sidecar if configured
	if logging != nil && logging.Mode != "" {
		containers = append(containers, r.buildLoggingSidecar(gateway, logging))
	}

	return containers
}

// buildGatewayContainer creates the combined varnish-gateway container specification.
// This container runs chaperone which manages varnishd internally.
func (r *GatewayReconciler) buildGatewayContainer(gateway *gatewayv1.Gateway, varnishdExtraArgs []string, extraVolumeMounts []corev1.VolumeMount, resources *corev1.ResourceRequirements) corev1.Container {
	hasTLS := hasHTTPSListener(gateway)

	// Build VARNISH_LISTEN from all listeners, deduplicating by port.
	// Format: semicolon-separated list of {proto}-{port}=:{port},{proto} entries.
	// Example: http-80=:80,http;https-443=:443,https
	type listenEntry struct {
		socketName string
		port       int32
		proto      string
	}
	seenPorts := make(map[int32]bool)
	var entries []listenEntry
	for i := range gateway.Spec.Listeners {
		l := &gateway.Spec.Listeners[i]
		port := int32(l.Port)
		if seenPorts[port] {
			continue
		}
		seenPorts[port] = true
		proto := "http"
		if l.Protocol == gatewayv1.HTTPSProtocolType || l.Protocol == gatewayv1.TLSProtocolType {
			proto = "https"
		}
		entries = append(entries, listenEntry{
			socketName: listenerSocketName(l),
			port:       port,
			proto:      proto,
		})
	}

	// Build VARNISH_LISTEN value
	// Always include a loopback-only HTTP listener for ghost reload.
	// This avoids sending plain HTTP reload requests to HTTPS listeners.
	const ghostReloadPort = 1969
	var listenParts []string
	listenParts = append(listenParts, fmt.Sprintf("ghost-reload=127.0.0.1:%d,http", ghostReloadPort))
	for _, e := range entries {
		listenParts = append(listenParts, fmt.Sprintf("%s=:%d,%s", e.socketName, e.port, e.proto))
	}
	varnishListen := strings.Join(listenParts, ";")

	// VARNISH_HTTP_ADDR: always use the dedicated ghost reload listener
	httpAddr := fmt.Sprintf("localhost:%d", ghostReloadPort)

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
		{Name: "VARNISH_HTTP_ADDR", Value: httpAddr},
		{Name: "VARNISH_LISTEN", Value: varnishListen},
		{Name: "VARNISH_STORAGE", Value: "malloc,256m"},
		{Name: "VCL_PATH", Value: "/etc/varnish/main.vcl"},
		{Name: "CONFIGMAP_NAME", Value: fmt.Sprintf("%s-vcl", gateway.Name)},
		{Name: "GHOST_CONFIG_PATH", Value: "/var/run/varnish/ghost.json"},
		{Name: "WORK_DIR", Value: "/var/run/varnish"},
		{Name: "VARNISH_DIR", Value: "/var/run/varnish/vsm"}, // VSM subdirectory on shared volume
		{Name: "HEALTH_ADDR", Value: fmt.Sprintf(":%d", chaperoneHealthPort)},
	}

	// Add varnishd extra args if specified (semicolon-separated)
	if len(varnishdExtraArgs) > 0 {
		env = append(env, corev1.EnvVar{
			Name:  "VARNISHD_EXTRA_ARGS",
			Value: strings.Join(varnishdExtraArgs, ";"),
		})
	}

	// Add TLS cert dir if any HTTPS listener exists
	if hasTLS {
		env = append(env,
			corev1.EnvVar{Name: "TLS_CERT_DIR", Value: "/etc/varnish/tls"},
		)
	}

	// Generate container ports dynamically from unique listener ports
	ports := []corev1.ContainerPort{
		{
			Name:          "health",
			ContainerPort: int32(chaperoneHealthPort),
			Protocol:      corev1.ProtocolTCP,
		},
	}
	for _, e := range entries {
		ports = append(ports, corev1.ContainerPort{
			Name:          e.socketName,
			ContainerPort: e.port,
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
	volumeMounts = append(volumeMounts, extraVolumeMounts...)

	// Apply resource requirements: use user-specified or sensible defaults.
	// Defaults are requests only (no limits) because Varnish memory usage
	// varies enormously by deployment.
	resourceReqs := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    mustParseQuantity("100m"),
			corev1.ResourceMemory: mustParseQuantity("256Mi"),
		},
	}
	if resources != nil {
		resourceReqs = *resources
	}

	return corev1.Container{
		Name:  "varnish-gateway",
		Image: r.Config.GatewayImage,
		Env:   env,
		SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{"IPC_LOCK", "NET_BIND_SERVICE"},
			},
		},
		Ports:        ports,
		VolumeMounts: volumeMounts,
		Resources:    resourceReqs,
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
					Port: intstr.FromInt(chaperoneHealthPort),
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
	// Container ports = listener ports (no translation).
	var ports []corev1.ServicePort
	seenPorts := make(map[int32]bool)
	for i := range gateway.Spec.Listeners {
		l := &gateway.Spec.Listeners[i]
		port := int32(l.Port)
		if seenPorts[port] {
			continue
		}
		seenPorts[port] = true
		ports = append(ports, corev1.ServicePort{
			Name:       listenerSocketName(l),
			Port:       port,
			TargetPort: intstr.FromInt(int(port)),
			Protocol:   corev1.ProtocolTCP,
		})
	}

	// Default to http-80 on port 80 if no listeners
	if len(ports) == 0 {
		ports = []corev1.ServicePort{
			{
				Name:       "http-80",
				Port:       80,
				TargetPort: intstr.FromInt(80),
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
