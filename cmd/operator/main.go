package main

import (
	"flag"
	"log/slog"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gatewayparamsv1alpha1 "github.com/varnish/gateway/api/v1alpha1"
	"github.com/varnish/gateway/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(gatewayparamsv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool

	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "Metrics endpoint address")
	flag.StringVar(&probeAddr, "health-probe-addr", ":8081", "Health probe endpoint address")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election")
	flag.Parse()

	// Configure slog
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	// Load config from environment
	cfg := controller.Config{
		GatewayClassName:    getEnvOrDefault("GATEWAY_CLASS_NAME", "varnish"),
		DefaultVarnishImage: getEnvOrDefault("DEFAULT_VARNISH_IMAGE", "quay.io/varnish-software/varnish-plus:7.6"),
		SidecarImage:        getEnvOrDefault("SIDECAR_IMAGE", "ghcr.io/varnish/gateway-sidecar:latest"),
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: server.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "varnish-gateway-operator",
	})
	if err != nil {
		logger.Error("unable to create manager", "error", err)
		os.Exit(1)
	}

	// Setup Gateway controller
	if err := (&controller.GatewayReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: cfg,
		Logger: logger.With("controller", "Gateway"),
	}).SetupWithManager(mgr); err != nil {
		logger.Error("unable to create controller", "controller", "Gateway", "error", err)
		os.Exit(1)
	}

	// Setup HTTPRoute controller
	if err := (&controller.HTTPRouteReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: cfg,
		Logger: logger.With("controller", "HTTPRoute"),
	}).SetupWithManager(mgr); err != nil {
		logger.Error("unable to create controller", "controller", "HTTPRoute", "error", err)
		os.Exit(1)
	}

	// Setup health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error("unable to setup health check", "error", err)
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error("unable to setup readiness check", "error", err)
		os.Exit(1)
	}

	logger.Info("starting operator",
		"gatewayClassName", cfg.GatewayClassName,
		"varnishImage", cfg.DefaultVarnishImage,
		"sidecarImage", cfg.SidecarImage)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error("manager exited with error", "error", err)
		os.Exit(1)
	}
}

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
