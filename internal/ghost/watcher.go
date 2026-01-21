package ghost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/varnish/gateway/internal/reload"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	// debounceDelay is the time to wait after a change before regenerating config
	debounceDelay = 100 * time.Millisecond

	// serviceLabelKey is the label used by Kubernetes to identify the service
	serviceLabelKey = "kubernetes.io/service-name"
)

// Watcher watches routing configuration and Kubernetes EndpointSlices,
// regenerating ghost.json when endpoints or routing rules change.
type Watcher struct {
	routingConfigPath string // path to routing.json from operator
	ghostConfigPath   string // path to write ghost.json
	varnishAddr       string // varnish HTTP address for reload trigger
	namespace         string
	client            kubernetes.Interface
	logger            *slog.Logger
	reloadClient      *reload.Client

	// Ready signaling
	readyCh   chan struct{}
	readyOnce sync.Once

	// Fatal error channel for reload failures
	fatalErrCh chan error

	// Internal state protected by mutex
	mu              sync.RWMutex
	initialSyncDone bool                      // true after initial EndpointSlice sync
	routingConfig   *RoutingConfig            // current routing rules (v1)
	routingConfigV2 *RoutingConfigV2          // v2 routing config with path-based routing
	endpoints       map[string][]Endpoint     // service key -> endpoints
	serviceWatch    map[string]struct{}       // services we care about (namespace/name)
}

// NewWatcher creates a new ghost configuration watcher.
func NewWatcher(
	client kubernetes.Interface,
	routingConfigPath string,
	ghostConfigPath string,
	varnishAddr string,
	namespace string,
	logger *slog.Logger,
) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Watcher{
		routingConfigPath: routingConfigPath,
		ghostConfigPath:   ghostConfigPath,
		varnishAddr:       varnishAddr,
		namespace:         namespace,
		client:            client,
		logger:            logger,
		reloadClient:      reload.NewClient(varnishAddr),
		readyCh:           make(chan struct{}),
		fatalErrCh:        make(chan error, 1), // buffered to avoid blocking
		endpoints:         make(map[string][]Endpoint),
		serviceWatch:      make(map[string]struct{}),
	}
}

// Ready returns a channel that closes when the watcher has completed its first
// successful configuration reload. This indicates backends are loaded and the
// gateway is ready to serve traffic.
func (w *Watcher) Ready() <-chan struct{} {
	return w.readyCh
}

// Run starts watching routing config and EndpointSlices.
// It blocks until the context is cancelled.
// The varnishReady channel should close when Varnish is ready to accept reload requests.
// If nil, the initial reload is attempted immediately (may fail if Varnish isn't ready).
func (w *Watcher) Run(ctx context.Context, varnishReady <-chan struct{}) error {
	// Load initial routing config
	if err := w.loadRoutingConfig(); err != nil {
		return fmt.Errorf("initial loadRoutingConfig: %w", err)
	}

	// Set up fsnotify for routing config
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify.NewWatcher: %w", err)
	}
	defer fsWatcher.Close()

	// Watch the directory containing routing config
	dir := filepath.Dir(w.routingConfigPath)
	if err := fsWatcher.Add(dir); err != nil {
		return fmt.Errorf("fsWatcher.Add(%s): %w", dir, err)
	}

	w.logger.Info("ghost watcher started",
		"routingConfigPath", w.routingConfigPath,
		"ghostConfigPath", w.ghostConfigPath,
		"varnishAddr", w.varnishAddr,
		"namespace", w.namespace,
	)

	// Wait for Varnish to be ready before setting up informers and triggering reload
	if varnishReady != nil {
		w.logger.Info("waiting for varnish to be ready")
		select {
		case <-varnishReady:
			w.logger.Debug("varnish is ready")
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Set up EndpointSlice informer (after Varnish is ready to avoid reload race)
	factory := informers.NewSharedInformerFactoryWithOptions(
		w.client,
		30*time.Second,
		informers.WithNamespace(w.namespace),
	)

	endpointSliceInformer := factory.Discovery().V1().EndpointSlices().Informer()
	_, err = endpointSliceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if slice, ok := obj.(*discoveryv1.EndpointSlice); ok {
				w.handleEndpointSliceUpdate(ctx, slice)
			}
		},
		UpdateFunc: func(_, newObj any) {
			if slice, ok := newObj.(*discoveryv1.EndpointSlice); ok {
				w.handleEndpointSliceUpdate(ctx, slice)
			}
		},
		DeleteFunc: func(obj any) {
			if slice, ok := obj.(*discoveryv1.EndpointSlice); ok {
				w.handleEndpointSliceDelete(ctx, slice)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("endpointSliceInformer.AddEventHandler: %w", err)
	}

	// Start the informer
	factory.Start(ctx.Done())

	// Wait for cache sync - all EndpointSlice events will be processed during this
	// but handlers won't trigger reloads until initialSyncDone is set
	if !cache.WaitForCacheSync(ctx.Done(), endpointSliceInformer.HasSynced) {
		return fmt.Errorf("failed to sync EndpointSlice cache")
	}

	// Mark initial sync complete - subsequent endpoint changes will trigger reloads
	w.mu.Lock()
	w.initialSyncDone = true
	w.mu.Unlock()

	// Generate ghost.json with all backends and trigger single reload
	if err := w.regenerateConfig(ctx); err != nil {
		// Initial reload failure is fatal - we can't serve traffic without backends
		return fmt.Errorf("initial ghost reload: %w", err)
	}

	// Signal ready after successful reload with all backends
	w.readyOnce.Do(func() {
		close(w.readyCh)
	})

	var debounceTimer *time.Timer
	filename := filepath.Base(w.routingConfigPath)

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("ghost watcher stopping")
			return ctx.Err()

		case err := <-w.fatalErrCh:
			// Fatal reload error - routing is out of sync
			w.logger.Error("fatal ghost reload error, exiting", "error", err)
			return fmt.Errorf("fatal ghost reload: %w", err)

		case event, ok := <-fsWatcher.Events:
			if !ok {
				return nil
			}

			// Only react to changes to our specific file
			if filepath.Base(event.Name) != filename {
				continue
			}

			// React to Write, Create, and Rename events
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}

			w.logger.Debug("routing config changed", "event", event.Op.String())

			// Debounce rapid changes
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(debounceDelay, func() {
				if err := w.loadRoutingConfig(); err != nil {
					w.logger.Error("failed to reload routing config", "error", err)
					select {
					case w.fatalErrCh <- fmt.Errorf("loadRoutingConfig: %w", err):
					default:
					}
					return
				}
				if err := w.regenerateConfig(ctx); err != nil {
					w.logger.Error("failed to regenerate ghost config", "error", err)
					select {
					case w.fatalErrCh <- err:
					default:
					}
				}
			})

		case err, ok := <-fsWatcher.Errors:
			if !ok {
				return nil
			}
			w.logger.Error("fsnotify error", "error", err)
		}
	}
}

// loadRoutingConfig reads and parses the routing configuration.
// Supports both v1 (hostname-only) and v2 (path-based) formats.
func (w *Watcher) loadRoutingConfig() error {
	data, err := os.ReadFile(w.routingConfigPath)
	if err != nil {
		return fmt.Errorf("os.ReadFile(%s): %w", w.routingConfigPath, err)
	}

	// Detect version
	var versionCheck struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &versionCheck); err != nil {
		return fmt.Errorf("json.Unmarshal version: %w", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	switch versionCheck.Version {
	case 2:
		config, err := ParseRoutingConfigV2(data)
		if err != nil {
			return err
		}
		w.routingConfigV2 = config
		w.routingConfig = nil
		w.updateServiceWatchV2(config)

	case 1:
		config, err := ParseRoutingConfig(data)
		if err != nil {
			return err
		}
		w.routingConfig = config
		w.routingConfigV2 = nil
		w.updateServiceWatchV1(config)

	default:
		return fmt.Errorf("unsupported routing config version: %d", versionCheck.Version)
	}

	w.logger.Info("routing config loaded",
		"version", versionCheck.Version,
		"vhosts", w.getVHostCount(),
	)

	return nil
}

// updateServiceWatchV1 updates the service watch map for v1 routing config.
func (w *Watcher) updateServiceWatchV1(config *RoutingConfig) {
	w.serviceWatch = make(map[string]struct{})
	for _, rule := range config.VHosts {
		key := ServiceKey(rule.Namespace, rule.Service)
		w.serviceWatch[key] = struct{}{}
	}
	if config.Default != nil {
		key := ServiceKey(config.Default.Namespace, config.Default.Service)
		w.serviceWatch[key] = struct{}{}
	}
}

// updateServiceWatchV2 updates the service watch map for v2 routing config.
func (w *Watcher) updateServiceWatchV2(config *RoutingConfigV2) {
	w.serviceWatch = make(map[string]struct{})
	for _, vhost := range config.VHosts {
		for _, route := range vhost.Routes {
			key := ServiceKey(route.Namespace, route.Service)
			w.serviceWatch[key] = struct{}{}
		}
	}
	if config.Default != nil {
		key := ServiceKey(config.Default.Namespace, config.Default.Service)
		w.serviceWatch[key] = struct{}{}
	}
}

// getVHostCount returns the number of vhosts in the current routing config.
func (w *Watcher) getVHostCount() int {
	if w.routingConfigV2 != nil {
		return len(w.routingConfigV2.VHosts)
	}
	if w.routingConfig != nil {
		return len(w.routingConfig.VHosts)
	}
	return 0
}

// handleEndpointSliceUpdate processes an EndpointSlice add or update event.
func (w *Watcher) handleEndpointSliceUpdate(ctx context.Context, slice *discoveryv1.EndpointSlice) {
	serviceName := slice.Labels[serviceLabelKey]
	if serviceName == "" {
		return
	}

	key := ServiceKey(slice.Namespace, serviceName)

	w.mu.Lock()

	// Check if this service is in our watch list
	if _, ok := w.serviceWatch[key]; !ok {
		w.mu.Unlock()
		return
	}

	// Extract ready endpoints
	newEndpoints := extractEndpoints(slice)
	oldEndpoints := w.endpoints[key]

	// Check if endpoints actually changed
	added, removed := diffEndpoints(oldEndpoints, newEndpoints)
	if len(added) == 0 && len(removed) == 0 {
		w.mu.Unlock()
		return
	}

	w.endpoints[key] = newEndpoints
	shouldReload := w.initialSyncDone
	w.mu.Unlock()

	// Log the specific changes
	w.logger.Info("endpoints changed",
		"service", key,
		"added", len(added),
		"removed", len(removed),
		"total", len(newEndpoints),
	)
	for _, ep := range added {
		w.logger.Info("backend added", "service", key, "address", ep.IP, "port", ep.Port)
	}
	for _, ep := range removed {
		w.logger.Info("backend removed", "service", key, "address", ep.IP, "port", ep.Port)
	}

	// Skip reload during initial sync - we'll do one reload after sync completes
	if !shouldReload {
		return
	}

	// Regenerate ghost.json
	if err := w.regenerateConfig(ctx); err != nil {
		w.logger.Error("failed to regenerate ghost config", "error", err)
		select {
		case w.fatalErrCh <- err:
		default:
		}
	}
}

// handleEndpointSliceDelete processes an EndpointSlice delete event.
func (w *Watcher) handleEndpointSliceDelete(ctx context.Context, slice *discoveryv1.EndpointSlice) {
	serviceName := slice.Labels[serviceLabelKey]
	if serviceName == "" {
		return
	}

	key := ServiceKey(slice.Namespace, serviceName)

	w.mu.Lock()

	// Check if this service is in our watch list
	if _, ok := w.serviceWatch[key]; !ok {
		w.mu.Unlock()
		return
	}

	// Check if we actually had endpoints for this service
	oldEndpoints, existed := w.endpoints[key]
	if !existed || len(oldEndpoints) == 0 {
		w.mu.Unlock()
		return
	}

	// Remove endpoints for this service
	delete(w.endpoints, key)
	shouldReload := w.initialSyncDone

	w.mu.Unlock()

	// Log the specific changes
	w.logger.Info("endpoints deleted",
		"service", key,
		"removed", len(oldEndpoints),
	)
	for _, ep := range oldEndpoints {
		w.logger.Info("backend removed", "service", key, "address", ep.IP, "port", ep.Port)
	}

	// Skip reload during initial sync - we'll do one reload after sync completes
	if !shouldReload {
		return
	}

	// Regenerate ghost.json
	if err := w.regenerateConfig(ctx); err != nil {
		w.logger.Error("failed to regenerate ghost config", "error", err)
		select {
		case w.fatalErrCh <- err:
		default:
		}
	}
}

// regenerateConfig generates and writes ghost.json, then triggers a reload.
func (w *Watcher) regenerateConfig(ctx context.Context) error {
	w.mu.RLock()
	if w.routingConfig == nil && w.routingConfigV2 == nil {
		w.mu.RUnlock()
		return fmt.Errorf("no routing config loaded")
	}

	// Copy endpoints map
	endpoints := make(ServiceEndpoints, len(w.endpoints))
	maps.Copy(endpoints, w.endpoints)

	routingConfigV1 := w.routingConfig
	routingConfigV2 := w.routingConfigV2
	w.mu.RUnlock()

	// Generate ghost config based on version
	if routingConfigV2 != nil {
		// V2 path
		config := GenerateV2(routingConfigV2, endpoints)
		if err := WriteConfigV2(w.ghostConfigPath, config); err != nil {
			return fmt.Errorf("WriteConfigV2: %w", err)
		}
		w.logger.Info("ghost.json v2 regenerated",
			"vhosts", len(config.VHosts),
			"path", w.ghostConfigPath,
		)
	} else {
		// V1 path (backward compatibility)
		config := Generate(routingConfigV1, endpoints)
		if err := WriteConfig(w.ghostConfigPath, config); err != nil {
			return fmt.Errorf("WriteConfig: %w", err)
		}
		w.logger.Info("ghost.json v1 regenerated",
			"vhosts", len(config.VHosts),
			"path", w.ghostConfigPath,
		)
	}

	// Trigger ghost reload
	if err := w.reloadClient.TriggerReload(ctx); err != nil {
		return fmt.Errorf("ghost reload failed: %w", err)
	}

	w.logger.Info("ghost reload triggered successfully")
	return nil
}

// diffEndpoints compares old and new endpoint slices, returning added and removed endpoints.
func diffEndpoints(oldEndpoints, newEndpoints []Endpoint) (added, removed []Endpoint) {
	oldSet := make(map[string]Endpoint)
	for _, ep := range oldEndpoints {
		oldSet[ep.String()] = ep
	}

	newSet := make(map[string]Endpoint)
	for _, ep := range newEndpoints {
		newSet[ep.String()] = ep
	}

	// Find added endpoints (in new but not in old)
	for key, ep := range newSet {
		if _, exists := oldSet[key]; !exists {
			added = append(added, ep)
		}
	}

	// Find removed endpoints (in old but not in new)
	for key, ep := range oldSet {
		if _, exists := newSet[key]; !exists {
			removed = append(removed, ep)
		}
	}

	return added, removed
}

// extractEndpoints extracts ready endpoints from an EndpointSlice.
func extractEndpoints(slice *discoveryv1.EndpointSlice) []Endpoint {
	var endpoints []Endpoint

	for _, ep := range slice.Endpoints {
		// Skip endpoints that are not ready
		if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
			continue
		}

		// Get port from slice ports if available
		port := 0
		if len(slice.Ports) > 0 && slice.Ports[0].Port != nil {
			port = int(*slice.Ports[0].Port)
		}

		// Add an endpoint for each address
		for _, addr := range ep.Addresses {
			endpoints = append(endpoints, Endpoint{
				IP:   addr,
				Port: port,
			})
		}
	}

	return endpoints
}

// ListEndpointSlices lists all EndpointSlices for a service (for testing/debugging).
func (w *Watcher) ListEndpointSlices(ctx context.Context, namespace, serviceName string) (*discoveryv1.EndpointSliceList, error) {
	selector := labels.Set{serviceLabelKey: serviceName}.AsSelector()
	return w.client.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
}
