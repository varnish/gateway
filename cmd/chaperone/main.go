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
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/varnish/gateway/internal/dashboard"
	"github.com/varnish/gateway/internal/ghost"
	"github.com/varnish/gateway/internal/invalidation"
	"github.com/varnish/gateway/internal/k8sutil"
	vtls "github.com/varnish/gateway/internal/tls"
	"github.com/varnish/gateway/internal/varnishadm"
	"github.com/varnish/gateway/internal/vcl"
	"github.com/varnish/gateway/internal/vrun"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

//go:embed .version
var version string

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
	GhostConfigPath string // path to write ghost.json

	// VCL configuration
	VCLPath string // path to watch for VCL changes

	// Kubernetes
	Namespace     string // kubernetes namespace to watch
	ConfigMapName string // name of ConfigMap containing routing.json and main.vcl

	// TLS configuration
	TLSCertDir string   // path to TLS cert directory (empty = no TLS)
	TLSListen  []string // -a arguments for HTTPS (e.g., "https=:8443,https")

	// Health endpoint
	HealthAddr string // address for health endpoint

	// Dashboard (optional, empty = disabled)
	DashboardAddr string // address for dashboard UI

	// Cache invalidation
	GatewayName string // name of the gateway (empty = skip invalidation watcher)
	PodName     string // this pod's name (from downward API)
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
		VarnishHTTPAddr:   getEnvOrDefault("VARNISH_HTTP_ADDR", "localhost:1969"),
		VarnishListen:     parseList(getEnvOrDefault("VARNISH_LISTEN", "http=:80")),
		VarnishStorage:    parseList(getEnvOrDefault("VARNISH_STORAGE", "malloc,256m")),
		VarnishdExtraArgs: parseList(os.Getenv("VARNISHD_EXTRA_ARGS")), // no default, optional
		GhostConfigPath:   getEnvOrDefault("GHOST_CONFIG_PATH", "/var/run/varnish/ghost.json"),
		VCLPath:           getEnvOrDefault("VCL_PATH", "/var/run/varnish/main.vcl"),
		Namespace:         getEnvOrDefault("NAMESPACE", "default"),
		ConfigMapName:     getEnvOrDefault("CONFIGMAP_NAME", "gateway-vcl"),
		TLSCertDir:        os.Getenv("TLS_CERT_DIR"),                  // empty by default
		TLSListen:         parseList(os.Getenv("VARNISH_TLS_LISTEN")), // empty by default
		HealthAddr:        getEnvOrDefault("HEALTH_ADDR", ":8080"),
		DashboardAddr:     os.Getenv("DASHBOARD_ADDR"), // empty = disabled
		GatewayName:       os.Getenv("GATEWAY_NAME"),
		PodName:           os.Getenv("POD_NAME"),
	}

	return cfg, nil
}

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// parseList parses a semicolon-separated list, trimming whitespace.
func parseList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ";")
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

	// Redirect klog (used by Kubernetes client libraries) to slog
	klog.SetLogger(logr.FromSlogHandler(handler))
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

	slog.Debug("configuration loaded",
		"workDir", cfg.WorkDir,
		"varnishDir", cfg.VarnishDir,
		"adminPort", cfg.AdminPort,
		"varnishHTTPAddr", cfg.VarnishHTTPAddr,
		"ghostConfigPath", cfg.GhostConfigPath,
		"vclPath", cfg.VCLPath,
		"namespace", cfg.Namespace,
		"configMapName", cfg.ConfigMapName,
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

	k8sutil.WrapTransportForSlowRequests(k8sConfig, slog.Default())

	k8sClient, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return fmt.Errorf("kubernetes.NewForConfig: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(k8sConfig)
	if err != nil {
		return fmt.Errorf("dynamic.NewForConfig: %w", err)
	}

	// Dashboard event bus (always created, used for state tracking even without UI)
	dashBus := dashboard.NewEventBus(256)
	dashState := dashboard.NewStateTracker(dashBus, strings.TrimSpace(version))

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
		dashState.SetDraining()

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
		cfg.GhostConfigPath,
		cfg.VarnishHTTPAddr,
		cfg.Namespace,
		cfg.ConfigMapName,
		logger.With("component", "ghost"),
	)
	ghostWatcher.SetDashboard(dashBus, dashState)

	// 3. VCL reloader - watches main.vcl and hot-reloads via varnishadm
	vclReloader := vcl.New(
		vadm,
		cfg.VCLPath,
		vcl.DefaultKeepCount,
		k8sClient,
		cfg.ConfigMapName,
		cfg.Namespace,
		logger.With("component", "vcl"),
	)
	vclReloader.SetDashboard(dashBus)

	// 4. TLS reloader - watches TLS cert directory and hot-reloads certs (if TLS enabled)
	var tlsReloader *vtls.Reloader
	if cfg.TLSCertDir != "" {
		tlsReloader = vtls.New(vadm, cfg.TLSCertDir, logger.With("component", "tls"))
		tlsReloader.SetDashboard(dashBus)
	}

	// 5. Cache invalidation watcher (if gateway name is configured)
	var invWatcher *invalidation.Watcher
	if cfg.GatewayName != "" && cfg.PodName != "" {
		invWatcher = invalidation.NewWatcher(
			dynClient,
			k8sClient,
			cfg.VarnishHTTPAddr,
			cfg.GatewayName,
			cfg.Namespace,
			cfg.PodName,
			logger.With("component", "invalidation"),
		)
	}

	// Build varnishd arguments
	listenAddrs := cfg.VarnishListen
	if len(cfg.TLSListen) > 0 {
		listenAddrs = append(listenAddrs, cfg.TLSListen...)
	}
	varnishCfg := &vrun.Config{
		WorkDir:    cfg.WorkDir,
		AdminPort:  cfg.AdminPort,
		VarnishDir: cfg.VarnishDir,
		Listen:     listenAddrs,
		Storage:    cfg.VarnishStorage,
		ExtraArgs:  cfg.VarnishdExtraArgs,
	}
	varnishArgs, err := vrun.BuildArgs(varnishCfg)
	if err != nil {
		return fmt.Errorf("vrun.BuildArgs: %w", err)
	}

	// Start Varnish (manager process only, no VCL loaded yet)
	// We need readyCh before starting ghost watcher so it can wait for Varnish
	slog.Debug("starting Varnish", "args", varnishArgs)
	readyCh, err := varnishMgr.Start(ctx, "", varnishArgs)
	if err != nil {
		return fmt.Errorf("varnishMgr.Start: %w", err)
	}

	// Start health server with drain endpoint for graceful shutdown
	mux := http.NewServeMux()
	mux.HandleFunc("/health", makeHealthHandler(dashState))
	mux.HandleFunc("/drain", makeDrainHandler(dashState))
	mux.HandleFunc("/debug/backends", makeBackendsHandler(vadm))

	healthServer := &http.Server{
		Addr:    cfg.HealthAddr,
		Handler: mux,
	}

	// Start all components concurrently using errgroup.
	// If any goroutine returns a non-nil error, gctx is cancelled,
	// signalling all other goroutines to shut down.
	g, gctx := errgroup.WithContext(ctx)

	// Wait for Varnish process to exit
	g.Go(func() error {
		if err := varnishMgr.Wait(); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("varnishMgr.Wait: %w", err)
		}
		return nil
	})

	// Start varnishadm server
	g.Go(func() error {
		if err := vadm.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("varnishadm.Run: %w", err)
		}
		return nil
	})

	// Start ghost watcher (with readyCh so it waits for Varnish before initial reload)
	g.Go(func() error {
		if err := ghostWatcher.Run(gctx, readyCh); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("ghostWatcher.Run: %w", err)
		}
		return nil
	})

	// Start VCL reloader
	g.Go(func() error {
		if err := vclReloader.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("vclReloader.Run: %w", err)
		}
		return nil
	})

	// Start TLS reloader (if TLS enabled)
	if tlsReloader != nil {
		g.Go(func() error {
			if err := tlsReloader.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("tlsReloader.Run: %w", err)
			}
			return nil
		})

		// Listen for fatal TLS reload errors
		g.Go(func() error {
			select {
			case err := <-tlsReloader.FatalError():
				slog.Error("fatal TLS reload error - exiting", "error", err)
				return err
			case <-gctx.Done():
				return nil
			}
		})
	}

	// Start invalidation watcher (if configured)
	if invWatcher != nil {
		g.Go(func() error {
			if err := invWatcher.Run(gctx, readyCh); err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("invWatcher.Run: %w", err)
			}
			return nil
		})
	}

	// Health server
	g.Go(func() error {
		slog.Debug("health server starting", "addr", cfg.HealthAddr)
		if err := healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("healthServer.ListenAndServe: %w", err)
		}
		return nil
	})

	// Shutdown health server when context is cancelled
	g.Go(func() error {
		<-gctx.Done()
		if err := healthServer.Close(); err != nil {
			slog.Error("health server close failed", "error", err)
		}
		return nil
	})

	// Startup sequence: wait for varnishadm connection, load VCL, start child
	g.Go(func() error {
		// Step 1: Wait for varnishadm connection
		slog.Debug("waiting for varnishadm connection")
		select {
		case <-vadm.Connected():
			slog.Debug("varnishadm connected")
			dashboard.PublishVarnishConnected(dashBus)
		case <-gctx.Done():
			return nil
		}

		// Step 2: Load initial VCL
		slog.Debug("loading initial VCL", "path", cfg.VCLPath)
		if err := vclReloader.Reload(); err != nil {
			return fmt.Errorf("initial VCL load: %w", err)
		}
		slog.Info("initial VCL loaded")

		// Step 3: Start the child process
		slog.Debug("starting Varnish child process")
		if _, err := vadm.Start(); err != nil {
			return fmt.Errorf("vadm.Start: %w", err)
		}

		// Step 4: Wait for child to signal readiness
		select {
		case <-readyCh:
			// Varnish child is running
		case <-gctx.Done():
			return nil
		}

		// Step 4.5: Load TLS certificates (if TLS enabled)
		// Must happen after child starts — tls.cert.load is a child-level command
		if tlsReloader != nil {
			slog.Debug("loading TLS certificates", "dir", cfg.TLSCertDir)
			if err := tlsReloader.LoadAll(); err != nil {
				return fmt.Errorf("initial TLS cert load: %w", err)
			}
			slog.Info("TLS certificates loaded")
		}

		// Step 5: Wait for ghost watcher to complete first backend reload
		select {
		case <-ghostWatcher.Ready():
			dashState.SetReady()
		case <-gctx.Done():
		}
		return nil
	})

	// Start dashboard server (if configured)
	if cfg.DashboardAddr != "" {
		dashServer := dashboard.NewServer(
			cfg.DashboardAddr, dashState, dashBus,
			logger.With("component", "dashboard"),
		)
		g.Go(func() error {
			if err := dashServer.Run(gctx); err != nil {
				return fmt.Errorf("dashServer.Run: %w", err)
			}
			return nil
		})
	}

	slog.Info("all components started, waiting for shutdown")
	if err := g.Wait(); err != nil {
		return err
	}
	slog.Info("shutting down")
	return nil
}

func makeHealthHandler(state *dashboard.StateTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !state.IsReady() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
			return
		}
		if state.IsDraining() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("draining"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

func makeDrainHandler(state *dashboard.StateTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state.SetDraining()
		slog.Info("drain requested via endpoint, health will now return 503")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("draining"))
	}
}

// makeBackendsHandler creates a handler that exposes varnishadm backend.list output
// Supports query parameters:
//   - format=json: Return JSON output (backend.list -j)
//   - detailed=true: Return detailed output (backend.list -p)
func makeBackendsHandler(vadm *varnishadm.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse query parameters
		format := r.URL.Query().Get("format")
		detailed := r.URL.Query().Get("detailed") == "true"

		// Determine flags
		var jsonMode bool
		if format == "json" {
			jsonMode = true
			detailed = false // JSON mode takes precedence
		}

		// Execute backend.list command
		resp, err := vadm.BackendList(detailed, jsonMode)
		if err != nil {
			slog.Error("backend.list failed", "error", err)
			http.Error(w, fmt.Sprintf("backend.list error: %v", err), http.StatusInternalServerError)
			return
		}

		// Check response status
		if resp.StatusCode() != 200 {
			slog.Warn("backend.list returned non-OK status", "status", resp.StatusCode())
			http.Error(w, fmt.Sprintf("backend.list status %d: %s", resp.StatusCode(), resp.Payload()), http.StatusInternalServerError)
			return
		}

		// Set content type based on format
		if jsonMode {
			w.Header().Set("Content-Type", "application/json")
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(resp.Payload()))
	}
}

