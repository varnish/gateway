package vcl

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/varnish/gateway/internal/varnishadm"
)

const (
	// DefaultKeepCount is the default number of old VCLs to keep for rollback
	DefaultKeepCount = 3

	// vclPrefix is the prefix for managed VCL names
	vclPrefix = "vcl_"

	// debounceDelay is the time to wait after a file change before reloading
	debounceDelay = 100 * time.Millisecond
)

// Reloader watches a VCL file and hot-reloads it into Varnish when it changes
type Reloader struct {
	varnishadm varnishadm.VarnishadmInterface
	vclPath    string
	keepCount  int
	logger     *slog.Logger
}

// New creates a new VCL reloader
func New(v varnishadm.VarnishadmInterface, vclPath string, keepCount int, logger *slog.Logger) *Reloader {
	if keepCount <= 0 {
		keepCount = DefaultKeepCount
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Reloader{
		varnishadm: v,
		vclPath:    vclPath,
		keepCount:  keepCount,
		logger:     logger,
	}
}

// Run starts watching the VCL file and reloading on changes
// It blocks until the context is cancelled
func (r *Reloader) Run(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify.NewWatcher(): %w", err)
	}
	defer watcher.Close()

	// Watch the directory containing the VCL file
	// This handles file recreation (e.g., ConfigMap updates which replace files)
	dir := filepath.Dir(r.vclPath)
	if err := watcher.Add(dir); err != nil {
		return fmt.Errorf("watcher.Add(%s): %w", dir, err)
	}

	r.logger.Info("VCL reloader started", "path", r.vclPath, "keepCount", r.keepCount)

	var debounceTimer *time.Timer
	filename := filepath.Base(r.vclPath)

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("VCL reloader stopping")
			return ctx.Err()

		case event, ok := <-watcher.Events:
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

			r.logger.Debug("VCL file changed", "event", event.Op.String())

			// Debounce rapid changes
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(debounceDelay, func() {
				if err := r.Reload(); err != nil {
					r.logger.Error("VCL reload failed", "error", err)
				}
			})

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			r.logger.Error("watcher error", "error", err)
		}
	}
}

// Reload performs a single VCL reload
func (r *Reloader) Reload() error {
	name := r.generateVCLName()

	r.logger.Info("loading VCL", "name", name, "path", r.vclPath)

	// Load the new VCL
	resp, err := r.varnishadm.VCLLoad(name, r.vclPath)
	if err != nil {
		return fmt.Errorf("varnishadm.VCLLoad(%s, %s): %w", name, r.vclPath, err)
	}
	if resp.StatusCode() != varnishadm.ClisOk {
		r.logger.Error("VCL compilation failed",
			"name", name,
			"status", resp.StatusCode(),
			"output", resp.Payload(),
		)
		return fmt.Errorf("VCL compilation failed: %s", resp.Payload())
	}

	// Switch to the new VCL
	r.logger.Info("activating VCL", "name", name)
	resp, err = r.varnishadm.VCLUse(name)
	if err != nil {
		return fmt.Errorf("varnishadm.VCLUse(%s): %w", name, err)
	}
	if resp.StatusCode() != varnishadm.ClisOk {
		r.logger.Error("VCL activation failed",
			"name", name,
			"status", resp.StatusCode(),
			"output", resp.Payload(),
		)
		return fmt.Errorf("VCL activation failed: %s", resp.Payload())
	}

	r.logger.Info("VCL reload complete", "name", name)

	// Garbage collect old VCLs
	if err := r.garbageCollect(); err != nil {
		r.logger.Warn("VCL garbage collection failed", "error", err)
		// Non-fatal, continue
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
		if resp.StatusCode() != varnishadm.ClisOk {
			r.logger.Warn("VCL discard failed",
				"name", name,
				"status", resp.StatusCode(),
				"output", resp.Payload(),
			)
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
