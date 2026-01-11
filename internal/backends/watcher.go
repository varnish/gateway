package backends

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	// debounceDelay is the time to wait after a file change before reloading
	debounceDelay = 100 * time.Millisecond

	// serviceLabelKey is the label used by Kubernetes to identify the service
	serviceLabelKey = "kubernetes.io/service-name"
)

// Watcher watches services.json and Kubernetes EndpointSlices,
// regenerating backends.conf when endpoints change
type Watcher struct {
	servicesPath string
	backendsPath string
	namespace    string
	client       kubernetes.Interface
	logger       *slog.Logger

	// Internal state protected by mutex
	mu              sync.RWMutex
	currentServices map[string]Service    // name -> Service
	endpoints       map[string][]Endpoint // service name -> endpoints
}

// NewWatcher creates a new backend watcher
func NewWatcher(client kubernetes.Interface, servicesPath, backendsPath, namespace string, logger *slog.Logger) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Watcher{
		servicesPath:    servicesPath,
		backendsPath:    backendsPath,
		namespace:       namespace,
		client:          client,
		logger:          logger,
		currentServices: make(map[string]Service),
		endpoints:       make(map[string][]Endpoint),
	}
}

// Run starts watching services.json and EndpointSlices
// It blocks until the context is cancelled
func (w *Watcher) Run(ctx context.Context) error {
	// Load initial services
	if err := w.loadServices(); err != nil {
		return fmt.Errorf("initial loadServices: %w", err)
	}

	// Set up fsnotify for services.json
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify.NewWatcher: %w", err)
	}
	defer fsWatcher.Close()

	// Watch the directory containing services.json
	dir := filepath.Dir(w.servicesPath)
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
		AddFunc: func(obj interface{}) {
			if slice, ok := obj.(*discoveryv1.EndpointSlice); ok {
				w.handleEndpointSliceUpdate(slice)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if slice, ok := newObj.(*discoveryv1.EndpointSlice); ok {
				w.handleEndpointSliceUpdate(slice)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if slice, ok := obj.(*discoveryv1.EndpointSlice); ok {
				w.handleEndpointSliceDelete(slice)
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

	w.logger.Info("backend watcher started",
		"servicesPath", w.servicesPath,
		"backendsPath", w.backendsPath,
		"namespace", w.namespace,
	)

	// Generate initial backends.conf
	if err := w.regenerateBackends(); err != nil {
		w.logger.Error("initial backends generation failed", "error", err)
	}

	var debounceTimer *time.Timer
	filename := filepath.Base(w.servicesPath)

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("backend watcher stopping")
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

			w.logger.Debug("services.json changed", "event", event.Op.String())

			// Debounce rapid changes
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(debounceDelay, func() {
				if err := w.loadServices(); err != nil {
					w.logger.Error("failed to reload services", "error", err)
					return
				}
				if err := w.regenerateBackends(); err != nil {
					w.logger.Error("failed to regenerate backends", "error", err)
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

// loadServices reads and parses services.json
func (w *Watcher) loadServices() error {
	config, err := LoadServicesConfig(w.servicesPath)
	if err != nil {
		return err
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	oldServices := w.currentServices
	w.currentServices = config.ToMap()

	// Log changes
	for name := range w.currentServices {
		if _, existed := oldServices[name]; !existed {
			w.logger.Info("service added", "name", name)
		}
	}
	for name := range oldServices {
		if _, exists := w.currentServices[name]; !exists {
			w.logger.Info("service removed", "name", name)
			// Clean up endpoints for removed services
			delete(w.endpoints, name)
		}
	}

	return nil
}

// handleEndpointSliceUpdate processes an EndpointSlice add or update event
func (w *Watcher) handleEndpointSliceUpdate(slice *discoveryv1.EndpointSlice) {
	serviceName := slice.Labels[serviceLabelKey]
	if serviceName == "" {
		return
	}

	w.mu.Lock()

	// Check if this service is in our watch list
	svc, exists := w.currentServices[serviceName]
	if !exists {
		w.mu.Unlock()
		return
	}

	// Extract ready endpoints
	endpoints := extractEndpoints(slice, svc.Port)
	w.endpoints[serviceName] = endpoints

	w.mu.Unlock()

	w.logger.Debug("endpoints updated",
		"service", serviceName,
		"count", len(endpoints),
	)

	// Regenerate backends.conf
	if err := w.regenerateBackends(); err != nil {
		w.logger.Error("failed to regenerate backends", "error", err)
	}
}

// handleEndpointSliceDelete processes an EndpointSlice delete event
func (w *Watcher) handleEndpointSliceDelete(slice *discoveryv1.EndpointSlice) {
	serviceName := slice.Labels[serviceLabelKey]
	if serviceName == "" {
		return
	}

	w.mu.Lock()

	// Check if this service is in our watch list
	if _, exists := w.currentServices[serviceName]; !exists {
		w.mu.Unlock()
		return
	}

	// Remove endpoints for this service
	delete(w.endpoints, serviceName)

	w.mu.Unlock()

	w.logger.Debug("endpoints deleted", "service", serviceName)

	// Regenerate backends.conf
	if err := w.regenerateBackends(); err != nil {
		w.logger.Error("failed to regenerate backends", "error", err)
	}
}

// regenerateBackends writes the backends.conf file atomically
func (w *Watcher) regenerateBackends() error {
	w.mu.RLock()
	serviceEndpoints := make(ServiceEndpoints, len(w.currentServices))
	for name, svc := range w.currentServices {
		eps, exists := w.endpoints[name]
		if !exists {
			// Service exists in config but no endpoints yet - use empty slice
			eps = []Endpoint{}
		}
		// Override port from services.json config
		for i := range eps {
			eps[i].Port = svc.Port
		}
		serviceEndpoints[name] = eps
	}
	w.mu.RUnlock()

	content := Generate(serviceEndpoints)

	// Atomic write: write to temp file, then rename
	tmpPath := w.backendsPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("os.WriteFile(%s): %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, w.backendsPath); err != nil {
		return fmt.Errorf("os.Rename(%s, %s): %w", tmpPath, w.backendsPath, err)
	}

	w.logger.Info("backends.conf regenerated",
		"services", len(serviceEndpoints),
		"path", w.backendsPath,
	)

	return nil
}

// extractEndpoints extracts ready endpoints from an EndpointSlice
func extractEndpoints(slice *discoveryv1.EndpointSlice, defaultPort int) []Endpoint {
	var endpoints []Endpoint

	for _, ep := range slice.Endpoints {
		// Skip endpoints that are not ready
		if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
			continue
		}

		// Get port - use slice ports if available, otherwise default
		port := defaultPort
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

// ListEndpointSlices lists all EndpointSlices for a service (for testing/debugging)
func (w *Watcher) ListEndpointSlices(ctx context.Context, serviceName string) (*discoveryv1.EndpointSliceList, error) {
	selector := labels.Set{serviceLabelKey: serviceName}.AsSelector()
	return w.client.DiscoveryV1().EndpointSlices(w.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
}
