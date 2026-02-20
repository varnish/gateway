package controller

import (
	"context"
	"log/slog"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
)

func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	must(clientgoscheme.AddToScheme(scheme))
	must(gatewayv1.Install(scheme))
	must(gatewayv1beta1.Install(scheme))
	must(gatewayparamsv1alpha1.AddToScheme(scheme))
	return scheme
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func newTestReconciler(scheme *runtime.Scheme, objs ...runtime.Object) *GatewayReconciler {
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&gatewayv1.Gateway{}).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		Build()

	return &GatewayReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Config: Config{
			GatewayClassName: "varnish",
			GatewayImage:     "ghcr.io/varnish/varnish-gateway:latest",
		},
		Logger: slog.Default(),
	}
}

// newTestTLSSecret creates a valid kubernetes.io/tls Secret for testing.
func newTestTLSSecret(name, namespace string) *corev1.Secret {
	// Minimal self-signed PEM cert and key for testing
	testCert := []byte(`-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABLU3
jRJN1NWgh1MJxnSK+tWjfRwSTaOGkI4bHmSreA6SE0IbKPl2WPfJjDzpNqkSsOCd
ShNzgBRMMA71IwaciUyjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2wpSek9nBDE0
HKRXRfbUE6v5gLP8HBgFGKMo0mIRn8oCIHyjk+aIKEjVJGSGFDt2MqXVpvGjj+xB
3HT5LiaoOKsm
-----END CERTIFICATE-----
`)
	testKey := []byte(`-----BEGIN EC PRIVATE KEY-----
MHQCAQEEIIrYSSNQFaA2Hwf583QmKbyavkgoftpCYFPJ1tx81lHLoAcGBSuBBAAi
oWQDYgAEjBFm5VUB+BIhqGeYYZBpWAn4fYIab1JIB+Vmz4HqPBquLsBanBp8X1AX
4PE/rSmh0VZ5a0N8es7PVxsxxBB4pyJEZ2FHjyVd5VACsXKfYGOxVwEqO6sXH4FG
k8H3ULWF
-----END EC PRIVATE KEY-----
`)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": testCert,
			"tls.key": testKey,
		},
	}
}

func TestBuildLabels(t *testing.T) {
	r := &GatewayReconciler{
		Config: Config{GatewayClassName: "varnish"},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
	}

	labels := r.buildLabels(gateway)

	if labels[LabelManagedBy] != ManagedByValue {
		t.Errorf("expected managed-by label %q, got %q", ManagedByValue, labels[LabelManagedBy])
	}
	if labels[LabelGatewayName] != "test-gateway" {
		t.Errorf("expected gateway-name label %q, got %q", "test-gateway", labels[LabelGatewayName])
	}
	if labels[LabelGatewayNamespace] != "default" {
		t.Errorf("expected gateway-namespace label %q, got %q", "default", labels[LabelGatewayNamespace])
	}
}

func TestBuildDeployment(t *testing.T) {
	r := &GatewayReconciler{
		Config: Config{
			GatewayClassName: "varnish",
			GatewayImage:     "ghcr.io/varnish/varnish-gateway:latest",
		},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	deployment := r.buildDeployment(gateway, nil, nil, "test-hash", false, nil, nil, nil)

	if deployment.Name != "test-gateway" {
		t.Errorf("expected deployment name %q, got %q", "test-gateway", deployment.Name)
	}
	if deployment.Namespace != "default" {
		t.Errorf("expected deployment namespace %q, got %q", "default", deployment.Namespace)
	}

	// Single combined container model
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Errorf("expected 1 container, got %d", len(deployment.Spec.Template.Spec.Containers))
	}

	// Verify container
	container := deployment.Spec.Template.Spec.Containers[0]
	if container.Name != "varnish-gateway" {
		t.Errorf("expected container name %q, got %q", "varnish-gateway", container.Name)
	}
	if container.Image != "ghcr.io/varnish/varnish-gateway:latest" {
		t.Errorf("expected image %q, got %q", "ghcr.io/varnish/varnish-gateway:latest", container.Image)
	}

	// Verify ports (HTTP and health)
	if len(container.Ports) != 2 {
		t.Errorf("expected 2 ports, got %d", len(container.Ports))
	}

	// Verify volumes
	if len(deployment.Spec.Template.Spec.Volumes) != 2 {
		t.Errorf("expected 2 volumes, got %d", len(deployment.Spec.Template.Spec.Volumes))
	}

	// Verify service account
	expectedSA := "test-gateway-chaperone"
	if deployment.Spec.Template.Spec.ServiceAccountName != expectedSA {
		t.Errorf("expected service account %q, got %q", expectedSA, deployment.Spec.Template.Spec.ServiceAccountName)
	}
}

func TestBuildDeployment_WithExtras(t *testing.T) {
	r := &GatewayReconciler{
		Config: Config{
			GatewayClassName: "varnish",
			GatewayImage:     "ghcr.io/varnish/varnish-gateway:latest",
		},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	extraVolumes := []corev1.Volume{
		{Name: "vmod-vol", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	extraVolumeMounts := []corev1.VolumeMount{
		{Name: "vmod-vol", MountPath: "/usr/lib/varnish/vmods"},
	}
	extraInitContainers := []corev1.Container{
		{Name: "vmod-loader", Image: "busybox:latest", Command: []string{"cp", "/src/libvmod.so", "/dst/"}},
	}

	deployment := r.buildDeployment(gateway, nil, nil, "test-hash", false, extraVolumes, extraVolumeMounts, extraInitContainers)

	// Verify extra volumes
	foundVol := false
	for _, v := range deployment.Spec.Template.Spec.Volumes {
		if v.Name == "vmod-vol" {
			foundVol = true
			break
		}
	}
	if !foundVol {
		t.Error("expected extra volume 'vmod-vol' in deployment volumes")
	}

	// Verify extra volume mounts on main container
	container := deployment.Spec.Template.Spec.Containers[0]
	foundMount := false
	for _, vm := range container.VolumeMounts {
		if vm.Name == "vmod-vol" && vm.MountPath == "/usr/lib/varnish/vmods" {
			foundMount = true
			break
		}
	}
	if !foundMount {
		t.Error("expected extra volume mount 'vmod-vol' on main container")
	}

	// Verify init containers
	if len(deployment.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(deployment.Spec.Template.Spec.InitContainers))
	}
	if deployment.Spec.Template.Spec.InitContainers[0].Name != "vmod-loader" {
		t.Errorf("expected init container name 'vmod-loader', got %q", deployment.Spec.Template.Spec.InitContainers[0].Name)
	}
}

func TestBuildService(t *testing.T) {
	r := &GatewayReconciler{
		Config: Config{GatewayClassName: "varnish"},
	}

	tests := []struct {
		name          string
		listeners     []gatewayv1.Listener
		expectedPorts int
		expectedPort  int32
	}{
		{
			name: "single HTTP listener",
			listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
			expectedPorts: 1,
			expectedPort:  80,
		},
		{
			name:          "no listeners defaults to port 80",
			listeners:     nil,
			expectedPorts: 1,
			expectedPort:  80,
		},
		{
			name: "multiple listeners",
			listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
				{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
			},
			expectedPorts: 2,
			expectedPort:  80, // First port
		},
		{
			name: "custom port",
			listeners: []gatewayv1.Listener{
				{Name: "http", Port: 8000, Protocol: gatewayv1.HTTPProtocolType},
			},
			expectedPorts: 1,
			expectedPort:  8000,
		},
		{
			name: "duplicate port listeners are deduplicated",
			listeners: []gatewayv1.Listener{
				{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
				{Name: "https-with-hostname", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
			},
			expectedPorts: 1,
			expectedPort:  443,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gateway := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "varnish",
					Listeners:        tc.listeners,
				},
			}

			svc := r.buildService(gateway)

			if len(svc.Spec.Ports) != tc.expectedPorts {
				t.Errorf("expected %d ports, got %d", tc.expectedPorts, len(svc.Spec.Ports))
			}
			if svc.Spec.Ports[0].Port != tc.expectedPort {
				t.Errorf("expected port %d, got %d", tc.expectedPort, svc.Spec.Ports[0].Port)
			}
			if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
				t.Errorf("expected service type LoadBalancer, got %s", svc.Spec.Type)
			}
		})
	}
}

func TestBuildVCLConfigMap(t *testing.T) {
	r := &GatewayReconciler{
		Config: Config{GatewayClassName: "varnish"},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
	}

	cm := r.buildVCLConfigMap(gateway, "vcl 4.1;\n\nimport ghost;\n")

	if cm.Name != "test-gateway-vcl" {
		t.Errorf("expected configmap name %q, got %q", "test-gateway-vcl", cm.Name)
	}

	if _, ok := cm.Data["main.vcl"]; !ok {
		t.Error("expected main.vcl in configmap data")
	}
	if _, ok := cm.Data["routing.json"]; !ok {
		t.Error("expected routing.json in configmap data")
	}

	// Verify VCL contains expected content
	vcl := cm.Data["main.vcl"]
	if vcl == "" {
		t.Error("expected non-empty VCL")
	}
}

func TestBuildAdminSecret(t *testing.T) {
	r := &GatewayReconciler{
		Config: Config{GatewayClassName: "varnish"},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
	}

	secret := r.buildAdminSecret(gateway)

	if secret.Name != "test-gateway-secret" {
		t.Errorf("expected secret name %q, got %q", "test-gateway-secret", secret.Name)
	}

	secretData, ok := secret.Data["secret"]
	if !ok {
		t.Error("expected secret data key 'secret'")
	}
	if len(secretData) != 64 { // 32 bytes hex encoded = 64 chars
		t.Errorf("expected secret length 64, got %d", len(secretData))
	}
}

func TestBuildServiceAccount(t *testing.T) {
	r := &GatewayReconciler{
		Config: Config{GatewayClassName: "varnish"},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
	}

	sa := r.buildServiceAccount(gateway)

	if sa.Name != "test-gateway-chaperone" {
		t.Errorf("expected service account name %q, got %q", "test-gateway-chaperone", sa.Name)
	}
	if sa.Namespace != "default" {
		t.Errorf("expected service account namespace %q, got %q", "default", sa.Namespace)
	}
}

func TestReconcile_SkipsDifferentGatewayClass(t *testing.T) {
	scheme := newTestScheme()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "other-class", // Not our class
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	r := newTestReconciler(scheme, gateway)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-gateway", Namespace: "default"},
	})

	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if result.Requeue {
		t.Error("expected no requeue for different gateway class")
	}

	// Verify no deployment was created
	var deployment appsv1.Deployment
	err = r.Get(context.Background(),
		types.NamespacedName{Name: "test-gateway", Namespace: "default"},
		&deployment)
	if err == nil {
		t.Error("expected no deployment to be created for different gateway class")
	}
}

// TestReconcile_CreatesResources was removed and replaced with TestReconcile_CreatesResources_Envtest
// in gateway_controller_envtest_test.go. The fake client doesn't support SSA properly, so we use
// envtest with a real API server instead. See ENVTEST-IMPLEMENTATION.md for details.

func TestValidateListenerTLSRefs_CrossNamespace_NoReferenceGrant(t *testing.T) {
	scheme := newTestScheme()
	r := newTestReconciler(scheme)

	tlsMode := gatewayv1.TLSModeTerminate
	certNS := gatewayv1.Namespace("cert-ns")

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-gw",
			Namespace:  "default",
			Generation: 1,
		},
	}

	listener := &gatewayv1.Listener{
		Name:     "https",
		Port:     443,
		Protocol: gatewayv1.HTTPSProtocolType,
		TLS: &gatewayv1.GatewayTLSConfig{
			Mode: &tlsMode,
			CertificateRefs: []gatewayv1.SecretObjectReference{
				{
					Name:      "my-cert",
					Namespace: &certNS,
				},
			},
		},
	}

	cond := r.validateListenerTLSRefs(context.Background(), gateway, listener)

	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected ConditionFalse, got %s", cond.Status)
	}
	if cond.Reason != string(gatewayv1.ListenerReasonRefNotPermitted) {
		t.Errorf("expected reason RefNotPermitted, got %s", cond.Reason)
	}
}

func TestValidateListenerTLSRefs_CrossNamespace_WithReferenceGrant(t *testing.T) {
	scheme := newTestScheme()

	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-gw-secrets",
			Namespace: "cert-ns",
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     "gateway.networking.k8s.io",
					Kind:      "Gateway",
					Namespace: "default",
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: "",
					Kind:  "Secret",
				},
			},
		},
	}

	secret := newTestTLSSecret("my-cert", "cert-ns")

	r := newTestReconciler(scheme, grant, secret)

	tlsMode := gatewayv1.TLSModeTerminate
	certNS := gatewayv1.Namespace("cert-ns")

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-gw",
			Namespace:  "default",
			Generation: 1,
		},
	}

	listener := &gatewayv1.Listener{
		Name:     "https",
		Port:     443,
		Protocol: gatewayv1.HTTPSProtocolType,
		TLS: &gatewayv1.GatewayTLSConfig{
			Mode: &tlsMode,
			CertificateRefs: []gatewayv1.SecretObjectReference{
				{
					Name:      "my-cert",
					Namespace: &certNS,
				},
			},
		},
	}

	cond := r.validateListenerTLSRefs(context.Background(), gateway, listener)

	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected ConditionTrue, got %s", cond.Status)
	}
	if cond.Reason != string(gatewayv1.ListenerReasonResolvedRefs) {
		t.Errorf("expected reason ResolvedRefs, got %s", cond.Reason)
	}
}

func TestValidateListenerTLSRefs_SameNamespace(t *testing.T) {
	scheme := newTestScheme()
	secret := newTestTLSSecret("my-cert", "default")
	r := newTestReconciler(scheme, secret)

	tlsMode := gatewayv1.TLSModeTerminate

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-gw",
			Namespace:  "default",
			Generation: 1,
		},
	}

	listener := &gatewayv1.Listener{
		Name:     "https",
		Port:     443,
		Protocol: gatewayv1.HTTPSProtocolType,
		TLS: &gatewayv1.GatewayTLSConfig{
			Mode: &tlsMode,
			CertificateRefs: []gatewayv1.SecretObjectReference{
				{
					Name: "my-cert",
				},
			},
		},
	}

	cond := r.validateListenerTLSRefs(context.Background(), gateway, listener)

	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected ConditionTrue for same-namespace ref, got %s", cond.Status)
	}
}

func TestGatewayReferencesSecret_CrossNamespace(t *testing.T) {
	r := &GatewayReconciler{
		Config: Config{GatewayClassName: "varnish"},
	}

	certNS := gatewayv1.Namespace("cert-ns")
	tlsMode := gatewayv1.TLSModeTerminate

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gw",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https",
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
					TLS: &gatewayv1.GatewayTLSConfig{
						Mode: &tlsMode,
						CertificateRefs: []gatewayv1.SecretObjectReference{
							{
								Name:      "my-cert",
								Namespace: &certNS,
							},
						},
					},
				},
			},
		},
	}

	// Should match cross-namespace ref
	if !r.gatewayReferencesSecret(gateway, "my-cert", "cert-ns") {
		t.Error("expected gateway to reference cross-namespace secret")
	}

	// Should not match wrong namespace
	if r.gatewayReferencesSecret(gateway, "my-cert", "other-ns") {
		t.Error("expected gateway NOT to reference secret in wrong namespace")
	}

	// Should not match wrong name
	if r.gatewayReferencesSecret(gateway, "other-cert", "cert-ns") {
		t.Error("expected gateway NOT to reference secret with wrong name")
	}
}

func TestGatewayHasCrossNSCertRefTo(t *testing.T) {
	certNS := gatewayv1.Namespace("cert-ns")
	tlsMode := gatewayv1.TLSModeTerminate

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name:     "https",
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
					TLS: &gatewayv1.GatewayTLSConfig{
						Mode: &tlsMode,
						CertificateRefs: []gatewayv1.SecretObjectReference{
							{Name: "cert", Namespace: &certNS},
						},
					},
				},
			},
		},
	}

	if !gatewayHasCrossNSCertRefTo(gateway, "cert-ns") {
		t.Error("expected true for matching target namespace")
	}
	if gatewayHasCrossNSCertRefTo(gateway, "other-ns") {
		t.Error("expected false for non-matching target namespace")
	}
	if gatewayHasCrossNSCertRefTo(gateway, "default") {
		t.Error("expected false for same namespace as gateway (not cross-namespace)")
	}
}

func TestReconcile_NotFoundReturnsNoError(t *testing.T) {
	scheme := newTestScheme()
	r := newTestReconciler(scheme) // No gateway exists

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
	})

	if err != nil {
		t.Errorf("expected no error for not found gateway, got: %v", err)
	}
	if result.Requeue {
		t.Error("expected no requeue for not found gateway")
	}
}
