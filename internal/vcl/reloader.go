package vcl

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/varnish/gateway/internal/varnishadm"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	// DefaultKeepCount is the default number of old VCLs to keep for rollback
	DefaultKeepCount = 3

	// vclPrefix is the prefix for managed VCL names
	vclPrefix = "vcl_"
)

// Reloader watches a VCL file and hot-reloads it into Varnish when it changes
type Reloader struct {
	varnishadm varnishadm.VarnishadmInterface
	vclPath    string
	keepCount  int
	logger     *slog.Logger

	// ConfigMap watching
	kubeClient         kubernetes.Interface
	configMapName      string
	configMapNamespace string
	lastVCL            string
	lastVCLMux         sync.RWMutex
	lastConfigMapRV    string

	// Fatal error signaling
	fatalErrCh   chan error
	fatalErrOnce sync.Once
}

// New creates a new VCL reloader
func New(
	v varnishadm.VarnishadmInterface,
	vclPath string,
	keepCount int,
	kubeClient kubernetes.Interface,
	configMapName string,
	configMapNamespace string,
	logger *slog.Logger,
) *Reloader {
	if keepCount <= 0 {
		keepCount = DefaultKeepCount
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Reloader{
		varnishadm:         v,
		vclPath:            vclPath,
		keepCount:          keepCount,
		kubeClient:         kubeClient,
		configMapName:      configMapName,
		configMapNamespace: configMapNamespace,
		logger:             logger,
		fatalErrCh:         make(chan error, 1),
	}
}

// FatalError returns a channel that receives fatal errors from VCL reload failures.
// VCL reload failures are fatal because the gateway cannot serve with incorrect configuration.
func (r *Reloader) FatalError() <-chan error {
	return r.fatalErrCh
}

// Run starts watching the VCL file and reloading on changes
// It blocks until the context is cancelled
func (r *Reloader) Run(ctx context.Context) error {
	r.logger.Debug("VCL reloader started",
		"path", r.vclPath,
		"configMapName", r.configMapName,
		"namespace", r.configMapNamespace,
		"keepCount", r.keepCount)

	// Set up ConfigMap informer
	factory := informers.NewSharedInformerFactoryWithOptions(
		r.kubeClient,
		30*time.Second,
		informers.WithNamespace(r.configMapNamespace),
	)

	configMapInformer := factory.Core().V1().ConfigMaps().Informer()
	_, err := configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if cm, ok := obj.(*corev1.ConfigMap); ok {
				r.handleConfigMapUpdate(ctx, cm)
			}
		},
		UpdateFunc: func(_, newObj any) {
			if cm, ok := newObj.(*corev1.ConfigMap); ok {
				r.handleConfigMapUpdate(ctx, cm)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("configMapInformer.AddEventHandler: %w", err)
	}

	// Start the informer
	factory.Start(ctx.Done())

	// Wait for informer to sync
	if !cache.WaitForCacheSync(ctx.Done(), configMapInformer.HasSynced) {
		return fmt.Errorf("failed to sync ConfigMap cache")
	}

	r.logger.Info("VCL reloader ready")

	// Wait for context cancellation
	<-ctx.Done()
	r.logger.Info("VCL reloader stopping")
	return ctx.Err()
}

// handleConfigMapUpdate processes ConfigMap add/update events
func (r *Reloader) handleConfigMapUpdate(ctx context.Context, cm *corev1.ConfigMap) {
	// Filter: only our ConfigMap
	if cm.Name != r.configMapName {
		return
	}

	// Deduplicate via ResourceVersion
	if cm.ResourceVersion != "" && cm.ResourceVersion == r.lastConfigMapRV {
		r.logger.Debug("skipping duplicate ConfigMap update", "resourceVersion", cm.ResourceVersion)
		return
	}
	r.lastConfigMapRV = cm.ResourceVersion

	// Extract main.vcl
	newVCL, ok := cm.Data["main.vcl"]
	if !ok {
		r.logger.Warn("ConfigMap missing main.vcl key", "name", cm.Name)
		return
	}

	// Check if VCL content actually changed
	r.lastVCLMux.Lock()
	if r.lastVCL == newVCL {
		r.lastVCLMux.Unlock()
		r.logger.Debug("ConfigMap updated but main.vcl unchanged, skipping reload",
			"resourceVersion", cm.ResourceVersion)
		return
	}
	r.lastVCL = newVCL
	r.lastVCLMux.Unlock()

	r.logger.Info("main.vcl changed, triggering VCL reload",
		"resourceVersion", cm.ResourceVersion)

	// Trigger varnishadm reload with inline VCL
	if err := r.ReloadInline(newVCL); err != nil {
		r.logger.Error("VCL reload failed - this is fatal", "error", err)
		// Send fatal error (only once to avoid blocking)
		r.fatalErrOnce.Do(func() {
			r.fatalErrCh <- fmt.Errorf("VCL reload failed: %w", err)
		})
	}
}

// Reload performs a single VCL reload from the configured file path
func (r *Reloader) Reload() error {
	name := r.generateVCLName()
	r.logger.Debug("loading VCL", "name", name, "path", r.vclPath)
	return r.loadAndActivate(name, func(n string) (varnishadm.VarnishResponse, error) {
		return r.varnishadm.VCLLoad(n, r.vclPath)
	})
}

// ReloadInline performs a single VCL reload with inline VCL content
func (r *Reloader) ReloadInline(vcl string) error {
	name := r.generateVCLName()
	r.logger.Debug("loading inline VCL", "name", name)
	return r.loadAndActivate(name, func(n string) (varnishadm.VarnishResponse, error) {
		return r.varnishadm.VCLInline(n, vcl)
	})
}

// loadAndActivate loads a VCL using the provided loadFn, activates it, and garbage collects old VCLs.
func (r *Reloader) loadAndActivate(name string, loadFn func(string) (varnishadm.VarnishResponse, error)) error {
	resp, err := loadFn(name)
	if err != nil {
		return fmt.Errorf("VCL load %s: %w", name, err)
	}
	if err := resp.CheckOK("VCL compilation failed"); err != nil {
		return err
	}

	r.logger.Debug("activating VCL", "name", name)
	resp, err = r.varnishadm.VCLUse(name)
	if err != nil {
		return fmt.Errorf("varnishadm.VCLUse(%s): %w", name, err)
	}
	if err := resp.CheckOK("VCL activation failed"); err != nil {
		return err
	}

	r.logger.Debug("VCL reload complete", "name", name)

	if err := r.garbageCollect(); err != nil {
		r.logger.Warn("VCL garbage collection failed", "error", err)
	}

	return nil
}

// garbageCollect removes old managed VCLs beyond keepCount
func (r *Reloader) garbageCollect() error {
	result, err := r.varnishadm.VCLListStructured()
	if err != nil {
		return fmt.Errorf("varnishadm.VCLListStructured(): %w", err)
	}

	// Filter to our managed VCLs (prefix vcl_) that are available (not active) and not labels
	var managed []string
	for _, entry := range result.Entries {
		// Skip active VCL
		if entry.Status == "active" {
			continue
		}
		// Skip labels (they have a target)
		if entry.LabelTarget != "" {
			continue
		}
		// Skip VCLs we don't manage
		if !strings.HasPrefix(entry.Name, vclPrefix) {
			continue
		}
		managed = append(managed, entry.Name)
	}

	// Sort by name (timestamp makes them sortable, oldest first)
	sort.Strings(managed)

	// Discard oldest beyond keepCount
	toDiscard := len(managed) - r.keepCount
	if toDiscard <= 0 {
		return nil
	}

	for i := range toDiscard {
		name := managed[i]
		r.logger.Debug("discarding old VCL", "name", name)
		resp, err := r.varnishadm.VCLDiscard(name)
		if err != nil {
			r.logger.Warn("VCL discard failed", "name", name, "error", err)
			continue
		}
		if err := resp.CheckOK("VCL discard %s failed", name); err != nil {
			r.logger.Warn("VCL discard failed", "name", name, "error", err)
		}
	}

	return nil
}

// generateVCLName creates a unique timestamped VCL name
func (r *Reloader) generateVCLName() string {
	now := time.Now()
	return fmt.Sprintf("%s%s_%03d",
		vclPrefix,
		now.Format("20060102_150405"),
		now.Nanosecond()/1e6, // milliseconds
	)
}
