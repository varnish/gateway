package controller

import (
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
)

func testReconcilerSimple() *GatewayReconciler {
	return &GatewayReconciler{
		Config: Config{GatewayImage: "ghcr.io/varnish/varnish-gateway:latest"},
		Logger: slog.Default(),
	}
}

func testGateway(name, namespace string, listeners ...gatewayv1.Listener) *gatewayv1.Gateway {
	if len(listeners) == 0 {
		listeners = []gatewayv1.Listener{
			{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
		}
	}
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "varnish",
			Listeners:        listeners,
		},
	}
}

// --- listenerSocketName ---

func TestListenerSocketName(t *testing.T) {
	tests := []struct {
		name     string
		listener gatewayv1.Listener
		want     string
	}{
		{"http", gatewayv1.Listener{Port: 80, Protocol: gatewayv1.HTTPProtocolType}, "http-80"},
		{"https", gatewayv1.Listener{Port: 443, Protocol: gatewayv1.HTTPSProtocolType}, "https-443"},
		{"tls", gatewayv1.Listener{Port: 443, Protocol: gatewayv1.TLSProtocolType}, "https-443"},
		{"custom port", gatewayv1.Listener{Port: 3000, Protocol: gatewayv1.HTTPProtocolType}, "http-3000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := listenerSocketName(&tt.listener)
			if got != tt.want {
				t.Errorf("listenerSocketName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- listenerSpecs ---

func TestListenerSpecs(t *testing.T) {
	tests := []struct {
		name      string
		listeners []gatewayv1.Listener
		want      string
	}{
		{
			name: "single http",
			listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
			want: "http-80",
		},
		{
			name: "http and https sorted",
			listeners: []gatewayv1.Listener{
				{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
			want: "http-80,https-443",
		},
		{
			name: "deduplicates same port",
			listeners: []gatewayv1.Listener{
				{Name: "web1", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: ptrTo(gatewayv1.Hostname("a.example.com"))},
				{Name: "web2", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: ptrTo(gatewayv1.Hostname("b.example.com"))},
			},
			want: "http-80",
		},
		{
			name:      "no listeners",
			listeners: []gatewayv1.Listener{},
			want:      "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := &gatewayv1.Gateway{Spec: gatewayv1.GatewaySpec{Listeners: tt.listeners}}
			got := listenerSpecs(gw)
			if got != tt.want {
				t.Errorf("listenerSpecs() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- hasHTTPSListener ---

func TestHasHTTPSListener(t *testing.T) {
	tests := []struct {
		name      string
		listeners []gatewayv1.Listener
		want      bool
	}{
		{"http only", []gatewayv1.Listener{{Protocol: gatewayv1.HTTPProtocolType}}, false},
		{"https", []gatewayv1.Listener{{Protocol: gatewayv1.HTTPSProtocolType}}, true},
		{"tls", []gatewayv1.Listener{{Protocol: gatewayv1.TLSProtocolType}}, true},
		{"mixed", []gatewayv1.Listener{
			{Protocol: gatewayv1.HTTPProtocolType},
			{Protocol: gatewayv1.HTTPSProtocolType},
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := &gatewayv1.Gateway{Spec: gatewayv1.GatewaySpec{Listeners: tt.listeners}}
			if got := hasHTTPSListener(gw); got != tt.want {
				t.Errorf("hasHTTPSListener() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- buildVCLConfigMap ---

func TestBuildVCLConfigMap_RoutingJSON(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "test-ns")

	cm := r.buildVCLConfigMap(gw, "vcl 4.1;\nbackend default { .host = \"127.0.0.1\"; }")

	if cm.Name != "my-gw-vcl" {
		t.Errorf("name = %q, want %q", cm.Name, "my-gw-vcl")
	}
	if cm.Namespace != "test-ns" {
		t.Errorf("namespace = %q, want %q", cm.Namespace, "test-ns")
	}
	if _, ok := cm.Data["main.vcl"]; !ok {
		t.Error("missing main.vcl key")
	}
	if _, ok := cm.Data["routing.json"]; !ok {
		t.Error("missing routing.json key")
	}
}

// --- buildAdminSecret ---

func TestBuildAdminSecret_SecretLength(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	s := r.buildAdminSecret(gw)

	if s.Name != "my-gw-secret" {
		t.Errorf("name = %q, want %q", s.Name, "my-gw-secret")
	}
	if s.Type != corev1.SecretTypeOpaque {
		t.Errorf("type = %v, want Opaque", s.Type)
	}
	secret, ok := s.Data["secret"]
	if !ok {
		t.Fatal("missing 'secret' key")
	}
	if len(secret) != 64 { // 32 bytes hex-encoded
		t.Errorf("secret length = %d, want 64", len(secret))
	}
}

// --- buildTLSSecret ---

func TestBuildTLSSecret_CertData(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	certData := map[string][]byte{"my-cert.pem": []byte("cert-data")}
	s := r.buildTLSSecret(gw, certData)

	if s.Name != "my-gw-tls" {
		t.Errorf("name = %q, want %q", s.Name, "my-gw-tls")
	}
	if string(s.Data["my-cert.pem"]) != "cert-data" {
		t.Error("cert data mismatch")
	}
}

// --- buildBackendCASecret ---

func TestBuildBackendCASecret(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	s := r.buildBackendCASecret(gw, []byte("ca-bundle-pem"))

	if s.Name != "my-gw-backend-tls" {
		t.Errorf("name = %q, want %q", s.Name, "my-gw-backend-tls")
	}
	if string(s.Data["ca-bundle.crt"]) != "ca-bundle-pem" {
		t.Error("ca bundle data mismatch")
	}
}

// --- buildServiceAccount ---

func TestBuildServiceAccount_Naming(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "test-ns")

	sa := r.buildServiceAccount(gw)

	if sa.Name != "my-gw-chaperone" {
		t.Errorf("name = %q, want %q", sa.Name, "my-gw-chaperone")
	}
	if sa.Namespace != "test-ns" {
		t.Errorf("namespace = %q, want %q", sa.Namespace, "test-ns")
	}
}

// --- buildClusterRoleBinding ---

func TestBuildClusterRoleBinding_Subjects(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "prod")

	crb := r.buildClusterRoleBinding(gw)

	if crb.Name != "prod-my-gw-chaperone" {
		t.Errorf("name = %q, want %q", crb.Name, "prod-my-gw-chaperone")
	}
	if crb.RoleRef.Name != "varnish-gateway-chaperone" {
		t.Errorf("roleRef = %q, want %q", crb.RoleRef.Name, "varnish-gateway-chaperone")
	}
	if len(crb.Subjects) != 1 || crb.Subjects[0].Name != "my-gw-chaperone" {
		t.Errorf("unexpected subjects: %v", crb.Subjects)
	}
	if crb.Subjects[0].Namespace != "prod" {
		t.Errorf("subject namespace = %q, want %q", crb.Subjects[0].Namespace, "prod")
	}
}

// --- buildService ---

func TestBuildService_SingleListener(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	svc := r.buildService(gw)

	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("type = %v, want LoadBalancer", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(svc.Spec.Ports))
	}
	if svc.Spec.Ports[0].Port != 80 {
		t.Errorf("port = %d, want 80", svc.Spec.Ports[0].Port)
	}
	if svc.Spec.Ports[0].Name != "http-80" {
		t.Errorf("port name = %q, want %q", svc.Spec.Ports[0].Name, "http-80")
	}
}

func TestBuildService_MultipleListeners(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default",
		gatewayv1.Listener{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
		gatewayv1.Listener{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
	)

	svc := r.buildService(gw)

	if len(svc.Spec.Ports) != 2 {
		t.Fatalf("expected 2 ports, got %d", len(svc.Spec.Ports))
	}
}

func TestBuildService_DeduplicatesPorts(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default",
		gatewayv1.Listener{Name: "web1", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: ptrTo(gatewayv1.Hostname("a.example.com"))},
		gatewayv1.Listener{Name: "web2", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: ptrTo(gatewayv1.Hostname("b.example.com"))},
	)

	svc := r.buildService(gw)

	if len(svc.Spec.Ports) != 1 {
		t.Errorf("expected 1 deduplicated port, got %d", len(svc.Spec.Ports))
	}
}

func TestBuildService_NoListenersFallback(t *testing.T) {
	r := testReconcilerSimple()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "my-gw", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "varnish"},
	}

	svc := r.buildService(gw)

	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("expected 1 fallback port, got %d", len(svc.Spec.Ports))
	}
	if svc.Spec.Ports[0].Port != 80 {
		t.Errorf("fallback port = %d, want 80", svc.Spec.Ports[0].Port)
	}
}

// --- buildVolumes ---

func TestBuildVolumes_HTTPOnly(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	volumes := r.buildVolumes(gw, nil, false)

	// Should have vcl-config and varnish-run, no tls or backend-ca
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(volumes))
	}
	names := map[string]bool{}
	for _, v := range volumes {
		names[v.Name] = true
	}
	if !names[volumeVCLConfig] || !names[volumeVarnishRun] {
		t.Errorf("unexpected volume names: %v", names)
	}
}

func TestBuildVolumes_WithTLS(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default",
		gatewayv1.Listener{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
	)

	volumes := r.buildVolumes(gw, nil, false)

	names := map[string]bool{}
	for _, v := range volumes {
		names[v.Name] = true
	}
	if !names[volumeTLSCerts] {
		t.Error("expected tls-certs volume for HTTPS listener")
	}
}

func TestBuildVolumes_WithBackendTLS(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	volumes := r.buildVolumes(gw, nil, true)

	names := map[string]bool{}
	for _, v := range volumes {
		names[v.Name] = true
	}
	if !names[volumeBackendCA] {
		t.Error("expected backend-ca volume when hasBackendTLS=true")
	}
}

func TestBuildVolumes_WithExtra(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	extra := []corev1.Volume{{Name: "extra-vol"}}
	volumes := r.buildVolumes(gw, extra, false)

	names := map[string]bool{}
	for _, v := range volumes {
		names[v.Name] = true
	}
	if !names["extra-vol"] {
		t.Error("expected extra volume to be included")
	}
}

// --- buildDeployment ---

func TestBuildDeployment_Basic(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	dep := r.buildDeployment(gw, "test-image:latest", nil, nil, "abc123", nil, nil, nil, nil, false)

	if dep.Name != "my-gw" {
		t.Errorf("name = %q, want %q", dep.Name, "my-gw")
	}
	if *dep.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", *dep.Spec.Replicas)
	}
	// Check infra hash annotation
	hash := dep.Spec.Template.Annotations[AnnotationInfraHash]
	if hash != "abc123" {
		t.Errorf("infra hash = %q, want %q", hash, "abc123")
	}
	// Check service account
	if dep.Spec.Template.Spec.ServiceAccountName != "my-gw-chaperone" {
		t.Errorf("serviceAccountName = %q, want %q", dep.Spec.Template.Spec.ServiceAccountName, "my-gw-chaperone")
	}
}

func TestBuildDeployment_WithImagePullSecrets(t *testing.T) {
	r := &GatewayReconciler{
		Config: Config{
			GatewayImage:     "test-image:latest",
			ImagePullSecrets: "secret-a,secret-b",
		},
		Logger: slog.Default(),
	}
	gw := testGateway("my-gw", "default")

	dep := r.buildDeployment(gw, "test-image:latest", nil, nil, "hash", nil, nil, nil, nil, false)

	secrets := dep.Spec.Template.Spec.ImagePullSecrets
	if len(secrets) != 2 {
		t.Fatalf("expected 2 image pull secrets, got %d", len(secrets))
	}
	if secrets[0].Name != "secret-a" || secrets[1].Name != "secret-b" {
		t.Errorf("unexpected secrets: %v", secrets)
	}
}

func TestBuildDeployment_WithLoggingSidecar(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")
	logging := &gatewayparamsv1alpha1.VarnishLogging{Mode: "varnishncsa"}

	dep := r.buildDeployment(gw, "test-image:latest", nil, logging, "hash", nil, nil, nil, nil, false)

	containers := dep.Spec.Template.Spec.Containers
	if len(containers) != 2 {
		t.Fatalf("expected 2 containers (main + sidecar), got %d", len(containers))
	}
	if containers[1].Name != "varnish-log" {
		t.Errorf("sidecar name = %q, want %q", containers[1].Name, "varnish-log")
	}
}

func TestBuildDeployment_WithBackendTLS(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	dep := r.buildDeployment(gw, "test-image:latest", nil, nil, "hash", nil, nil, nil, nil, true)

	// Check that backend-ca volume exists
	volumes := dep.Spec.Template.Spec.Volumes
	found := false
	for _, v := range volumes {
		if v.Name == volumeBackendCA {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected backend-ca volume when hasBackendTLS=true")
	}

	// Check SSL_CERT_FILE env var in main container
	mainContainer := dep.Spec.Template.Spec.Containers[0]
	foundEnv := false
	for _, e := range mainContainer.Env {
		if e.Name == "SSL_CERT_FILE" {
			foundEnv = true
			break
		}
	}
	if !foundEnv {
		t.Error("expected SSL_CERT_FILE env var when hasBackendTLS=true")
	}
}

// --- buildGatewayContainer ---

func TestBuildGatewayContainer_EnvVars(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	container := r.buildGatewayContainer(gw, "test:latest", nil, nil, nil, false)

	envMap := map[string]string{}
	for _, e := range container.Env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
		}
	}

	if envMap["CONFIGMAP_NAME"] != "my-gw-vcl" {
		t.Errorf("CONFIGMAP_NAME = %q, want %q", envMap["CONFIGMAP_NAME"], "my-gw-vcl")
	}
	if envMap["GATEWAY_NAME"] != "my-gw" {
		t.Errorf("GATEWAY_NAME = %q, want %q", envMap["GATEWAY_NAME"], "my-gw")
	}
	// Should not have TLS_CERT_DIR for HTTP-only
	if _, ok := envMap["TLS_CERT_DIR"]; ok {
		t.Error("unexpected TLS_CERT_DIR for HTTP-only gateway")
	}
}

func TestBuildGatewayContainer_VarnishListen(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default",
		gatewayv1.Listener{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
		gatewayv1.Listener{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
	)

	container := r.buildGatewayContainer(gw, "test:latest", nil, nil, nil, false)

	var varnishListen string
	for _, e := range container.Env {
		if e.Name == "VARNISH_LISTEN" {
			varnishListen = e.Value
			break
		}
	}
	if varnishListen == "" {
		t.Fatal("missing VARNISH_LISTEN env var")
	}
	// Should contain ghost-reload, http-80, and https-443
	for _, want := range []string{"ghost-reload=127.0.0.1:1969,http", "http-80=:80,http", "https-443=:443,https"} {
		if !strings.Contains(varnishListen, want) {
			t.Errorf("VARNISH_LISTEN %q missing %q", varnishListen, want)
		}
	}
}

func TestBuildGatewayContainer_WithVarnishdExtraArgs(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	container := r.buildGatewayContainer(gw, "test:latest", []string{"-p", "thread_pool_stack=160k"}, nil, nil, false)

	var extraArgs string
	for _, e := range container.Env {
		if e.Name == "VARNISHD_EXTRA_ARGS" {
			extraArgs = e.Value
			break
		}
	}
	if extraArgs != "-p;thread_pool_stack=160k" {
		t.Errorf("VARNISHD_EXTRA_ARGS = %q, want %q", extraArgs, "-p;thread_pool_stack=160k")
	}
}

func TestBuildGatewayContainer_TLSEnv(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default",
		gatewayv1.Listener{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
	)

	container := r.buildGatewayContainer(gw, "test:latest", nil, nil, nil, false)

	envMap := map[string]string{}
	for _, e := range container.Env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
		}
	}
	if envMap["TLS_CERT_DIR"] != "/etc/varnish/tls" {
		t.Errorf("TLS_CERT_DIR = %q, want %q", envMap["TLS_CERT_DIR"], "/etc/varnish/tls")
	}
}

func TestBuildGatewayContainer_BackendTLSEnv(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	container := r.buildGatewayContainer(gw, "test:latest", nil, nil, nil, true)

	envMap := map[string]string{}
	for _, e := range container.Env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
		}
	}
	if envMap["SSL_CERT_FILE"] != "/etc/varnish/backend-ca/ca-bundle.crt" {
		t.Errorf("SSL_CERT_FILE = %q, want set", envMap["SSL_CERT_FILE"])
	}
}

func TestBuildGatewayContainer_Ports(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default",
		gatewayv1.Listener{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
	)

	container := r.buildGatewayContainer(gw, "test:latest", nil, nil, nil, false)

	portNames := map[string]int32{}
	for _, p := range container.Ports {
		portNames[p.Name] = p.ContainerPort
	}
	if portNames["health"] != int32(chaperoneHealthPort) {
		t.Errorf("health port = %d, want %d", portNames["health"], chaperoneHealthPort)
	}
	if portNames["http-80"] != 80 {
		t.Errorf("http-80 port = %d, want 80", portNames["http-80"])
	}
}

func TestBuildGatewayContainer_DeduplicatesListenerPorts(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default",
		gatewayv1.Listener{Name: "web1", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: ptrTo(gatewayv1.Hostname("a.example.com"))},
		gatewayv1.Listener{Name: "web2", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: ptrTo(gatewayv1.Hostname("b.example.com"))},
	)

	container := r.buildGatewayContainer(gw, "test:latest", nil, nil, nil, false)

	// Should have health + dashboard + one http-80, not two
	httpPorts := 0
	for _, p := range container.Ports {
		if p.Name == "http-80" {
			httpPorts++
		}
	}
	if httpPorts != 1 {
		t.Errorf("expected 1 http-80 port, got %d", httpPorts)
	}
}

func TestBuildGatewayContainer_CustomResources(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	custom := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    mustParseQuantity("500m"),
			corev1.ResourceMemory: mustParseQuantity("1Gi"),
		},
	}
	container := r.buildGatewayContainer(gw, "test:latest", nil, nil, custom, false)

	if container.Resources.Requests.Cpu().String() != "500m" {
		t.Errorf("cpu request = %s, want 500m", container.Resources.Requests.Cpu())
	}
}

func TestBuildGatewayContainer_DefaultResources(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	container := r.buildGatewayContainer(gw, "test:latest", nil, nil, nil, false)

	if container.Resources.Requests.Cpu().String() != "100m" {
		t.Errorf("default cpu request = %s, want 100m", container.Resources.Requests.Cpu())
	}
	if container.Resources.Requests.Memory().String() != "256Mi" {
		t.Errorf("default memory request = %s, want 256Mi", container.Resources.Requests.Memory())
	}
}

func TestBuildGatewayContainer_VolumeMounts_BackendTLS(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")

	container := r.buildGatewayContainer(gw, "test:latest", nil, nil, nil, true)

	mountNames := map[string]bool{}
	for _, m := range container.VolumeMounts {
		mountNames[m.Name] = true
	}
	if !mountNames[volumeBackendCA] {
		t.Error("expected backend-ca volume mount when hasBackendTLS=true")
	}
}

// --- buildLoggingSidecar ---

func TestBuildLoggingSidecar_Varnishlog(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")
	logging := &gatewayparamsv1alpha1.VarnishLogging{Mode: "varnishlog"}

	container := r.buildLoggingSidecar(gw, "test:latest", logging)

	if container.Name != "varnish-log" {
		t.Errorf("name = %q, want %q", container.Name, "varnish-log")
	}
	if len(container.Command) != 1 || container.Command[0] != "varnishlog" {
		t.Errorf("command = %v, want [varnishlog]", container.Command)
	}
}

func TestBuildLoggingSidecar_VarnishncsaWithFormat(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")
	logging := &gatewayparamsv1alpha1.VarnishLogging{
		Mode:   "varnishncsa",
		Format: "%h %s %b",
	}

	container := r.buildLoggingSidecar(gw, "test:latest", logging)

	foundFormat := false
	for i, arg := range container.Args {
		if arg == "-F" && i+1 < len(container.Args) && container.Args[i+1] == "%h %s %b" {
			foundFormat = true
			break
		}
	}
	if !foundFormat {
		t.Errorf("expected -F format in args: %v", container.Args)
	}
}

func TestBuildLoggingSidecar_CustomImage(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")
	logging := &gatewayparamsv1alpha1.VarnishLogging{
		Mode:  "varnishlog",
		Image: "custom-log:v1",
	}

	container := r.buildLoggingSidecar(gw, "test:latest", logging)

	if container.Image != "custom-log:v1" {
		t.Errorf("image = %q, want %q", container.Image, "custom-log:v1")
	}
}

func TestBuildLoggingSidecar_DefaultImage(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")
	logging := &gatewayparamsv1alpha1.VarnishLogging{Mode: "varnishlog"}

	container := r.buildLoggingSidecar(gw, "test:latest", logging)

	if container.Image != "test:latest" {
		t.Errorf("image = %q, want %q (should use effectiveImage)", container.Image, "test:latest")
	}
}

func TestBuildLoggingSidecar_ExtraArgs(t *testing.T) {
	r := testReconcilerSimple()
	gw := testGateway("my-gw", "default")
	logging := &gatewayparamsv1alpha1.VarnishLogging{
		Mode:      "varnishlog",
		ExtraArgs: []string{"-g", "request"},
	}

	container := r.buildLoggingSidecar(gw, "test:latest", logging)

	// Extra args should appear after the base args
	found := false
	for i, arg := range container.Args {
		if arg == "-g" && i+1 < len(container.Args) && container.Args[i+1] == "request" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected extra args in: %v", container.Args)
	}
}

