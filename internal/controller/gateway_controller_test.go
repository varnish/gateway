package controller

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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

	deployment := r.buildDeployment(gateway, nil, nil, "test-hash", false, nil, nil, nil, nil)

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

	deployment := r.buildDeployment(gateway, nil, nil, "test-hash", false, extraVolumes, extraVolumeMounts, extraInitContainers, nil)

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

// ============================================================
// Phase 1: Resource Builder Tests
// ============================================================

func TestBuildTLSSecret(t *testing.T) {
	r := &GatewayReconciler{
		Config: Config{GatewayClassName: "varnish"},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
	}

	tests := []struct {
		name     string
		certData map[string][]byte
	}{
		{
			name:     "empty certData",
			certData: map[string][]byte{},
		},
		{
			name:     "single cert",
			certData: map[string][]byte{"my-cert.pem": []byte("cert-data")},
		},
		{
			name: "multiple certs",
			certData: map[string][]byte{
				"cert-a.pem": []byte("cert-a-data"),
				"cert-b.pem": []byte("cert-b-data"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			secret := r.buildTLSSecret(gateway, tc.certData)

			if secret.Name != "my-gw-tls" {
				t.Errorf("expected name %q, got %q", "my-gw-tls", secret.Name)
			}
			if secret.Namespace != "default" {
				t.Errorf("expected namespace %q, got %q", "default", secret.Namespace)
			}
			if secret.Type != corev1.SecretTypeOpaque {
				t.Errorf("expected type Opaque, got %s", secret.Type)
			}
			if secret.Labels[LabelManagedBy] != ManagedByValue {
				t.Errorf("expected managed-by label %q, got %q", ManagedByValue, secret.Labels[LabelManagedBy])
			}
			if len(secret.Data) != len(tc.certData) {
				t.Errorf("expected %d data entries, got %d", len(tc.certData), len(secret.Data))
			}
			for k, v := range tc.certData {
				if string(secret.Data[k]) != string(v) {
					t.Errorf("expected data[%s] = %q, got %q", k, v, secret.Data[k])
				}
			}
		})
	}
}

func TestBuildClusterRoleBinding(t *testing.T) {
	r := &GatewayReconciler{
		Config: Config{GatewayClassName: "varnish"},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "prod"},
	}

	crb := r.buildClusterRoleBinding(gateway)

	if crb.Name != "prod-my-gw-chaperone" {
		t.Errorf("expected name %q, got %q", "prod-my-gw-chaperone", crb.Name)
	}
	// Cluster-scoped, no namespace
	if crb.Namespace != "" {
		t.Errorf("expected empty namespace for cluster-scoped resource, got %q", crb.Namespace)
	}
	if crb.RoleRef.Name != "varnish-gateway-chaperone" {
		t.Errorf("expected roleRef name %q, got %q", "varnish-gateway-chaperone", crb.RoleRef.Name)
	}
	if crb.RoleRef.Kind != "ClusterRole" {
		t.Errorf("expected roleRef kind ClusterRole, got %q", crb.RoleRef.Kind)
	}
	if len(crb.Subjects) != 1 {
		t.Fatalf("expected 1 subject, got %d", len(crb.Subjects))
	}
	subj := crb.Subjects[0]
	if subj.Kind != "ServiceAccount" {
		t.Errorf("expected subject kind ServiceAccount, got %q", subj.Kind)
	}
	if subj.Name != "my-gw-chaperone" {
		t.Errorf("expected subject name %q, got %q", "my-gw-chaperone", subj.Name)
	}
	if subj.Namespace != "prod" {
		t.Errorf("expected subject namespace %q, got %q", "prod", subj.Namespace)
	}
}

func TestBuildLoggingSidecar(t *testing.T) {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
	}

	tests := []struct {
		name            string
		logging         *gatewayparamsv1alpha1.VarnishLogging
		expectCommand   []string
		expectImage     string
		expectFormatArg bool
		expectExtraArgs []string
	}{
		{
			name:          "varnishlog mode",
			logging:       &gatewayparamsv1alpha1.VarnishLogging{Mode: "varnishlog"},
			expectCommand: []string{"varnishlog"},
			expectImage:   "ghcr.io/varnish/varnish-gateway:latest",
		},
		{
			name: "varnishncsa with format",
			logging: &gatewayparamsv1alpha1.VarnishLogging{
				Mode:   "varnishncsa",
				Format: "%h %l %u %t",
			},
			expectCommand:   []string{"varnishncsa"},
			expectImage:     "ghcr.io/varnish/varnish-gateway:latest",
			expectFormatArg: true,
		},
		{
			name: "varnishncsa with extra args",
			logging: &gatewayparamsv1alpha1.VarnishLogging{
				Mode:      "varnishncsa",
				ExtraArgs: []string{"-g", "request"},
			},
			expectCommand:   []string{"varnishncsa"},
			expectExtraArgs: []string{"-g", "request"},
		},
		{
			name: "custom image override",
			logging: &gatewayparamsv1alpha1.VarnishLogging{
				Mode:  "varnishlog",
				Image: "custom-image:v1",
			},
			expectCommand: []string{"varnishlog"},
			expectImage:   "custom-image:v1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &GatewayReconciler{
				Config: Config{
					GatewayClassName: "varnish",
					GatewayImage:     "ghcr.io/varnish/varnish-gateway:latest",
				},
			}

			container := r.buildLoggingSidecar(gateway, tc.logging)

			if container.Name != "varnish-log" {
				t.Errorf("expected name varnish-log, got %q", container.Name)
			}
			if len(container.Command) != len(tc.expectCommand) || container.Command[0] != tc.expectCommand[0] {
				t.Errorf("expected command %v, got %v", tc.expectCommand, container.Command)
			}
			if tc.expectImage != "" && container.Image != tc.expectImage {
				t.Errorf("expected image %q, got %q", tc.expectImage, container.Image)
			}

			// Check -F flag for format
			argsStr := strings.Join(container.Args, " ")
			if tc.expectFormatArg {
				if !strings.Contains(argsStr, "-F") {
					t.Error("expected -F flag in args for varnishncsa with format")
				}
			}

			// Check extra args are present
			for _, ea := range tc.expectExtraArgs {
				found := false
				for _, a := range container.Args {
					if a == ea {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected extra arg %q in args %v", ea, container.Args)
				}
			}

			// Check standard args
			if !strings.Contains(argsStr, "-n /var/run/varnish/vsm") {
				t.Error("expected -n /var/run/varnish/vsm in args")
			}
			if !strings.Contains(argsStr, "-t off") {
				t.Error("expected -t off in args")
			}

			// Check volume mount
			if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].Name != volumeVarnishRun {
				t.Errorf("expected single volume mount %q, got %v", volumeVarnishRun, container.VolumeMounts)
			}
			if !container.VolumeMounts[0].ReadOnly {
				t.Error("expected read-only volume mount")
			}

			// Check resources
			if container.Resources.Requests.Cpu().IsZero() {
				t.Error("expected non-zero CPU request")
			}
			if container.Resources.Limits.Memory().IsZero() {
				t.Error("expected non-zero memory limit")
			}
		})
	}
}

func TestBuildGatewayContainer(t *testing.T) {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
	}

	tests := []struct {
		name              string
		varnishdExtraArgs []string
		hasTLS            bool
		extraVolumeMounts []corev1.VolumeMount
		expectPorts       int
		expectVolMounts   int
		expectTLSEnv      bool
		expectExtraArgsEnv string
	}{
		{
			name:            "no TLS",
			hasTLS:          false,
			expectPorts:     2, // http + health
			expectVolMounts: 2, // vcl-config + varnish-run
		},
		{
			name:            "with TLS",
			hasTLS:          true,
			expectPorts:     3, // http + health + https
			expectVolMounts: 3, // vcl-config + varnish-run + tls-certs
			expectTLSEnv:    true,
		},
		{
			name:               "with varnishdExtraArgs",
			varnishdExtraArgs:  []string{"-p", "thread_pools=4"},
			expectPorts:        2,
			expectVolMounts:    2,
			expectExtraArgsEnv: "-p;thread_pools=4",
		},
		{
			name:   "with extra volume mounts",
			hasTLS: false,
			extraVolumeMounts: []corev1.VolumeMount{
				{Name: "custom", MountPath: "/custom"},
			},
			expectPorts:     2,
			expectVolMounts: 3, // 2 standard + 1 extra
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &GatewayReconciler{
				Config: Config{
					GatewayClassName: "varnish",
					GatewayImage:     "ghcr.io/varnish/varnish-gateway:latest",
				},
			}

			container := r.buildGatewayContainer(gateway, tc.varnishdExtraArgs, tc.hasTLS, tc.extraVolumeMounts, nil)

			if len(container.Ports) != tc.expectPorts {
				t.Errorf("expected %d ports, got %d", tc.expectPorts, len(container.Ports))
			}
			if len(container.VolumeMounts) != tc.expectVolMounts {
				t.Errorf("expected %d volume mounts, got %d", tc.expectVolMounts, len(container.VolumeMounts))
			}

			// Check TLS env vars
			hasTLSListen := false
			hasTLSCertDir := false
			hasExtraArgs := ""
			for _, env := range container.Env {
				if env.Name == "VARNISH_TLS_LISTEN" {
					hasTLSListen = true
				}
				if env.Name == "TLS_CERT_DIR" {
					hasTLSCertDir = true
				}
				if env.Name == "VARNISHD_EXTRA_ARGS" {
					hasExtraArgs = env.Value
				}
			}
			if tc.expectTLSEnv && (!hasTLSListen || !hasTLSCertDir) {
				t.Error("expected TLS env vars when hasTLS is true")
			}
			if !tc.expectTLSEnv && (hasTLSListen || hasTLSCertDir) {
				t.Error("did not expect TLS env vars when hasTLS is false")
			}
			if tc.expectExtraArgsEnv != "" && hasExtraArgs != tc.expectExtraArgsEnv {
				t.Errorf("expected VARNISHD_EXTRA_ARGS=%q, got %q", tc.expectExtraArgsEnv, hasExtraArgs)
			}

			// Check IPC_LOCK capability
			if container.SecurityContext == nil || container.SecurityContext.Capabilities == nil {
				t.Fatal("expected security context with capabilities")
			}
			foundIPCLock := false
			for _, cap := range container.SecurityContext.Capabilities.Add {
				if cap == "IPC_LOCK" {
					foundIPCLock = true
				}
			}
			if !foundIPCLock {
				t.Error("expected IPC_LOCK capability")
			}

			// Check probes
			if container.ReadinessProbe == nil {
				t.Error("expected readiness probe")
			}
			if container.LivenessProbe == nil {
				t.Error("expected liveness probe")
			}

			// Check lifecycle
			if container.Lifecycle == nil || container.Lifecycle.PreStop == nil {
				t.Error("expected preStop lifecycle hook")
			}
		})
	}
}

func TestBuildVolumes(t *testing.T) {
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
	}

	tests := []struct {
		name   string
		hasTLS bool
		extra  []corev1.Volume
		expect int
	}{
		{
			name:   "without TLS",
			hasTLS: false,
			expect: 2, // vcl-config + varnish-run
		},
		{
			name:   "with TLS",
			hasTLS: true,
			expect: 3, // vcl-config + varnish-run + tls-certs
		},
		{
			name:   "with extra volumes",
			hasTLS: false,
			extra: []corev1.Volume{
				{Name: "extra-vol", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			expect: 3, // 2 standard + 1 extra
		},
		{
			name:   "with TLS and extra volumes",
			hasTLS: true,
			extra: []corev1.Volume{
				{Name: "extra-vol", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			expect: 4, // 3 TLS + 1 extra
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &GatewayReconciler{Config: Config{GatewayClassName: "varnish"}}
			volumes := r.buildVolumes(gateway, tc.hasTLS, tc.extra)
			if len(volumes) != tc.expect {
				t.Errorf("expected %d volumes, got %d", tc.expect, len(volumes))
			}

			// First two should always be vcl-config and varnish-run
			if volumes[0].Name != volumeVCLConfig {
				t.Errorf("expected first volume %q, got %q", volumeVCLConfig, volumes[0].Name)
			}
			if volumes[1].Name != volumeVarnishRun {
				t.Errorf("expected second volume %q, got %q", volumeVarnishRun, volumes[1].Name)
			}

			if tc.hasTLS {
				if volumes[2].Name != volumeTLSCerts {
					t.Errorf("expected third volume %q for TLS, got %q", volumeTLSCerts, volumes[2].Name)
				}
			}
		})
	}
}

func TestBuildContainers(t *testing.T) {
	r := &GatewayReconciler{
		Config: Config{
			GatewayClassName: "varnish",
			GatewayImage:     "ghcr.io/varnish/varnish-gateway:latest",
		},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
	}

	t.Run("without logging", func(t *testing.T) {
		containers := r.buildContainers(gateway, nil, nil, false, nil, nil)
		if len(containers) != 1 {
			t.Errorf("expected 1 container, got %d", len(containers))
		}
		if containers[0].Name != "varnish-gateway" {
			t.Errorf("expected container name varnish-gateway, got %q", containers[0].Name)
		}
	})

	t.Run("with logging", func(t *testing.T) {
		logging := &gatewayparamsv1alpha1.VarnishLogging{Mode: "varnishlog"}
		containers := r.buildContainers(gateway, nil, logging, false, nil, nil)
		if len(containers) != 2 {
			t.Errorf("expected 2 containers, got %d", len(containers))
		}
		if containers[1].Name != "varnish-log" {
			t.Errorf("expected second container name varnish-log, got %q", containers[1].Name)
		}
	})

	t.Run("logging with empty mode is not added", func(t *testing.T) {
		logging := &gatewayparamsv1alpha1.VarnishLogging{Mode: ""}
		containers := r.buildContainers(gateway, nil, logging, false, nil, nil)
		if len(containers) != 1 {
			t.Errorf("expected 1 container when logging mode is empty, got %d", len(containers))
		}
	})
}

// ============================================================
// Phase 2: Pure Logic Function Tests
// ============================================================

func TestNeedsDeploymentUpdate(t *testing.T) {
	tests := []struct {
		name           string
		existing       *appsv1.Deployment
		desired        *appsv1.Deployment
		expectUpdate   bool
	}{
		{
			name: "same image same hash",
			existing: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationInfraHash: "abc"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Image: "img:v1"}}},
				}},
			},
			desired: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationInfraHash: "abc"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Image: "img:v1"}}},
				}},
			},
			expectUpdate: false,
		},
		{
			name: "different image",
			existing: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationInfraHash: "abc"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Image: "img:v1"}}},
				}},
			},
			desired: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationInfraHash: "abc"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Image: "img:v2"}}},
				}},
			},
			expectUpdate: true,
		},
		{
			name: "different hash",
			existing: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationInfraHash: "abc"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Image: "img:v1"}}},
				}},
			},
			desired: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationInfraHash: "def"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Image: "img:v1"}}},
				}},
			},
			expectUpdate: true,
		},
		{
			name: "empty containers",
			existing: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{}},
				}},
			},
			desired: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{}},
				}},
			},
			expectUpdate: false,
		},
		{
			name: "nil annotations handled gracefully",
			existing: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "img:v1"}}},
				}},
			},
			desired: &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationInfraHash: "abc"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Image: "img:v1"}}},
				}},
			},
			expectUpdate: true, // empty hash != "abc"
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := needsDeploymentUpdate(tc.existing, tc.desired)
			if result != tc.expectUpdate {
				t.Errorf("expected %v, got %v", tc.expectUpdate, result)
			}
		})
	}
}

func TestNeedsServiceUpdate(t *testing.T) {
	tests := []struct {
		name         string
		existing     *corev1.Service
		desired      *corev1.Service
		expectUpdate bool
	}{
		{
			name: "same ports",
			existing: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP},
					},
				},
			},
			desired: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP},
					},
				},
			},
			expectUpdate: false,
		},
		{
			name: "HTTPS port added",
			existing: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP},
					},
				},
			},
			desired: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP},
						{Name: "https", Port: 443, TargetPort: intstr.FromInt(8443), Protocol: corev1.ProtocolTCP},
					},
				},
			},
			expectUpdate: true,
		},
		{
			name: "port number changed",
			existing: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP},
					},
				},
			},
			desired: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Port: 8080, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP},
					},
				},
			},
			expectUpdate: true,
		},
		{
			name: "port removed",
			existing: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP},
						{Name: "https", Port: 443, TargetPort: intstr.FromInt(8443), Protocol: corev1.ProtocolTCP},
					},
				},
			},
			desired: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP},
					},
				},
			},
			expectUpdate: true,
		},
		{
			name: "target port changed",
			existing: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP},
					},
				},
			},
			desired: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Port: 80, TargetPort: intstr.FromInt(9090), Protocol: corev1.ProtocolTCP},
					},
				},
			},
			expectUpdate: true,
		},
		{
			name: "ignores API server added fields like NodePort",
			existing: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP, NodePort: 31234},
					},
				},
			},
			desired: &corev1.Service{
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{Name: "http", Port: 80, TargetPort: intstr.FromInt(8080), Protocol: corev1.ProtocolTCP},
					},
				},
			},
			expectUpdate: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := needsServiceUpdate(tc.existing, tc.desired)
			if result != tc.expectUpdate {
				t.Errorf("expected %v, got %v", tc.expectUpdate, result)
			}
		})
	}
}

func TestNeedsSecretUpdate(t *testing.T) {
	tests := []struct {
		name         string
		existing     *corev1.Secret
		desired      *corev1.Secret
		expectUpdate bool
	}{
		{
			name: "same data",
			existing: &corev1.Secret{
				Data: map[string][]byte{"cert.pem": []byte("certdata")},
			},
			desired: &corev1.Secret{
				Data: map[string][]byte{"cert.pem": []byte("certdata")},
			},
			expectUpdate: false,
		},
		{
			name: "cert added",
			existing: &corev1.Secret{
				Data: map[string][]byte{"cert.pem": []byte("certdata")},
			},
			desired: &corev1.Secret{
				Data: map[string][]byte{
					"cert.pem":  []byte("certdata"),
					"cert2.pem": []byte("certdata2"),
				},
			},
			expectUpdate: true,
		},
		{
			name: "cert removed",
			existing: &corev1.Secret{
				Data: map[string][]byte{
					"cert.pem":  []byte("certdata"),
					"cert2.pem": []byte("certdata2"),
				},
			},
			desired: &corev1.Secret{
				Data: map[string][]byte{"cert.pem": []byte("certdata")},
			},
			expectUpdate: true,
		},
		{
			name: "cert data changed",
			existing: &corev1.Secret{
				Data: map[string][]byte{"cert.pem": []byte("olddata")},
			},
			desired: &corev1.Secret{
				Data: map[string][]byte{"cert.pem": []byte("newdata")},
			},
			expectUpdate: true,
		},
		{
			name: "both empty",
			existing: &corev1.Secret{
				Data: map[string][]byte{},
			},
			desired: &corev1.Secret{
				Data: map[string][]byte{},
			},
			expectUpdate: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := needsSecretUpdate(tc.existing, tc.desired)
			if result != tc.expectUpdate {
				t.Errorf("expected %v, got %v", tc.expectUpdate, result)
			}
		})
	}
}

func TestValidateListenerRouteKinds(t *testing.T) {
	tests := []struct {
		name             string
		listener         *gatewayv1.Listener
		expectKindsCount int
		expectInvalid    bool
	}{
		{
			name:             "no AllowedRoutes defaults to HTTPRoute",
			listener:         &gatewayv1.Listener{},
			expectKindsCount: 1,
			expectInvalid:    false,
		},
		{
			name: "empty Kinds defaults to HTTPRoute",
			listener: &gatewayv1.Listener{
				AllowedRoutes: &gatewayv1.AllowedRoutes{Kinds: []gatewayv1.RouteGroupKind{}},
			},
			expectKindsCount: 1,
			expectInvalid:    false,
		},
		{
			name: "HTTPRoute only",
			listener: &gatewayv1.Listener{
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Kinds: []gatewayv1.RouteGroupKind{
						{Group: ptr(gatewayv1.Group("gateway.networking.k8s.io")), Kind: "HTTPRoute"},
					},
				},
			},
			expectKindsCount: 1,
			expectInvalid:    false,
		},
		{
			name: "unsupported kind only",
			listener: &gatewayv1.Listener{
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Kinds: []gatewayv1.RouteGroupKind{
						{Group: ptr(gatewayv1.Group("gateway.networking.k8s.io")), Kind: "TLSRoute"},
					},
				},
			},
			expectKindsCount: 0,
			expectInvalid:    true,
		},
		{
			name: "mix of valid and invalid",
			listener: &gatewayv1.Listener{
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Kinds: []gatewayv1.RouteGroupKind{
						{Group: ptr(gatewayv1.Group("gateway.networking.k8s.io")), Kind: "HTTPRoute"},
						{Group: ptr(gatewayv1.Group("gateway.networking.k8s.io")), Kind: "GRPCRoute"},
					},
				},
			},
			expectKindsCount: 1,
			expectInvalid:    true,
		},
		{
			name: "HTTPRoute with nil group defaults correctly",
			listener: &gatewayv1.Listener{
				AllowedRoutes: &gatewayv1.AllowedRoutes{
					Kinds: []gatewayv1.RouteGroupKind{
						{Kind: "HTTPRoute"}, // nil Group
					},
				},
			},
			expectKindsCount: 1,
			expectInvalid:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kinds, hasInvalid := validateListenerRouteKinds(tc.listener)
			if len(kinds) != tc.expectKindsCount {
				t.Errorf("expected %d kinds, got %d", tc.expectKindsCount, len(kinds))
			}
			if hasInvalid != tc.expectInvalid {
				t.Errorf("expected hasInvalid=%v, got %v", tc.expectInvalid, hasInvalid)
			}
		})
	}
}

func TestParseImagePullSecrets(t *testing.T) {
	tests := []struct {
		name   string
		config string
		expect []string
	}{
		{name: "empty string", config: "", expect: nil},
		{name: "single secret", config: "my-secret", expect: []string{"my-secret"}},
		{name: "multiple secrets", config: "a,b,c", expect: []string{"a", "b", "c"}},
		{name: "whitespace handling", config: " a , b , c ", expect: []string{"a", "b", "c"}},
		{name: "trailing comma", config: "a,b,", expect: []string{"a", "b"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &GatewayReconciler{Config: Config{ImagePullSecrets: tc.config}}
			result := r.parseImagePullSecrets()
			if len(result) != len(tc.expect) {
				t.Fatalf("expected %d secrets, got %d: %v", len(tc.expect), len(result), result)
			}
			for i, s := range tc.expect {
				if result[i] != s {
					t.Errorf("expected secret[%d] = %q, got %q", i, s, result[i])
				}
			}
		})
	}
}

func TestGenerateVCL(t *testing.T) {
	t.Run("no user VCL", func(t *testing.T) {
		scheme := newTestScheme()
		r := newTestReconciler(scheme)

		gateway := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
		}

		vcl := r.generateVCL(context.Background(), gateway)
		if !strings.Contains(vcl, "import ghost") {
			t.Error("expected generated VCL to contain 'import ghost'")
		}
	})

	t.Run("with user VCL", func(t *testing.T) {
		scheme := newTestScheme()
		userVCLCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "my-vcl", Namespace: "default"},
			Data:       map[string]string{"user.vcl": "sub vcl_recv { set req.http.X-Custom = \"hello\"; }"},
		}
		params := &gatewayparamsv1alpha1.GatewayClassParameters{
			ObjectMeta: metav1.ObjectMeta{Name: "varnish-params"},
			Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
				UserVCLConfigMapRef: &gatewayparamsv1alpha1.ConfigMapReference{
					Name:      "my-vcl",
					Namespace: "default",
				},
			},
		}
		gc := &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: ControllerName,
				ParametersRef: &gatewayv1.ParametersReference{
					Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
					Kind:  "GatewayClassParameters",
					Name:  "varnish-params",
				},
			},
		}
		r := newTestReconciler(scheme, gc, params, userVCLCM)

		gateway := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
		}

		vcl := r.generateVCL(context.Background(), gateway)
		if !strings.Contains(vcl, "import ghost") {
			t.Error("expected generated VCL to contain 'import ghost'")
		}
		if !strings.Contains(vcl, "X-Custom") {
			t.Error("expected merged VCL to contain user VCL content")
		}
	})
}

// ============================================================
// Phase 3: Client-Dependent Function Tests
// ============================================================

func TestGetGatewayClassParameters(t *testing.T) {
	scheme := newTestScheme()

	tests := []struct {
		name      string
		objs      []runtime.Object
		gateway   *gatewayv1.Gateway
		expectNil bool
	}{
		{
			name: "GatewayClass not found",
			objs: nil,
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
			},
			expectNil: true,
		},
		{
			name: "GatewayClass has no ParametersRef",
			objs: []runtime.Object{
				&gatewayv1.GatewayClass{
					ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
					Spec: gatewayv1.GatewayClassSpec{
						ControllerName: ControllerName,
					},
				},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
			},
			expectNil: true,
		},
		{
			name: "ParametersRef wrong group",
			objs: []runtime.Object{
				&gatewayv1.GatewayClass{
					ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
					Spec: gatewayv1.GatewayClassSpec{
						ControllerName: ControllerName,
						ParametersRef: &gatewayv1.ParametersReference{
							Group: "wrong.group",
							Kind:  "GatewayClassParameters",
							Name:  "params",
						},
					},
				},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
			},
			expectNil: true,
		},
		{
			name: "ParametersRef wrong kind",
			objs: []runtime.Object{
				&gatewayv1.GatewayClass{
					ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
					Spec: gatewayv1.GatewayClassSpec{
						ControllerName: ControllerName,
						ParametersRef: &gatewayv1.ParametersReference{
							Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
							Kind:  "WrongKind",
							Name:  "params",
						},
					},
				},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
			},
			expectNil: true,
		},
		{
			name: "happy path",
			objs: []runtime.Object{
				&gatewayv1.GatewayClass{
					ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
					Spec: gatewayv1.GatewayClassSpec{
						ControllerName: ControllerName,
						ParametersRef: &gatewayv1.ParametersReference{
							Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
							Kind:  "GatewayClassParameters",
							Name:  "my-params",
						},
					},
				},
				&gatewayparamsv1alpha1.GatewayClassParameters{
					ObjectMeta: metav1.ObjectMeta{Name: "my-params"},
					Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
						VarnishdExtraArgs: []string{"-p", "thread_pools=4"},
					},
				},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
			},
			expectNil: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestReconciler(scheme, tc.objs...)
			result := r.getGatewayClassParameters(context.Background(), tc.gateway)
			if tc.expectNil && result != nil {
				t.Error("expected nil result")
			}
			if !tc.expectNil && result == nil {
				t.Error("expected non-nil result")
			}
			if !tc.expectNil && result != nil {
				if len(result.Spec.VarnishdExtraArgs) != 2 {
					t.Errorf("expected 2 extra args, got %d", len(result.Spec.VarnishdExtraArgs))
				}
			}
		})
	}
}

func TestGetUserVCL(t *testing.T) {
	scheme := newTestScheme()

	tests := []struct {
		name      string
		objs      []runtime.Object
		gateway   *gatewayv1.Gateway
		expectVCL string
	}{
		{
			name: "no GatewayClass",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
			},
			expectVCL: "",
		},
		{
			name: "no ParametersRef",
			objs: []runtime.Object{
				&gatewayv1.GatewayClass{
					ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
					Spec:       gatewayv1.GatewayClassSpec{ControllerName: ControllerName},
				},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
			},
			expectVCL: "",
		},
		{
			name: "no UserVCLConfigMapRef",
			objs: []runtime.Object{
				&gatewayv1.GatewayClass{
					ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
					Spec: gatewayv1.GatewayClassSpec{
						ControllerName: ControllerName,
						ParametersRef: &gatewayv1.ParametersReference{
							Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
							Kind:  "GatewayClassParameters",
							Name:  "params",
						},
					},
				},
				&gatewayparamsv1alpha1.GatewayClassParameters{
					ObjectMeta: metav1.ObjectMeta{Name: "params"},
				},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
			},
			expectVCL: "",
		},
		{
			name: "ConfigMap exists with default key",
			objs: []runtime.Object{
				&gatewayv1.GatewayClass{
					ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
					Spec: gatewayv1.GatewayClassSpec{
						ControllerName: ControllerName,
						ParametersRef: &gatewayv1.ParametersReference{
							Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
							Kind:  "GatewayClassParameters",
							Name:  "params",
						},
					},
				},
				&gatewayparamsv1alpha1.GatewayClassParameters{
					ObjectMeta: metav1.ObjectMeta{Name: "params"},
					Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
						UserVCLConfigMapRef: &gatewayparamsv1alpha1.ConfigMapReference{
							Name:      "user-vcl",
							Namespace: "default",
						},
					},
				},
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "user-vcl", Namespace: "default"},
					Data:       map[string]string{"user.vcl": "sub vcl_recv {}"},
				},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
			},
			expectVCL: "sub vcl_recv {}",
		},
		{
			name: "ConfigMap with custom key",
			objs: []runtime.Object{
				&gatewayv1.GatewayClass{
					ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
					Spec: gatewayv1.GatewayClassSpec{
						ControllerName: ControllerName,
						ParametersRef: &gatewayv1.ParametersReference{
							Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
							Kind:  "GatewayClassParameters",
							Name:  "params",
						},
					},
				},
				&gatewayparamsv1alpha1.GatewayClassParameters{
					ObjectMeta: metav1.ObjectMeta{Name: "params"},
					Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
						UserVCLConfigMapRef: &gatewayparamsv1alpha1.ConfigMapReference{
							Name:      "user-vcl",
							Namespace: "default",
							Key:       "custom.vcl",
						},
					},
				},
				&corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: "user-vcl", Namespace: "default"},
					Data:       map[string]string{"custom.vcl": "sub vcl_deliver {}"},
				},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
			},
			expectVCL: "sub vcl_deliver {}",
		},
		{
			name: "ConfigMap missing",
			objs: []runtime.Object{
				&gatewayv1.GatewayClass{
					ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
					Spec: gatewayv1.GatewayClassSpec{
						ControllerName: ControllerName,
						ParametersRef: &gatewayv1.ParametersReference{
							Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
							Kind:  "GatewayClassParameters",
							Name:  "params",
						},
					},
				},
				&gatewayparamsv1alpha1.GatewayClassParameters{
					ObjectMeta: metav1.ObjectMeta{Name: "params"},
					Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
						UserVCLConfigMapRef: &gatewayparamsv1alpha1.ConfigMapReference{
							Name:      "missing-cm",
							Namespace: "default",
						},
					},
				},
			},
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
			},
			expectVCL: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestReconciler(scheme, tc.objs...)
			result := r.getUserVCL(context.Background(), tc.gateway)
			if result != tc.expectVCL {
				t.Errorf("expected %q, got %q", tc.expectVCL, result)
			}
		})
	}
}

func TestCollectTLSCertData(t *testing.T) {
	scheme := newTestScheme()
	tlsMode := gatewayv1.TLSModeTerminate

	t.Run("no HTTPS listeners", func(t *testing.T) {
		r := newTestReconciler(scheme)
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
				},
			},
		}
		result := r.collectTLSCertData(context.Background(), gw)
		if len(result) != 0 {
			t.Errorf("expected empty map, got %d entries", len(result))
		}
	})

	t.Run("HTTPS listener with valid secret", func(t *testing.T) {
		secret := newTestTLSSecret("my-cert", "default")
		r := newTestReconciler(scheme, secret)
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{
						Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
						TLS: &gatewayv1.GatewayTLSConfig{
							Mode:            &tlsMode,
							CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "my-cert"}},
						},
					},
				},
			},
		}
		result := r.collectTLSCertData(context.Background(), gw)
		if len(result) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(result))
		}
		if _, ok := result["my-cert.pem"]; !ok {
			t.Error("expected key 'my-cert.pem'")
		}
	})

	t.Run("cross-namespace with ReferenceGrant", func(t *testing.T) {
		secret := newTestTLSSecret("cross-cert", "cert-ns")
		grant := &gatewayv1beta1.ReferenceGrant{
			ObjectMeta: metav1.ObjectMeta{Name: "allow", Namespace: "cert-ns"},
			Spec: gatewayv1beta1.ReferenceGrantSpec{
				From: []gatewayv1beta1.ReferenceGrantFrom{
					{Group: "gateway.networking.k8s.io", Kind: "Gateway", Namespace: "default"},
				},
				To: []gatewayv1beta1.ReferenceGrantTo{
					{Group: "", Kind: "Secret"},
				},
			},
		}
		r := newTestReconciler(scheme, secret, grant)
		certNS := gatewayv1.Namespace("cert-ns")
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{
						Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
						TLS: &gatewayv1.GatewayTLSConfig{
							Mode:            &tlsMode,
							CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "cross-cert", Namespace: &certNS}},
						},
					},
				},
			},
		}
		result := r.collectTLSCertData(context.Background(), gw)
		if len(result) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(result))
		}
		if _, ok := result["cert-ns-cross-cert.pem"]; !ok {
			t.Errorf("expected key 'cert-ns-cross-cert.pem', got keys: %v", keys(result))
		}
	})

	t.Run("cross-namespace without ReferenceGrant skipped", func(t *testing.T) {
		secret := newTestTLSSecret("cross-cert", "cert-ns")
		r := newTestReconciler(scheme, secret) // No grant
		certNS := gatewayv1.Namespace("cert-ns")
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{
						Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
						TLS: &gatewayv1.GatewayTLSConfig{
							Mode:            &tlsMode,
							CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "cross-cert", Namespace: &certNS}},
						},
					},
				},
			},
		}
		result := r.collectTLSCertData(context.Background(), gw)
		if len(result) != 0 {
			t.Errorf("expected 0 entries without ReferenceGrant, got %d", len(result))
		}
	})

	t.Run("missing secret skipped", func(t *testing.T) {
		r := newTestReconciler(scheme) // No secret
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{
						Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
						TLS: &gatewayv1.GatewayTLSConfig{
							Mode:            &tlsMode,
							CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "nonexistent"}},
						},
					},
				},
			},
		}
		result := r.collectTLSCertData(context.Background(), gw)
		if len(result) != 0 {
			t.Errorf("expected 0 entries for missing secret, got %d", len(result))
		}
	})

	t.Run("wrong secret type skipped", func(t *testing.T) {
		opaqueSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "opaque-secret", Namespace: "default"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"tls.crt": []byte("cert"), "tls.key": []byte("key")},
		}
		r := newTestReconciler(scheme, opaqueSecret)
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{
						Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
						TLS: &gatewayv1.GatewayTLSConfig{
							Mode:            &tlsMode,
							CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "opaque-secret"}},
						},
					},
				},
			},
		}
		result := r.collectTLSCertData(context.Background(), gw)
		if len(result) != 0 {
			t.Errorf("expected 0 entries for wrong secret type, got %d", len(result))
		}
	})

	t.Run("duplicate ref deduplicated", func(t *testing.T) {
		secret := newTestTLSSecret("dup-cert", "default")
		r := newTestReconciler(scheme, secret)
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{
						Name: "https1", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
						TLS: &gatewayv1.GatewayTLSConfig{
							Mode:            &tlsMode,
							CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "dup-cert"}},
						},
					},
					{
						Name: "https2", Port: 8443, Protocol: gatewayv1.HTTPSProtocolType,
						TLS: &gatewayv1.GatewayTLSConfig{
							Mode:            &tlsMode,
							CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "dup-cert"}},
						},
					},
				},
			},
		}
		result := r.collectTLSCertData(context.Background(), gw)
		if len(result) != 1 {
			t.Errorf("expected 1 entry after dedup, got %d", len(result))
		}
	})
}

func TestReconcileDelete(t *testing.T) {
	scheme := newTestScheme()
	ctx := context.Background()

	t.Run("gateway with finalizer and existing CRB", func(t *testing.T) {
		crb := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "default-gw-chaperone"},
		}
		gateway := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "gw",
				Namespace:  "default",
				Finalizers: []string{FinalizerName},
			},
			Spec: gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
		}
		r := newTestReconciler(scheme, gateway, crb)

		result, err := r.reconcileDelete(ctx, gateway)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Requeue {
			t.Error("expected no requeue")
		}

		// CRB should be deleted
		var checkCRB rbacv1.ClusterRoleBinding
		if err := r.Get(ctx, types.NamespacedName{Name: "default-gw-chaperone"}, &checkCRB); err == nil {
			t.Error("expected CRB to be deleted")
		}

		// Finalizer should be removed
		var checkGW gatewayv1.Gateway
		if err := r.Get(ctx, types.NamespacedName{Name: "gw", Namespace: "default"}, &checkGW); err != nil {
			t.Fatalf("failed to get gateway: %v", err)
		}
		if controllerutil.ContainsFinalizer(&checkGW, FinalizerName) {
			t.Error("expected finalizer to be removed")
		}
	})

	t.Run("gateway with finalizer but CRB already gone", func(t *testing.T) {
		gateway := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "gw",
				Namespace:  "default",
				Finalizers: []string{FinalizerName},
			},
			Spec: gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
		}
		r := newTestReconciler(scheme, gateway) // No CRB

		result, err := r.reconcileDelete(ctx, gateway)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Requeue {
			t.Error("expected no requeue")
		}

		var checkGW gatewayv1.Gateway
		if err := r.Get(ctx, types.NamespacedName{Name: "gw", Namespace: "default"}, &checkGW); err != nil {
			t.Fatalf("failed to get gateway: %v", err)
		}
		if controllerutil.ContainsFinalizer(&checkGW, FinalizerName) {
			t.Error("expected finalizer to be removed")
		}
	})
}

func TestReconcileResource(t *testing.T) {
	scheme := newTestScheme()
	ctx := context.Background()

	makeGateway := func() *gatewayv1.Gateway {
		return &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "gw",
				Namespace: "default",
				UID:       "test-uid",
			},
			Spec: gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
		}
	}

	t.Run("create path: resource doesn't exist", func(t *testing.T) {
		gw := makeGateway()
		r := newTestReconciler(scheme, gw)
		sa := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-chaperone", Namespace: "default"},
		}
		if err := r.reconcileResource(ctx, gw, sa); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var check corev1.ServiceAccount
		if err := r.Get(ctx, types.NamespacedName{Name: "gw-chaperone", Namespace: "default"}, &check); err != nil {
			t.Fatalf("expected resource to be created: %v", err)
		}
	})

	t.Run("configmap update: main.vcl changed", func(t *testing.T) {
		gw := makeGateway()
		existingCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-vcl", Namespace: "default"},
			Data: map[string]string{
				"main.vcl":     "old-vcl",
				"routing.json": `{"version":2,"vhosts":{"example.com":{}}}`,
			},
		}
		r := newTestReconciler(scheme, gw, existingCM)
		desiredCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-vcl", Namespace: "default"},
			Data: map[string]string{
				"main.vcl":     "new-vcl",
				"routing.json": `{"version":2,"vhosts":{}}`,
			},
		}
		if err := r.reconcileResource(ctx, gw, desiredCM); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var check corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{Name: "gw-vcl", Namespace: "default"}, &check); err != nil {
			t.Fatalf("failed to get configmap: %v", err)
		}
		if check.Data["main.vcl"] != "new-vcl" {
			t.Errorf("expected main.vcl to be updated, got %q", check.Data["main.vcl"])
		}
		// routing.json should be preserved from existing
		if check.Data["routing.json"] != `{"version":2,"vhosts":{"example.com":{}}}` {
			t.Errorf("expected routing.json to be preserved, got %q", check.Data["routing.json"])
		}
	})

	t.Run("configmap no-op: main.vcl same", func(t *testing.T) {
		gw := makeGateway()
		existingCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-vcl", Namespace: "default"},
			Data: map[string]string{
				"main.vcl":     "same-vcl",
				"routing.json": `{"version":2,"vhosts":{}}`,
			},
		}
		r := newTestReconciler(scheme, gw, existingCM)
		desiredCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-vcl", Namespace: "default"},
			Data: map[string]string{
				"main.vcl":     "same-vcl",
				"routing.json": `{"version":2,"vhosts":{}}`,
			},
		}
		// Should succeed without error (no-op)
		if err := r.reconcileResource(ctx, gw, desiredCM); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("deployment update: image change", func(t *testing.T) {
		gw := makeGateway()
		existing := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gw"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"app": "gw"},
						Annotations: map[string]string{AnnotationInfraHash: "hash1"},
					},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "varnish-gateway", Image: "img:v1"}}},
				},
			},
		}
		r := newTestReconciler(scheme, gw, existing)
		desired := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gw"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"app": "gw"},
						Annotations: map[string]string{AnnotationInfraHash: "hash1"},
					},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "varnish-gateway", Image: "img:v2"}}},
				},
			},
		}
		if err := r.reconcileResource(ctx, gw, desired); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var check appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Name: "gw", Namespace: "default"}, &check); err != nil {
			t.Fatalf("failed to get deployment: %v", err)
		}
		if check.Spec.Template.Spec.Containers[0].Image != "img:v2" {
			t.Errorf("expected image v2, got %q", check.Spec.Template.Spec.Containers[0].Image)
		}
	})

	t.Run("deployment no-op: same image and hash", func(t *testing.T) {
		gw := makeGateway()
		existing := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gw"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"app": "gw"},
						Annotations: map[string]string{AnnotationInfraHash: "hash1"},
					},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "varnish-gateway", Image: "img:v1"}}},
				},
			},
		}
		r := newTestReconciler(scheme, gw, existing)
		desired := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "gw"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      map[string]string{"app": "gw"},
						Annotations: map[string]string{AnnotationInfraHash: "hash1"},
					},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "varnish-gateway", Image: "img:v1"}}},
				},
			},
		}
		if err := r.reconcileResource(ctx, gw, desired); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("cluster-scoped resource: no owner reference", func(t *testing.T) {
		gw := makeGateway()
		r := newTestReconciler(scheme, gw)
		crb := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "default-gw-chaperone"}, // No namespace = cluster-scoped
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     "varnish-gateway-chaperone",
			},
		}
		if err := r.reconcileResource(ctx, gw, crb); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var check rbacv1.ClusterRoleBinding
		if err := r.Get(ctx, types.NamespacedName{Name: "default-gw-chaperone"}, &check); err != nil {
			t.Fatalf("expected CRB to be created: %v", err)
		}
		// Should have no owner references since it's cluster-scoped
		if len(check.OwnerReferences) != 0 {
			t.Errorf("expected no owner references for cluster-scoped resource, got %d", len(check.OwnerReferences))
		}
	})
}

// ============================================================
// Phase 4: Event Handlers and Helper Tests
// ============================================================

func TestGatewayClassNamesForParams(t *testing.T) {
	scheme := newTestScheme()
	ctx := context.Background()

	t.Run("empty params", func(t *testing.T) {
		r := newTestReconciler(scheme)
		result, err := r.gatewayClassNamesForParams(ctx, map[string]struct{}{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 0 {
			t.Errorf("expected empty result, got %d", len(result))
		}
	})

	t.Run("params matching a GatewayClass", func(t *testing.T) {
		gc := &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: ControllerName,
				ParametersRef: &gatewayv1.ParametersReference{
					Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
					Kind:  "GatewayClassParameters",
					Name:  "my-params",
				},
			},
		}
		r := newTestReconciler(scheme, gc)
		result, err := r.gatewayClassNamesForParams(ctx, map[string]struct{}{"my-params": {}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := result["varnish"]; !ok {
			t.Error("expected 'varnish' class name in result")
		}
	})

	t.Run("params not matching any GatewayClass", func(t *testing.T) {
		gc := &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: ControllerName,
				ParametersRef: &gatewayv1.ParametersReference{
					Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
					Kind:  "GatewayClassParameters",
					Name:  "other-params",
				},
			},
		}
		r := newTestReconciler(scheme, gc)
		result, err := r.gatewayClassNamesForParams(ctx, map[string]struct{}{"my-params": {}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 0 {
			t.Errorf("expected empty result, got %d", len(result))
		}
	})

	t.Run("GatewayClass with no parametersRef ignored", func(t *testing.T) {
		gc := &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
			Spec:       gatewayv1.GatewayClassSpec{ControllerName: ControllerName},
		}
		r := newTestReconciler(scheme, gc)
		result, err := r.gatewayClassNamesForParams(ctx, map[string]struct{}{"my-params": {}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 0 {
			t.Errorf("expected empty result, got %d", len(result))
		}
	})
}

func TestGatewayRequestsForClassNames(t *testing.T) {
	scheme := newTestScheme()
	ctx := context.Background()

	t.Run("empty class names", func(t *testing.T) {
		r := newTestReconciler(scheme)
		result, err := r.gatewayRequestsForClassNames(ctx, map[string]struct{}{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != nil {
			t.Errorf("expected nil, got %v", result)
		}
	})

	t.Run("matching gateways", func(t *testing.T) {
		gw1 := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
		}
		gw2 := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw2", Namespace: "prod"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
		}
		gwOther := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw3", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "other"},
		}
		r := newTestReconciler(scheme, gw1, gw2, gwOther)
		result, err := r.gatewayRequestsForClassNames(ctx, map[string]struct{}{"varnish": {}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 2 {
			t.Errorf("expected 2 requests, got %d", len(result))
		}
	})

	t.Run("no matching gateways", func(t *testing.T) {
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw1", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "other"},
		}
		r := newTestReconciler(scheme, gw)
		result, err := r.gatewayRequestsForClassNames(ctx, map[string]struct{}{"varnish": {}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 0 {
			t.Errorf("expected 0 requests, got %d", len(result))
		}
	})
}

func TestSetListenerStatusesForPatch(t *testing.T) {
	scheme := newTestScheme()
	ctx := context.Background()

	t.Run("single HTTP listener", func(t *testing.T) {
		r := newTestReconciler(scheme)
		original := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default", Generation: 1},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
				},
			},
		}
		patch := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		}

		r.setListenerStatusesForPatch(ctx, patch, original)

		if len(patch.Status.Listeners) != 1 {
			t.Fatalf("expected 1 listener status, got %d", len(patch.Status.Listeners))
		}
		ls := patch.Status.Listeners[0]
		if ls.Name != "http" {
			t.Errorf("expected listener name 'http', got %q", ls.Name)
		}

		// Check Accepted, Programmed, ResolvedRefs all True
		condMap := conditionsToMap(ls.Conditions)
		assertConditionTrue(t, condMap, string(gatewayv1.ListenerConditionAccepted))
		assertConditionTrue(t, condMap, string(gatewayv1.ListenerConditionProgrammed))
		assertConditionTrue(t, condMap, string(gatewayv1.ListenerConditionResolvedRefs))
	})

	t.Run("HTTPS listener with valid cert", func(t *testing.T) {
		tlsMode := gatewayv1.TLSModeTerminate
		secret := newTestTLSSecret("my-cert", "default")
		r := newTestReconciler(scheme, secret)
		original := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default", Generation: 1},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{
						Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
						TLS: &gatewayv1.GatewayTLSConfig{
							Mode:            &tlsMode,
							CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "my-cert"}},
						},
					},
				},
			},
		}
		patch := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		}

		r.setListenerStatusesForPatch(ctx, patch, original)

		ls := patch.Status.Listeners[0]
		condMap := conditionsToMap(ls.Conditions)
		assertConditionTrue(t, condMap, string(gatewayv1.ListenerConditionAccepted))
		assertConditionTrue(t, condMap, string(gatewayv1.ListenerConditionProgrammed))
		assertConditionTrue(t, condMap, string(gatewayv1.ListenerConditionResolvedRefs))
	})

	t.Run("HTTPS listener with missing cert", func(t *testing.T) {
		tlsMode := gatewayv1.TLSModeTerminate
		r := newTestReconciler(scheme) // No secret
		original := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default", Generation: 1},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{
						Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
						TLS: &gatewayv1.GatewayTLSConfig{
							Mode:            &tlsMode,
							CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "missing-cert"}},
						},
					},
				},
			},
		}
		patch := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		}

		r.setListenerStatusesForPatch(ctx, patch, original)

		ls := patch.Status.Listeners[0]
		condMap := conditionsToMap(ls.Conditions)
		// ResolvedRefs should be False
		assertConditionFalse(t, condMap, string(gatewayv1.ListenerConditionResolvedRefs))
		// Programmed should be overridden to False
		assertConditionFalse(t, condMap, string(gatewayv1.ListenerConditionProgrammed))
		// Accepted should still be True
		assertConditionTrue(t, condMap, string(gatewayv1.ListenerConditionAccepted))
	})

	t.Run("preserves condition time from existing status", func(t *testing.T) {
		r := newTestReconciler(scheme)
		existingTime := metav1.Now()
		original := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default", Generation: 2},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
				},
			},
			Status: gatewayv1.GatewayStatus{
				Listeners: []gatewayv1.ListenerStatus{
					{
						Name: "http",
						Conditions: []metav1.Condition{
							{
								Type:               string(gatewayv1.ListenerConditionAccepted),
								Status:             metav1.ConditionTrue,
								LastTransitionTime: existingTime,
							},
						},
					},
				},
			},
		}
		patch := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		}

		r.setListenerStatusesForPatch(ctx, patch, original)

		ls := patch.Status.Listeners[0]
		for _, c := range ls.Conditions {
			if c.Type == string(gatewayv1.ListenerConditionAccepted) {
				if !c.LastTransitionTime.Equal(&existingTime) {
					t.Errorf("expected preserved transition time %v, got %v", existingTime, c.LastTransitionTime)
				}
			}
		}
	})

	t.Run("AttachedRoutes count", func(t *testing.T) {
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default", Generation: 1},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: "varnish",
				Listeners: []gatewayv1.Listener{
					{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
				},
			},
		}
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{
					ParentRefs: []gatewayv1.ParentReference{
						{Name: "gw"},
					},
				},
			},
		}
		r := newTestReconciler(scheme, gw, route)
		patch := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		}

		r.setListenerStatusesForPatch(ctx, patch, gw)

		if len(patch.Status.Listeners) != 1 {
			t.Fatalf("expected 1 listener status, got %d", len(patch.Status.Listeners))
		}
		if patch.Status.Listeners[0].AttachedRoutes != 1 {
			t.Errorf("expected AttachedRoutes=1, got %d", patch.Status.Listeners[0].AttachedRoutes)
		}
	})
}

func TestEnqueueGatewaysForTLSSecret(t *testing.T) {
	scheme := newTestScheme()
	ctx := context.Background()

	tlsMode := gatewayv1.TLSModeTerminate
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
			Listeners: []gatewayv1.Listener{
				{
					Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
					TLS: &gatewayv1.GatewayTLSConfig{
						Mode:            &tlsMode,
						CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "my-tls"}},
					},
				},
			},
		},
	}

	t.Run("non-TLS secret returns nil", func(t *testing.T) {
		r := newTestReconciler(scheme, gw)
		h := r.enqueueGatewaysForTLSSecret()
		requests := extractRequests(t, h, ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "opaque", Namespace: "default"},
			Type:       corev1.SecretTypeOpaque,
		})
		if len(requests) != 0 {
			t.Errorf("expected no requests for non-TLS secret, got %d", len(requests))
		}
	})

	t.Run("our managed secret returns nil", func(t *testing.T) {
		r := newTestReconciler(scheme, gw)
		h := r.enqueueGatewaysForTLSSecret()
		requests := extractRequests(t, h, ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "gw-tls",
				Namespace: "default",
				Labels:    map[string]string{LabelManagedBy: ManagedByValue},
			},
			Type: corev1.SecretTypeTLS,
		})
		if len(requests) != 0 {
			t.Errorf("expected no requests for managed secret, got %d", len(requests))
		}
	})

	t.Run("TLS secret referenced by gateway returns request", func(t *testing.T) {
		r := newTestReconciler(scheme, gw)
		h := r.enqueueGatewaysForTLSSecret()
		requests := extractRequests(t, h, ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "my-tls", Namespace: "default"},
			Type:       corev1.SecretTypeTLS,
			Data:       map[string][]byte{"tls.crt": []byte("cert"), "tls.key": []byte("key")},
		})
		if len(requests) != 1 {
			t.Fatalf("expected 1 request, got %d", len(requests))
		}
		if requests[0].Name != "gw" || requests[0].Namespace != "default" {
			t.Errorf("expected request for gw/default, got %v", requests[0])
		}
	})

	t.Run("TLS secret not referenced returns nil", func(t *testing.T) {
		r := newTestReconciler(scheme, gw)
		h := r.enqueueGatewaysForTLSSecret()
		requests := extractRequests(t, h, ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "unrelated-tls", Namespace: "default"},
			Type:       corev1.SecretTypeTLS,
			Data:       map[string][]byte{"tls.crt": []byte("cert"), "tls.key": []byte("key")},
		})
		if len(requests) != 0 {
			t.Errorf("expected no requests for unrelated TLS secret, got %d", len(requests))
		}
	})
}

func TestEnqueueGatewaysForParams(t *testing.T) {
	scheme := newTestScheme()
	ctx := context.Background()

	t.Run("matching params enqueues gateway", func(t *testing.T) {
		gc := &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: ControllerName,
				ParametersRef: &gatewayv1.ParametersReference{
					Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
					Kind:  "GatewayClassParameters",
					Name:  "my-params",
				},
			},
		}
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
		}
		r := newTestReconciler(scheme, gc, gw)
		h := r.enqueueGatewaysForParams()

		params := &gatewayparamsv1alpha1.GatewayClassParameters{
			ObjectMeta: metav1.ObjectMeta{Name: "my-params"},
		}
		requests := extractRequests(t, h, ctx, params)
		if len(requests) != 1 {
			t.Fatalf("expected 1 request, got %d", len(requests))
		}
		if requests[0].Name != "gw" {
			t.Errorf("expected request for 'gw', got %q", requests[0].Name)
		}
	})

	t.Run("no matching GatewayClass returns nil", func(t *testing.T) {
		r := newTestReconciler(scheme) // No GatewayClass
		h := r.enqueueGatewaysForParams()
		params := &gatewayparamsv1alpha1.GatewayClassParameters{
			ObjectMeta: metav1.ObjectMeta{Name: "my-params"},
		}
		requests := extractRequests(t, h, ctx, params)
		if len(requests) != 0 {
			t.Errorf("expected 0 requests, got %d", len(requests))
		}
	})
}

func TestEnqueueGatewaysForConfigMap(t *testing.T) {
	scheme := newTestScheme()
	ctx := context.Background()

	t.Run("matching ConfigMap enqueues gateway", func(t *testing.T) {
		params := &gatewayparamsv1alpha1.GatewayClassParameters{
			ObjectMeta: metav1.ObjectMeta{Name: "my-params"},
			Spec: gatewayparamsv1alpha1.GatewayClassParametersSpec{
				UserVCLConfigMapRef: &gatewayparamsv1alpha1.ConfigMapReference{
					Name:      "user-vcl",
					Namespace: "default",
				},
			},
		}
		gc := &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "varnish"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: ControllerName,
				ParametersRef: &gatewayv1.ParametersReference{
					Group: gatewayv1.Group(gatewayparamsv1alpha1.GroupName),
					Kind:  "GatewayClassParameters",
					Name:  "my-params",
				},
			},
		}
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
		}
		r := newTestReconciler(scheme, params, gc, gw)
		h := r.enqueueGatewaysForConfigMap()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "user-vcl", Namespace: "default"},
		}
		requests := extractRequests(t, h, ctx, cm)
		if len(requests) != 1 {
			t.Fatalf("expected 1 request, got %d", len(requests))
		}
	})

	t.Run("unrelated ConfigMap returns nil", func(t *testing.T) {
		r := newTestReconciler(scheme) // No params
		h := r.enqueueGatewaysForConfigMap()
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "random", Namespace: "default"},
		}
		requests := extractRequests(t, h, ctx, cm)
		if len(requests) != 0 {
			t.Errorf("expected 0 requests, got %d", len(requests))
		}
	})
}

func TestEnqueueGatewaysForReferenceGrant(t *testing.T) {
	scheme := newTestScheme()
	ctx := context.Background()

	certNS := gatewayv1.Namespace("cert-ns")
	tlsMode := gatewayv1.TLSModeTerminate

	t.Run("grant allowing Secret access enqueues gateway", func(t *testing.T) {
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: "varnish",
				Listeners: []gatewayv1.Listener{
					{
						Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
						TLS: &gatewayv1.GatewayTLSConfig{
							Mode:            &tlsMode,
							CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "cert", Namespace: &certNS}},
						},
					},
				},
			},
		}
		r := newTestReconciler(scheme, gw)
		h := r.enqueueGatewaysForReferenceGrant()

		grant := &gatewayv1beta1.ReferenceGrant{
			ObjectMeta: metav1.ObjectMeta{Name: "allow", Namespace: "cert-ns"},
			Spec: gatewayv1beta1.ReferenceGrantSpec{
				From: []gatewayv1beta1.ReferenceGrantFrom{
					{Group: "gateway.networking.k8s.io", Kind: "Gateway", Namespace: "default"},
				},
				To: []gatewayv1beta1.ReferenceGrantTo{
					{Group: "", Kind: "Secret"},
				},
			},
		}
		requests := extractRequests(t, h, ctx, grant)
		if len(requests) != 1 {
			t.Fatalf("expected 1 request, got %d", len(requests))
		}
	})

	t.Run("grant with no Secret To entries returns nil", func(t *testing.T) {
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: "varnish",
				Listeners: []gatewayv1.Listener{
					{
						Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
						TLS: &gatewayv1.GatewayTLSConfig{
							Mode:            &tlsMode,
							CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "cert", Namespace: &certNS}},
						},
					},
				},
			},
		}
		r := newTestReconciler(scheme, gw)
		h := r.enqueueGatewaysForReferenceGrant()

		grant := &gatewayv1beta1.ReferenceGrant{
			ObjectMeta: metav1.ObjectMeta{Name: "allow", Namespace: "cert-ns"},
			Spec: gatewayv1beta1.ReferenceGrantSpec{
				From: []gatewayv1beta1.ReferenceGrantFrom{
					{Group: "gateway.networking.k8s.io", Kind: "Gateway", Namespace: "default"},
				},
				To: []gatewayv1beta1.ReferenceGrantTo{
					{Group: "", Kind: "ConfigMap"}, // Not Secret
				},
			},
		}
		requests := extractRequests(t, h, ctx, grant)
		if len(requests) != 0 {
			t.Errorf("expected 0 requests, got %d", len(requests))
		}
	})

	t.Run("grant with no Gateway From entries returns nil", func(t *testing.T) {
		r := newTestReconciler(scheme)
		h := r.enqueueGatewaysForReferenceGrant()

		grant := &gatewayv1beta1.ReferenceGrant{
			ObjectMeta: metav1.ObjectMeta{Name: "allow", Namespace: "cert-ns"},
			Spec: gatewayv1beta1.ReferenceGrantSpec{
				From: []gatewayv1beta1.ReferenceGrantFrom{
					{Group: "some.other.group", Kind: "SomeKind", Namespace: "default"},
				},
				To: []gatewayv1beta1.ReferenceGrantTo{
					{Group: "", Kind: "Secret"},
				},
			},
		}
		requests := extractRequests(t, h, ctx, grant)
		if len(requests) != 0 {
			t.Errorf("expected 0 requests, got %d", len(requests))
		}
	})
}

func TestEnqueueGatewaysForHTTPRoute(t *testing.T) {
	scheme := newTestScheme()
	ctx := context.Background()

	t.Run("route referencing our gateway enqueues it", func(t *testing.T) {
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
		}
		r := newTestReconciler(scheme, gw)
		h := r.enqueueGatewaysForHTTPRoute()

		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{
					ParentRefs: []gatewayv1.ParentReference{
						{Name: "gw"},
					},
				},
			},
		}
		requests := extractRequests(t, h, ctx, route)
		if len(requests) != 1 {
			t.Fatalf("expected 1 request, got %d", len(requests))
		}
		if requests[0].Name != "gw" {
			t.Errorf("expected request for 'gw', got %q", requests[0].Name)
		}
	})

	t.Run("route referencing different gateway class not enqueued", func(t *testing.T) {
		gw := &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "other-gw", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "other-class"},
		}
		r := newTestReconciler(scheme, gw)
		h := r.enqueueGatewaysForHTTPRoute()

		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{
					ParentRefs: []gatewayv1.ParentReference{
						{Name: "other-gw"},
					},
				},
			},
		}
		requests := extractRequests(t, h, ctx, route)
		if len(requests) != 0 {
			t.Errorf("expected 0 requests, got %d", len(requests))
		}
	})
}

// ============================================================
// Test Helpers
// ============================================================

// keys returns the keys of a map as a slice (for error messages).
func keys[V any](m map[string]V) []string {
	result := make([]string, 0, len(m))
	for k := range m {
		result = append(result, k)
	}
	return result
}

// conditionsToMap converts a slice of conditions to a map keyed by type.
func conditionsToMap(conditions []metav1.Condition) map[string]metav1.Condition {
	m := make(map[string]metav1.Condition)
	for _, c := range conditions {
		m[c.Type] = c
	}
	return m
}

func assertConditionTrue(t *testing.T, conditions map[string]metav1.Condition, condType string) {
	t.Helper()
	c, ok := conditions[condType]
	if !ok {
		t.Errorf("expected condition %q to exist", condType)
		return
	}
	if c.Status != metav1.ConditionTrue {
		t.Errorf("expected condition %q to be True, got %s (reason: %s)", condType, c.Status, c.Reason)
	}
}

func assertConditionFalse(t *testing.T, conditions map[string]metav1.Condition, condType string) {
	t.Helper()
	c, ok := conditions[condType]
	if !ok {
		t.Errorf("expected condition %q to exist", condType)
		return
	}
	if c.Status != metav1.ConditionFalse {
		t.Errorf("expected condition %q to be False, got %s (reason: %s)", condType, c.Status, c.Reason)
	}
}

// extractRequests invokes an EventHandler's MapFunc to get reconcile requests.
// It creates a fake event and workqueue, triggers the handler, and collects results.
func extractRequests(t *testing.T, h handler.EventHandler, ctx context.Context, obj client.Object) []ctrl.Request {
	t.Helper()
	q := &fakeQueue{}
	h.Create(ctx, event.CreateEvent{Object: obj}, q)
	return q.requests
}

// fakeQueue implements workqueue.TypedRateLimitingInterface[reconcile.Request]
// just enough for handler testing.
type fakeQueue struct {
	requests []ctrl.Request
}

func (q *fakeQueue) Add(item reconcile.Request)                             { q.requests = append(q.requests, item) }
func (q *fakeQueue) Len() int                                               { return len(q.requests) }
func (q *fakeQueue) Get() (reconcile.Request, bool)                         { return reconcile.Request{}, false }
func (q *fakeQueue) Done(item reconcile.Request)                            {}
func (q *fakeQueue) ShutDown()                                              {}
func (q *fakeQueue) ShutDownWithDrain()                                     {}
func (q *fakeQueue) ShuttingDown() bool                                     { return false }
func (q *fakeQueue) AddAfter(item reconcile.Request, duration time.Duration) {}
func (q *fakeQueue) AddRateLimited(item reconcile.Request)                  {}
func (q *fakeQueue) Forget(item reconcile.Request)                          {}
func (q *fakeQueue) NumRequeues(item reconcile.Request) int                 { return 0 }


