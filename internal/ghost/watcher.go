package ghost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/varnish/gateway/internal/reload"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
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
	routingConfig   *RoutingConfig        // current routing rules (v1)
	routingConfigV2 *RoutingConfigV2      // v2 routing config with path-based routing
	endpoints       map[string][]Endpoint // service key -> endpoints
	serviceWatch    map[string]struct{}   // services we care about (namespace/name)

	// ConfigMap watching
	configMapName      string // name of ConfigMap to watch
	lastConfigMapRV    string // last seen ResourceVersion for deduplication
	lastRoutingJSON    string // last seen routing.json content for content-based deduplication
	lastRoutingJSONMux sync.RWMutex // protect lastRoutingJSON
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

	// Count services and backends
	serviceCount := len(endpoints)
	backendCount := 0
	for _, eps := range endpoints {
		backendCount += len(eps)
	}

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
			"services", serviceCount,
			"backends", backendCount,
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
			"services", serviceCount,
			"backends", backendCount,
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

	// Parse routing config (v1 or v2)
	w.mu.Lock()
	var versionCheck struct {
		Version int `json:"version"`
	}
	json.Unmarshal(data, &versionCheck)

	switch versionCheck.Version {
	case 2:
		config, err := ParseRoutingConfigV2(data)
		if err != nil {
			w.mu.Unlock()
			w.logger.Error("failed to parse routing config v2", "error", err)
			return
		}
		w.routingConfigV2 = config
		w.routingConfig = nil
		w.updateServiceWatchV2(config)
	case 1:
		config, err := ParseRoutingConfig(data)
		if err != nil {
			w.mu.Unlock()
			w.logger.Error("failed to parse routing config v1", "error", err)
			return
		}
		w.routingConfig = config
		w.routingConfigV2 = nil
		w.updateServiceWatchV1(config)
	}
	w.mu.Unlock()

	w.logger.Info("routing config loaded from ConfigMap",
		"version", versionCheck.Version,
	)

	// Skip reload during initial sync
	if !shouldReload {
		return
	}

	// Regenerate ghost.json and trigger reload
	if err := w.regenerateConfig(ctx); err != nil {
		w.logger.Error("failed to regenerate ghost config", "error", err)
		select {
		case w.fatalErrCh <- err:
		default:
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
