package ghost

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/varnish/gateway/internal/reload"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	discoveryv1listers "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	// serviceLabelKey is the label used by Kubernetes to identify the service
	serviceLabelKey = "kubernetes.io/service-name"
)

// Watcher watches routing configuration and Kubernetes EndpointSlices,
// regenerating ghost.json when endpoints or routing rules change.
type Watcher struct {
	ghostConfigPath string // path to write ghost.json
	varnishAddr     string // varnish HTTP address for reload trigger
	namespace       string
	client          kubernetes.Interface
	logger          *slog.Logger
	reloadClient    *reload.Client

	// Ready signaling
	readyCh   chan struct{}
	readyOnce sync.Once

	// Fatal error channel for reload failures
	fatalErrCh chan error

	// Internal state protected by mutex
	mu              sync.RWMutex
	initialSyncDone bool                  // true after initial EndpointSlice sync
	routingConfig   *RoutingConfig        // current routing rules
	endpoints       map[string][]Endpoint // service key -> endpoints
	serviceWatch    map[string]struct{}   // services we care about (namespace/name)

	// ConfigMap watching
	configMapName      string       // name of ConfigMap to watch
	lastConfigMapRV    string       // last seen ResourceVersion for deduplication
	lastRoutingJSON    string       // last seen routing.json content for content-based deduplication
	lastRoutingJSONMux sync.RWMutex // protect lastRoutingJSON

	// EndpointSlice lister for backfilling endpoints on new service watches
	endpointSliceLister discoveryv1listers.EndpointSliceLister
}

// NewWatcher creates a new ghost configuration watcher.
func NewWatcher(
	client kubernetes.Interface,
	ghostConfigPath string,
	varnishAddr string,
	namespace string,
	configMapName string,
	logger *slog.Logger,
) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Watcher{
		ghostConfigPath: ghostConfigPath,
		varnishAddr:     varnishAddr,
		namespace:       namespace,
		configMapName:   configMapName,
		client:          client,
		logger:          logger,
		reloadClient:    reload.NewClient(varnishAddr),
		readyCh:         make(chan struct{}),
		fatalErrCh:      make(chan error, 1), // buffered to avoid blocking
		endpoints:       make(map[string][]Endpoint),
		serviceWatch:    make(map[string]struct{}),
	}
}

// notifyFatal sends err on the fatal error channel without blocking.
func (w *Watcher) notifyFatal(err error) {
	select {
	case w.fatalErrCh <- err:
	default:
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
	w.logger.Debug("ghost watcher started",
		"configMapName", w.configMapName,
		"ghostConfigPath", w.ghostConfigPath,
		"varnishAddr", w.varnishAddr,
		"namespace", w.namespace,
	)

	// Wait for Varnish to be ready before setting up informers and triggering reload
	if varnishReady != nil {
		w.logger.Debug("waiting for varnish to be ready")
		select {
		case <-varnishReady:
			w.logger.Debug("varnish is ready")
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Load initial routing config BEFORE starting informers
	// This populates serviceWatch so EndpointSlice events won't be ignored
	cm, err := w.client.CoreV1().ConfigMaps(w.namespace).Get(ctx, w.configMapName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get initial ConfigMap %s: %w", w.configMapName, err)
	}
	w.handleConfigMapUpdate(ctx, cm)

	w.logger.Debug("routing config loaded, starting informers",
		"vhosts", w.getVHostCount(),
		"watchedServices", len(w.serviceWatch),
	)

	// Set up EndpointSlice informer - cluster-wide to watch all namespaces
	// Routes can reference services in any namespace, not just the Gateway's namespace
	endpointSliceFactory := informers.NewSharedInformerFactory(w.client, 30*time.Second)

	// Set up ConfigMap informer - namespace-scoped to Gateway's namespace only
	configMapFactory := informers.NewSharedInformerFactoryWithOptions(
		w.client,
		30*time.Second,
		informers.WithNamespace(w.namespace),
	)

	endpointSliceInformer := endpointSliceFactory.Discovery().V1().EndpointSlices().Informer()
	w.endpointSliceLister = endpointSliceFactory.Discovery().V1().EndpointSlices().Lister()
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

	// Set up ConfigMap informer using namespace-scoped factory
	configMapInformer := configMapFactory.Core().V1().ConfigMaps().Informer()
	_, err = configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if cm, ok := obj.(*corev1.ConfigMap); ok {
				w.handleConfigMapUpdate(ctx, cm)
			}
		},
		UpdateFunc: func(_, newObj any) {
			if cm, ok := newObj.(*corev1.ConfigMap); ok {
				w.handleConfigMapUpdate(ctx, cm)
			}
		},
		DeleteFunc: func(obj any) {
			if cm, ok := obj.(*corev1.ConfigMap); ok {
				w.handleConfigMapDelete(ctx, cm)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("configMapInformer.AddEventHandler: %w", err)
	}

	// Start both informer factories
	endpointSliceFactory.Start(ctx.Done())
	configMapFactory.Start(ctx.Done())

	// Wait for both informers to sync
	if !cache.WaitForCacheSync(ctx.Done(),
		endpointSliceInformer.HasSynced,
		configMapInformer.HasSynced) {
		return fmt.Errorf("failed to sync caches")
	}

	// Mark initial sync complete - subsequent endpoint changes will trigger reloads
	w.mu.Lock()
	w.initialSyncDone = true

	// Log initial endpoints summary
	totalServices := len(w.endpoints)
	totalBackends := 0
	for _, eps := range w.endpoints {
		totalBackends += len(eps)
	}
	w.mu.Unlock()

	w.logger.Info("initial endpoints discovered",
		"services", totalServices,
		"backends", totalBackends,
	)

	// Generate ghost.json with all backends and trigger single reload
	if err := w.regenerateConfig(ctx); err != nil {
		// Initial reload failure is fatal - we can't serve traffic without backends
		return fmt.Errorf("initial ghost reload: %w", err)
	}

	// Signal ready after successful reload with all backends
	w.readyOnce.Do(func() {
		close(w.readyCh)
	})

	// Wait for context cancellation or fatal error
	// ConfigMap and EndpointSlice updates are handled by informer callbacks
	select {
	case <-ctx.Done():
		w.logger.Info("ghost watcher stopping")
		return ctx.Err()
	case err := <-w.fatalErrCh:
		// Fatal reload error - routing is out of sync
		w.logger.Error("fatal ghost reload error, exiting", "error", err)
		return fmt.Errorf("fatal ghost reload: %w", err)
	}
}

// updateServiceWatch updates the service watch map from the routing config.
func (w *Watcher) updateServiceWatch(config *RoutingConfig) {
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
	// During initial sync, just log at DEBUG; after sync, log at INFO
	logLevel := slog.LevelDebug
	if shouldReload {
		logLevel = slog.LevelInfo
	}

	w.logger.Log(context.Background(), logLevel, "endpoints changed",
		"service", key,
		"added", len(added),
		"removed", len(removed),
		"total", len(newEndpoints),
	)
	for _, ep := range added {
		w.logger.Debug("backend added", "service", key, "address", ep.IP, "port", ep.Port)
	}
	for _, ep := range removed {
		w.logger.Debug("backend removed", "service", key, "address", ep.IP, "port", ep.Port)
	}

	// Skip reload during initial sync - we'll do one reload after sync completes
	if !shouldReload {
		return
	}

	// Regenerate ghost.json with retry
	w.regenerateConfigWithRetry(ctx)
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
	// During initial sync, just log at DEBUG; after sync, log at INFO
	logLevel := slog.LevelDebug
	if shouldReload {
		logLevel = slog.LevelInfo
	}

	w.logger.Log(context.Background(), logLevel, "endpoints deleted",
		"service", key,
		"removed", len(oldEndpoints),
	)
	for _, ep := range oldEndpoints {
		w.logger.Debug("backend removed", "service", key, "address", ep.IP, "port", ep.Port)
	}

	// Skip reload during initial sync - we'll do one reload after sync completes
	if !shouldReload {
		return
	}

	// Regenerate ghost.json with retry
	w.regenerateConfigWithRetry(ctx)
}

// regenerateConfigWithRetry retries regenerateConfig up to 3 times with exponential
// backoff (500ms, 1s, 2s) before sending a fatal error. This prevents transient reload
// failures (e.g., ghost temporarily unavailable) from crashing the watcher.
func (w *Watcher) regenerateConfigWithRetry(ctx context.Context) {
	backoffs := []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second}
	var lastErr error
	for attempt := 0; attempt <= len(backoffs); attempt++ {
		if err := w.regenerateConfig(ctx); err != nil {
			lastErr = err
			if attempt < len(backoffs) {
				w.logger.Warn("ghost reload failed, retrying",
					"error", err,
					"attempt", attempt+1,
					"backoff", backoffs[attempt],
				)
				select {
				case <-time.After(backoffs[attempt]):
				case <-ctx.Done():
					w.notifyFatal(err)
					return
				}
				continue
			}
			w.logger.Error("ghost reload failed after all retries", "error", err)
			w.notifyFatal(err)
			return
		}
		return // success
	}
	// Should not reach here, but just in case
	w.notifyFatal(lastErr)
}

// regenerateConfig generates and writes ghost.json, then triggers a reload.
func (w *Watcher) regenerateConfig(ctx context.Context) error {
	w.mu.RLock()
	if w.routingConfig == nil {
		w.mu.RUnlock()
		return fmt.Errorf("no routing config loaded")
	}

	// Copy endpoints map
	endpoints := make(ServiceEndpoints, len(w.endpoints))
	maps.Copy(endpoints, w.endpoints)

	// Count services and backends
	serviceCount := len(endpoints)
	backendCount := 0
	for _, eps := range endpoints {
		backendCount += len(eps)
	}

	routingConfig := w.routingConfig
	w.mu.RUnlock()

	config := Generate(routingConfig, endpoints)
	if err := WriteConfig(w.ghostConfigPath, config); err != nil {
		return fmt.Errorf("WriteConfig: %w", err)
	}
	w.logger.Info("ghost.json regenerated",
		"vhosts", len(config.VHosts),
		"services", serviceCount,
		"backends", backendCount,
		"path", w.ghostConfigPath,
	)

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
// An endpoint entry is created for each (address, port) combination so that
// multi-port Services are represented correctly. The generator filters by
// the route's target port when building backends.
func extractEndpoints(slice *discoveryv1.EndpointSlice) []Endpoint {
	var endpoints []Endpoint

	// Collect all ports from the slice. If no ports are defined, use port 0
	// (the generator will fall back to the route's port).
	ports := []int{0}
	if len(slice.Ports) > 0 {
		ports = make([]int, 0, len(slice.Ports))
		for _, p := range slice.Ports {
			if p.Port != nil {
				ports = append(ports, int(*p.Port))
			}
		}
		if len(ports) == 0 {
			ports = []int{0}
		}
	}

	for _, ep := range slice.Endpoints {
		// Skip endpoints that are not ready
		if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
			continue
		}

		// Add an endpoint for each (address, port) combination
		for _, addr := range ep.Addresses {
			for _, port := range ports {
				endpoints = append(endpoints, Endpoint{
					IP:   addr,
					Port: port,
				})
			}
		}
	}

	return endpoints
}

// handleConfigMapUpdate processes ConfigMap add/update events.
func (w *Watcher) handleConfigMapUpdate(ctx context.Context, cm *corev1.ConfigMap) {
	// Filter: only our ConfigMap
	if cm.Name != w.configMapName {
		return
	}

	w.mu.Lock()

	// Deduplicate via ResourceVersion (but allow first update even if empty)
	if cm.ResourceVersion != "" && cm.ResourceVersion == w.lastConfigMapRV {
		w.mu.Unlock()
		w.logger.Debug("skipping duplicate ConfigMap update", "resourceVersion", cm.ResourceVersion)
		return
	}

	w.logger.Info("ConfigMap updated",
		"name", cm.Name,
		"resourceVersion", cm.ResourceVersion,
	)

	w.lastConfigMapRV = cm.ResourceVersion
	shouldReload := w.initialSyncDone
	w.mu.Unlock()

	// Extract and validate routing config
	data, err := ExtractRoutingConfig(cm)
	if err != nil {
		w.logger.Error("failed to extract routing config", "error", err)
		return
	}

	if err := ValidateRoutingConfig(data); err != nil {
		w.logger.Error("invalid routing config", "error", err)
		return
	}

	// Check if routing.json content actually changed
	routingJSONStr := string(data)
	w.lastRoutingJSONMux.Lock()
	if w.lastRoutingJSON == routingJSONStr {
		w.lastRoutingJSONMux.Unlock()
		w.logger.Debug("ConfigMap updated but routing.json unchanged, skipping reload",
			"resourceVersion", cm.ResourceVersion)
		return
	}
	w.lastRoutingJSON = routingJSONStr
	w.lastRoutingJSONMux.Unlock()

	w.logger.Info("routing.json changed, triggering ghost reload",
		"resourceVersion", cm.ResourceVersion)

	// Parse routing config
	config, err := ParseRoutingConfig(data)
	if err != nil {
		w.logger.Error("failed to parse routing config", "error", err)
		return
	}

	w.mu.Lock()
	oldServiceWatch := w.serviceWatch
	w.routingConfig = config
	w.updateServiceWatch(config)

	// Prune endpoints for services no longer watched
	for key := range w.endpoints {
		if _, ok := w.serviceWatch[key]; !ok {
			delete(w.endpoints, key)
			w.logger.Debug("pruned stale endpoints", "service", key)
		}
	}

	// Backfill endpoints for newly-watched services from the informer cache.
	// When the ConfigMap adds new services, the EndpointSlice informer may have
	// already synced those slices but filtered them out because they weren't in
	// serviceWatch at the time. Query the lister to pick them up immediately.
	if w.endpointSliceLister != nil {
		w.backfillEndpoints(oldServiceWatch)
	}
	w.mu.Unlock()

	w.logger.Info("routing config loaded from ConfigMap")

	// Skip reload during initial sync
	if !shouldReload {
		return
	}

	// Regenerate ghost.json and trigger reload with retry
	w.regenerateConfigWithRetry(ctx)
}

// backfillEndpoints queries the EndpointSlice lister for services that are newly watched
// but whose EndpointSlice events were missed. Must be called with w.mu held.
func (w *Watcher) backfillEndpoints(oldServiceWatch map[string]struct{}) {
	for serviceKey := range w.serviceWatch {
		if _, existed := oldServiceWatch[serviceKey]; existed {
			continue // already watching, skip
		}

		// Parse namespace/name from service key
		parts := strings.SplitN(serviceKey, "/", 2)
		if len(parts) != 2 {
			continue
		}
		namespace, serviceName := parts[0], parts[1]

		// Query the EndpointSlice lister for this service
		slices, err := w.endpointSliceLister.EndpointSlices(namespace).List(
			labels.SelectorFromSet(labels.Set{serviceLabelKey: serviceName}))
		if err != nil {
			w.logger.Error("failed to list EndpointSlices from cache",
				"service", serviceKey, "error", err)
			continue
		}

		var allEndpoints []Endpoint
		for _, slice := range slices {
			allEndpoints = append(allEndpoints, extractEndpoints(slice)...)
		}

		if len(allEndpoints) > 0 {
			w.endpoints[serviceKey] = allEndpoints
			w.logger.Info("backfilled endpoints for newly-watched service",
				"service", serviceKey,
				"endpoints", len(allEndpoints))
		}
	}
}

// handleConfigMapDelete processes ConfigMap delete events.
func (w *Watcher) handleConfigMapDelete(ctx context.Context, cm *corev1.ConfigMap) {
	if cm.Name != w.configMapName {
		return
	}

	w.logger.Error("ConfigMap deleted - fatal error", "name", cm.Name)

	// Fatal: can't operate without routing config
	select {
	case w.fatalErrCh <- fmt.Errorf("ConfigMap %s deleted", cm.Name):
	default:
	}
}
