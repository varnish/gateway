//go:build integration && !race

package controller

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
)

// EnvtestEnvironment holds the envtest environment and client
type EnvtestEnvironment struct {
	Env    *envtest.Environment
	Client client.Client
	Scheme *runtime.Scheme
}

// SetupEnvtest initializes the envtest environment and installs CRDs
func SetupEnvtest() (*EnvtestEnvironment, error) {
	// Create scheme with all required types
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := gatewayv1.Install(scheme); err != nil {
		return nil, err
	}
	if err := gatewayv1beta1.Install(scheme); err != nil {
		return nil, err
	}
	if err := gatewayparamsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}

	// Setup envtest environment
	// CRDDirectoryPaths points to directories containing CRDs:
	// - testdata: Gateway API CRDs (downloaded for testing)
	// - deploy: Our custom CRDs (GatewayClassParameters)
	//
	// BinaryAssetsDirectory: If KUBEBUILDER_ASSETS is set, convert to absolute path
	// (envtest requires absolute paths, but setup-envtest may return relative paths)
	binaryAssetsDir := os.Getenv("KUBEBUILDER_ASSETS")
	if binaryAssetsDir != "" && !filepath.IsAbs(binaryAssetsDir) {
		absDir, err := filepath.Abs(binaryAssetsDir)
		if err == nil {
			binaryAssetsDir = absDir
		}
	}

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("testdata"),           // Gateway API CRDs
			filepath.Join("..", "..", "deploy"), // Custom CRDs
		},
		ErrorIfCRDPathMissing: true,
		Scheme:                scheme,
		BinaryAssetsDirectory: binaryAssetsDir,
	}

	// Start the test environment (kube-apiserver + etcd)
	cfg, err := testEnv.Start()
	if err != nil {
		return nil, err
	}

	// Create a client for the test environment
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = testEnv.Stop()
		return nil, err
	}

	return &EnvtestEnvironment{
		Env:    testEnv,
		Client: k8sClient,
		Scheme: scheme,
	}, nil
}

// TeardownEnvtest stops the envtest environment
func TeardownEnvtest(env *EnvtestEnvironment) error {
	if env != nil && env.Env != nil {
		return env.Env.Stop()
	}
	return nil
}

// NewEnvtestGatewayReconciler creates a GatewayReconciler with the envtest client
func NewEnvtestGatewayReconciler(env *EnvtestEnvironment) *GatewayReconciler {
	return &GatewayReconciler{
		Client: env.Client,
		Scheme: env.Scheme,
		Config: Config{
			GatewayClassName: "varnish",
			GatewayImage:     "ghcr.io/varnish/varnish-gateway:latest",
		},
		Logger: slog.Default(),
	}
}

// NewEnvtestHTTPRouteReconciler creates an HTTPRouteReconciler with the envtest client
func NewEnvtestHTTPRouteReconciler(env *EnvtestEnvironment) *HTTPRouteReconciler {
	return &HTTPRouteReconciler{
		Client: env.Client,
		Scheme: env.Scheme,
		Config: Config{
			GatewayClassName: "varnish",
		},
		Logger: slog.Default(),
	}
}

// CleanupEnvtestResources deletes all test resources from the environment
func CleanupEnvtestResources(ctx context.Context, env *EnvtestEnvironment) error {
	// Delete all gateways
	if err := env.Client.DeleteAllOf(ctx, &gatewayv1.Gateway{}, client.InNamespace("default")); err != nil {
		return err
	}
	// Delete all gateway classes
	if err := env.Client.DeleteAllOf(ctx, &gatewayv1.GatewayClass{}); err != nil {
		return err
	}
	return nil
}
