package ghost

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
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

	// Internal state protected by mutex
	mu            sync.RWMutex
	routingConfig *RoutingConfig            // current routing rules
	endpoints     map[string][]Endpoint     // service key -> endpoints
	serviceWatch  map[string]struct{}       // services we care about (namespace/name)
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
		endpoints:         make(map[string][]Endpoint),
		serviceWatch:      make(map[string]struct{}),
	}
}

// Run starts watching routing config and EndpointSlices.
// It blocks until the context is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
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

	// Set up EndpointSlice informer
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

	// Wait for cache sync
	if !cache.WaitForCacheSync(ctx.Done(), endpointSliceInformer.HasSynced) {
		return fmt.Errorf("failed to sync EndpointSlice cache")
	}

	w.logger.Info("ghost watcher started",
		"routingConfigPath", w.routingConfigPath,
		"ghostConfigPath", w.ghostConfigPath,
		"varnishAddr", w.varnishAddr,
		"namespace", w.namespace,
	)

	// Generate initial ghost.json
	if err := w.regenerateConfig(ctx); err != nil {
		w.logger.Error("initial ghost config generation failed", "error", err)
	}

	var debounceTimer *time.Timer
	filename := filepath.Base(w.routingConfigPath)

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("ghost watcher stopping")
			return ctx.Err()

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
					return
				}
				if err := w.regenerateConfig(ctx); err != nil {
					w.logger.Error("failed to regenerate ghost config", "error", err)
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
func (w *Watcher) loadRoutingConfig() error {
	config, err := LoadRoutingConfig(w.routingConfigPath)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	w.routingConfig = config

	// Update the set of services we care about
	w.serviceWatch = make(map[string]struct{})
	for _, rule := range config.VHosts {
		key := ServiceKey(rule.Namespace, rule.Service)
		w.serviceWatch[key] = struct{}{}
	}
	if config.Default != nil {
		key := ServiceKey(config.Default.Namespace, config.Default.Service)
		w.serviceWatch[key] = struct{}{}
	}

	w.logger.Info("routing config loaded",
		"vhosts", len(config.VHosts),
		"hasDefault", config.Default != nil,
	)

	return nil
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
	endpoints := extractEndpoints(slice)
	w.endpoints[key] = endpoints

	w.mu.Unlock()

	w.logger.Debug("endpoints updated",
		"service", key,
		"count", len(endpoints),
	)

	// Regenerate ghost.json
	if err := w.regenerateConfig(ctx); err != nil {
		w.logger.Error("failed to regenerate ghost config", "error", err)
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

	// Remove endpoints for this service
	delete(w.endpoints, key)

	w.mu.Unlock()

	w.logger.Debug("endpoints deleted", "service", key)

	// Regenerate ghost.json
	if err := w.regenerateConfig(ctx); err != nil {
		w.logger.Error("failed to regenerate ghost config", "error", err)
	}
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

	routingConfig := w.routingConfig
	w.mu.RUnlock()

	// Generate ghost config
	config := Generate(routingConfig, endpoints)

	// Write ghost.json atomically
	if err := WriteConfig(w.ghostConfigPath, config); err != nil {
		return fmt.Errorf("WriteConfig: %w", err)
	}

	w.logger.Info("ghost.json regenerated",
		"vhosts", len(config.VHosts),
		"path", w.ghostConfigPath,
	)

	// Trigger ghost reload
	if err := w.reloadClient.TriggerReload(ctx); err != nil {
		w.logger.Warn("ghost reload failed (varnish may not be ready yet)", "error", err)
		// Don't return error - varnish may not be ready yet
	} else {
		w.logger.Info("ghost reload triggered successfully")
	}

	return nil
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
