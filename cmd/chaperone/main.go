package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/varnish/gateway/internal/backends"
	"github.com/varnish/gateway/internal/varnishadm"
	"github.com/varnish/gateway/internal/vcl"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

//go:embed .version
var version string

// Config holds the sidecar configuration from environment variables
type Config struct {
	VarnishAdminAddr  string // listen address for varnishadm (e.g., "localhost:6082")
	VarnishSecretPath string // path to varnish admin secret file
	BackendsFilePath  string // where to write backends.conf
	VCLPath           string // path to watch for VCL changes
	ServicesFilePath  string // path to services.json
	Namespace         string // kubernetes namespace to watch
	HealthAddr        string // address for health endpoint
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		VarnishAdminAddr:  getEnvOrDefault("VARNISH_ADMIN_ADDR", "localhost:6082"),
		VarnishSecretPath: getEnvOrDefault("VARNISH_SECRET_PATH", "/etc/varnish/secret"),
		BackendsFilePath:  getEnvOrDefault("BACKENDS_FILE_PATH", "/var/run/varnish/backends.conf"),
		VCLPath:           getEnvOrDefault("VCL_PATH", "/var/run/varnish/main.vcl"),
		ServicesFilePath:  getEnvOrDefault("SERVICES_FILE_PATH", "/var/run/varnish/services.json"),
		Namespace:         getEnvOrDefault("NAMESPACE", "default"),
		HealthAddr:        getEnvOrDefault("HEALTH_ADDR", ":8080"),
	}

	return cfg, nil
}

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// parseAdminPort extracts the port number from an address string like "localhost:6082"
func parseAdminPort(addr string) (uint16, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("net.SplitHostPort(%s): %w", addr, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("strconv.ParseUint(%s): %w", portStr, err)
	}
	return uint16(port), nil
}

func readSecret(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("os.ReadFile(%s): %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

const useJSONLogging = false // set to true for production/k8s

func configureLogger() {
	var handler slog.Handler
	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}
	if useJSONLogging {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))
}

func main() {
	configureLogger()
	if err := run(); err != nil {
		slog.Error("sidecar failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	slog.Info("sidecar starting", "version", strings.TrimSpace(version))

	// Load configuration from environment
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loadConfig: %w", err)
	}

	slog.Info("configuration loaded",
		"varnishAdminAddr", cfg.VarnishAdminAddr,
		"varnishSecretPath", cfg.VarnishSecretPath,
		"backendsFilePath", cfg.BackendsFilePath,
		"vclPath", cfg.VCLPath,
		"servicesFilePath", cfg.ServicesFilePath,
		"namespace", cfg.Namespace,
	)

	// Read varnish admin secret
	secret, err := readSecret(cfg.VarnishSecretPath)
	if err != nil {
		return fmt.Errorf("readSecret: %w", err)
	}

	// Parse admin port from address
	adminPort, err := parseAdminPort(cfg.VarnishAdminAddr)
	if err != nil {
		return fmt.Errorf("parseAdminPort: %w", err)
	}

	// Create Kubernetes client - try in-cluster first, fall back to kubeconfig
	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		slog.Info("not running in-cluster, using kubeconfig")
		k8sConfig, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return fmt.Errorf("clientcmd.ClientConfig: %w", err)
		}
	}

	k8sClient, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}

	// Set up context with signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Create components
	logger := slog.Default()

	// 1. varnishadm server - listens for connections from Varnish
	vadm := varnishadm.New(adminPort, secret, logger.With("component", "varnishadm"))

	// 2. backends watcher - watches services.json and EndpointSlices
	backendsWatcher := backends.NewWatcher(
		k8sClient,
		cfg.ServicesFilePath,
		cfg.BackendsFilePath,
		cfg.Namespace,
		logger.With("component", "backends"),
	)

	// 3. VCL reloader - watches main.vcl and hot-reloads via varnishadm
	vclReloader := vcl.New(
		vadm,
		cfg.VCLPath,
		vcl.DefaultKeepCount,
		logger.With("component", "vcl"),
	)

	// Start all components concurrently
	var wg sync.WaitGroup
	errCh := make(chan error, 4) // buffer for all components + health server

	// Start varnishadm server
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := vadm.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("varnishadm.Run: %w", err)
		}
	}()

	// Start backends watcher
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := backendsWatcher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("backendsWatcher.Run: %w", err)
		}
	}()

	// Start VCL reloader
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := vclReloader.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("vclReloader.Run: %w", err)
		}
	}()

	// Start health server
	healthServer := &http.Server{
		Addr:    cfg.HealthAddr,
		Handler: http.HandlerFunc(healthHandler),
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		slog.Info("health server starting", "addr", cfg.HealthAddr)
		if err := healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("healthServer.ListenAndServe: %w", err)
		}
	}()

	// Shutdown health server when context is cancelled
	go func() {
		<-ctx.Done()
		if err := healthServer.Close(); err != nil {
			slog.Error("health server close failed", "error", err)
		}
	}()

	slog.Info("sidecar started, waiting for Varnish to connect",
		"adminPort", adminPort,
	)

	// Wait for first error or context cancellation
	select {
	case err := <-errCh:
		cancel() // Signal other components to stop
		wg.Wait()
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		wg.Wait()
		return nil
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
