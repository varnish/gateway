package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/varnish/gateway/internal/ghost"
	"github.com/varnish/gateway/internal/varnishadm"
	"github.com/varnish/gateway/internal/vcl"
	"github.com/varnish/gateway/internal/vrun"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

//go:embed .version
var version string

// healthState tracks the health/readiness/draining state of the chaperone.
type healthState struct {
	mu       sync.RWMutex
	ready    bool
	draining bool
}

func (s *healthState) setReady() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = true
	slog.Info("health endpoint now returning healthy")
}

func (s *healthState) isReady() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready
}

func (s *healthState) setDraining() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draining = true
}

func (s *healthState) isDraining() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.draining
}

// Global health state for graceful shutdown
var state = &healthState{}

// Config holds the chaperone configuration from environment variables
type Config struct {
	// Varnish process management
	WorkDir    string // working directory for secrets, etc.
	VarnishDir string // varnish working directory (-n flag)
	AdminPort  int    // varnishadm port

	// Varnish runtime configuration
	VarnishHTTPAddr   string   // varnish HTTP address for ghost reload (e.g., "localhost:80")
	VarnishListen     []string // -a arguments for varnishd
	VarnishStorage    []string // -s arguments for varnishd
	VarnishdExtraArgs []string // additional command-line arguments for varnishd

	// Ghost configuration
	RoutingConfigPath string // path to routing.json from operator
	GhostConfigPath   string // path to write ghost.json

	// VCL configuration
	VCLPath string // path to watch for VCL changes

	// Kubernetes
	Namespace string // kubernetes namespace to watch

	// Health endpoint
	HealthAddr string // address for health endpoint
}

func loadConfig() (*Config, error) {
	adminPort, err := strconv.Atoi(getEnvOrDefault("VARNISH_ADMIN_PORT", "6082"))
	if err != nil {
		return nil, fmt.Errorf("invalid VARNISH_ADMIN_PORT: %w", err)
	}

	cfg := &Config{
		WorkDir:           getEnvOrDefault("WORK_DIR", "/var/run/varnish"),
		VarnishDir:        getEnvOrDefault("VARNISH_DIR", ""), // empty means use varnish default
		AdminPort:         adminPort,
		VarnishHTTPAddr:   getEnvOrDefault("VARNISH_HTTP_ADDR", "localhost:80"),
		VarnishListen:     parseList(getEnvOrDefault("VARNISH_LISTEN", ":80,http")),
		VarnishStorage:    parseList(getEnvOrDefault("VARNISH_STORAGE", "malloc,256m")),
		VarnishdExtraArgs: parseList(os.Getenv("VARNISHD_EXTRA_ARGS")), // no default, optional
		RoutingConfigPath: getEnvOrDefault("ROUTING_CONFIG_PATH", "/etc/varnish/routing.json"),
		GhostConfigPath:   getEnvOrDefault("GHOST_CONFIG_PATH", "/var/run/varnish/ghost.json"),
		VCLPath:           getEnvOrDefault("VCL_PATH", "/var/run/varnish/main.vcl"),
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

// parseList parses a comma-separated list, trimming whitespace
func parseList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	// For varnish args like ":80,http" we need to handle this specially
	// Actually, the format is space-separated for multiple -a args
	// Let's use semicolon as separator for multiple args
	parts = strings.Split(s, ";")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
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
		slog.Error("chaperone failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	slog.Info("chaperone starting", "version", strings.TrimSpace(version))

	// Load configuration from environment
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loadConfig: %w", err)
	}

	slog.Info("configuration loaded",
		"workDir", cfg.WorkDir,
		"varnishDir", cfg.VarnishDir,
		"adminPort", cfg.AdminPort,
		"varnishHTTPAddr", cfg.VarnishHTTPAddr,
		"routingConfigPath", cfg.RoutingConfigPath,
		"ghostConfigPath", cfg.GhostConfigPath,
		"vclPath", cfg.VCLPath,
		"namespace", cfg.Namespace,
	)

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

	// Drain wait period for graceful shutdown
	const drainWait = 10 * time.Second

	go func() {
		sig := <-sigCh
		slog.Info("received signal, initiating graceful shutdown", "signal", sig)

		// Set draining state - health endpoint will return 503
		state.setDraining()

		// Wait for drain period to allow load balancer to stop sending traffic
		// and existing requests to complete
		slog.Info("waiting for connections to drain", "duration", drainWait)
		time.Sleep(drainWait)

		slog.Info("drain complete, shutting down")
		cancel()
	}()

	// Create vrun manager to prepare workspace and start Varnish
	logger := slog.Default()
	varnishMgr := vrun.New(cfg.WorkDir, logger.With("component", "vrun"), cfg.VarnishDir)

	// Prepare workspace (creates secret file)
	if err := varnishMgr.PrepareWorkspace(); err != nil {
		return fmt.Errorf("varnishMgr.PrepareWorkspace: %w", err)
	}

	// Get the generated secret for varnishadm
	secret := varnishMgr.GetSecret()

	// Create components
	// 1. varnishadm server - listens for connections from Varnish
	vadm := varnishadm.New(uint16(cfg.AdminPort), secret, logger.With("component", "varnishadm"))

	// 2. ghost watcher - watches routing config and EndpointSlices
	ghostWatcher := ghost.NewWatcher(
		k8sClient,
		cfg.RoutingConfigPath,
		cfg.GhostConfigPath,
		cfg.VarnishHTTPAddr,
		cfg.Namespace,
		logger.With("component", "ghost"),
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
	errCh := make(chan error, 5) // buffer for all components

	// Build varnishd arguments
	varnishCfg := &vrun.Config{
		WorkDir:    cfg.WorkDir,
		AdminPort:  cfg.AdminPort,
		VarnishDir: cfg.VarnishDir,
		Listen:     cfg.VarnishListen,
		Storage:    cfg.VarnishStorage,
		ExtraArgs:  cfg.VarnishdExtraArgs,
	}
	varnishArgs, err := vrun.BuildArgs(varnishCfg)
	if err != nil {
		return fmt.Errorf("vrun.BuildArgs: %w", err)
	}

	// Start Varnish (manager process only, no VCL loaded yet)
	// We need readyCh before starting ghost watcher so it can wait for Varnish
	slog.Info("starting Varnish", "args", varnishArgs)
	readyCh, err := varnishMgr.Start(ctx, "", varnishArgs)
	if err != nil {
		return fmt.Errorf("varnishMgr.Start: %w", err)
	}

	// Wait for Varnish process to exit
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := varnishMgr.Wait(); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("varnishMgr.Wait: %w", err)
		}
	}()

	// Start varnishadm server
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := vadm.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("varnishadm.Run: %w", err)
		}
	}()

	// Start ghost watcher (with readyCh so it waits for Varnish before initial reload)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := ghostWatcher.Run(ctx, readyCh); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("ghostWatcher.Run: %w", err)
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

	// Start health server with drain endpoint for graceful shutdown
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/drain", drainHandler)

	healthServer := &http.Server{
		Addr:    cfg.HealthAddr,
		Handler: mux,
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

	// Startup sequence: wait for varnishadm connection, load VCL, start child
	wg.Add(1)
	go func() {
		defer wg.Done()

		// Step 1: Wait for varnishadm connection
		slog.Info("waiting for varnishadm connection")
		select {
		case <-vadm.Connected():
			slog.Info("varnishadm connected")
		case <-ctx.Done():
			return
		}

		// Step 2: Load initial VCL
		slog.Info("loading initial VCL", "path", cfg.VCLPath)
		if err := vclReloader.Reload(); err != nil {
			slog.Error("initial VCL load failed", "error", err)
			errCh <- fmt.Errorf("initial VCL load: %w", err)
			return
		}
		slog.Info("initial VCL loaded")

		// Step 3: Start the child process
		slog.Info("starting Varnish child process")
		if _, err := vadm.Start(); err != nil {
			slog.Error("failed to start Varnish child", "error", err)
			errCh <- fmt.Errorf("vadm.Start: %w", err)
			return
		}

		// Step 4: Wait for child to signal readiness
		select {
		case <-readyCh:
			// Varnish is ready, but we still need to wait for ghost to load backends
		case <-ctx.Done():
			return
		}

		// Step 5: Wait for ghost watcher to complete first backend reload
		select {
		case <-ghostWatcher.Ready():
			state.setReady()
		case <-ctx.Done():
			return
		}
	}()

	slog.Info("chaperone started",
		"adminPort", cfg.AdminPort,
		"varnishHTTPAddr", cfg.VarnishHTTPAddr,
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
	if !state.isReady() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready"))
		return
	}
	if state.isDraining() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("draining"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func drainHandler(w http.ResponseWriter, r *http.Request) {
	state.setDraining()
	slog.Info("drain requested via endpoint, health will now return 503")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("draining"))
}
