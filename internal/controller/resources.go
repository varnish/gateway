package controller

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/varnish/gateway/internal/vcl"
)

const (
	// Volume names
	volumeVCLConfig     = "vcl-config"
	volumeVarnishRun    = "varnish-run"
	volumeVarnishSecret = "varnish-secret"

	// Default port for Varnish HTTP
	varnishHTTPPort = 8080

	// Sidecar health port
	sidecarHealthPort = 8081
)

// buildVCLConfigMap creates the ConfigMap containing VCL and services.json.
func (r *GatewayReconciler) buildVCLConfigMap(gateway *gatewayv1.Gateway) *corev1.ConfigMap {
	// Generate initial VCL with no routes (valid but empty routing)
	initialVCL := vcl.Generate(nil, vcl.GeneratorConfig{})

	// Empty services initially
	servicesJSON := `{"services": []}`

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
			"main.vcl":      initialVCL,
			"services.json": servicesJSON,
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

// buildServiceAccount creates the ServiceAccount for the sidecar.
func (r *GatewayReconciler) buildServiceAccount(gateway *gatewayv1.Gateway) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ServiceAccount",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-sidecar", gateway.Name),
			Namespace: gateway.Namespace,
			Labels:    r.buildLabels(gateway),
		},
	}
}

// buildDeployment creates the Deployment containing Varnish and sidecar containers.
func (r *GatewayReconciler) buildDeployment(gateway *gatewayv1.Gateway) *appsv1.Deployment {
	labels := r.buildLabels(gateway)
	replicas := int32(1) // TODO: get from GatewayClassParameters

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
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: fmt.Sprintf("%s-sidecar", gateway.Name),
					Containers: []corev1.Container{
						r.buildVarnishContainer(gateway),
						r.buildSidecarContainer(gateway),
					},
					Volumes: []corev1.Volume{
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
						{
							Name: volumeVarnishSecret,
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: fmt.Sprintf("%s-secret", gateway.Name),
								},
							},
						},
					},
				},
			},
		},
	}
}

// buildVarnishContainer creates the Varnish container specification.
func (r *GatewayReconciler) buildVarnishContainer(gateway *gatewayv1.Gateway) corev1.Container {
	return corev1.Container{
		Name:  "varnish",
		Image: r.Config.DefaultVarnishImage,
		Args: []string{
			"-F",
			"-f", "/etc/varnish/main.vcl",
			"-a", fmt.Sprintf(":%d", varnishHTTPPort),
			"-M", "localhost:6082", // Reverse CLI mode
			"-S", "/etc/varnish/secret",
			"-s", "malloc,256m",
		},
		Ports: []corev1.ContainerPort{
			{
				Name:          "http",
				ContainerPort: int32(varnishHTTPPort),
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      volumeVCLConfig,
				MountPath: "/etc/varnish",
				ReadOnly:  true,
			},
			{
				Name:      volumeVarnishRun,
				MountPath: "/var/run/varnish",
			},
			{
				Name:      volumeVarnishSecret,
				MountPath: "/etc/varnish/secret",
				SubPath:   "secret",
				ReadOnly:  true,
			},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.FromInt(varnishHTTPPort),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
	}
}

// buildSidecarContainer creates the sidecar container specification.
func (r *GatewayReconciler) buildSidecarContainer(gateway *gatewayv1.Gateway) corev1.Container {
	return corev1.Container{
		Name:  "sidecar",
		Image: r.Config.SidecarImage,
		Env: []corev1.EnvVar{
			{
				Name: "NAMESPACE",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.namespace",
					},
				},
			},
			{Name: "VARNISH_ADMIN_ADDR", Value: "localhost:6082"},
			{Name: "VARNISH_SECRET_PATH", Value: "/etc/varnish/secret"},
			{Name: "VCL_PATH", Value: "/etc/varnish/main.vcl"},
			{Name: "SERVICES_FILE_PATH", Value: "/etc/varnish/services.json"},
			{Name: "BACKENDS_FILE_PATH", Value: "/var/run/varnish/backends.conf"},
			{Name: "HEALTH_ADDR", Value: fmt.Sprintf(":%d", sidecarHealthPort)},
		},
		Ports: []corev1.ContainerPort{
			{
				Name:          "health",
				ContainerPort: int32(sidecarHealthPort),
				Protocol:      corev1.ProtocolTCP,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      volumeVCLConfig,
				MountPath: "/etc/varnish",
				ReadOnly:  true,
			},
			{
				Name:      volumeVarnishRun,
				MountPath: "/var/run/varnish",
			},
			{
				Name:      volumeVarnishSecret,
				MountPath: "/etc/varnish/secret",
				SubPath:   "secret",
				ReadOnly:  true,
			},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/health",
					Port:   intstr.FromInt(sidecarHealthPort),
					Scheme: corev1.URISchemeHTTP,
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
	}
}

// buildService creates the Service for the Gateway.
func (r *GatewayReconciler) buildService(gateway *gatewayv1.Gateway) *corev1.Service {
	labels := r.buildLabels(gateway)

	// Map Gateway listeners to Service ports
	var ports []corev1.ServicePort
	for _, listener := range gateway.Spec.Listeners {
		ports = append(ports, corev1.ServicePort{
			Name:       string(listener.Name),
			Port:       int32(listener.Port),
			TargetPort: intstr.FromInt(varnishHTTPPort), // Varnish listens on 8080
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
