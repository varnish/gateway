package tls

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/varnish/gateway/internal/varnishadm"
)

// Reloader loads and hot-reloads TLS certificates into Varnish via varnishadm.
// It watches a directory of .pem files and performs a full load/commit cycle on changes,
// which handles new, updated, and removed certificates.
type Reloader struct {
	varnishadm    varnishadm.VarnishadmInterface
	certDir       string
	logger        *slog.Logger
	fatalErrCh    chan error
	fatalErrOnce  sync.Once
	debounceDelay time.Duration
	reloadMu      sync.Mutex // protects reloadAllCerts from concurrent execution
}

// New creates a new TLS Reloader.
func New(vadm varnishadm.VarnishadmInterface, certDir string, logger *slog.Logger) *Reloader {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reloader{
		varnishadm:    vadm,
		certDir:       certDir,
		logger:        logger,
		fatalErrCh:    make(chan error, 1),
		debounceDelay: 200 * time.Millisecond,
	}
}

// FatalError returns a channel that receives fatal errors from TLS reload failures.
func (r *Reloader) FatalError() <-chan error {
	return r.fatalErrCh
}

// LoadAll reads all .pem files from the cert directory and loads them into Varnish.
// Should be called once during startup before the Varnish child starts.
func (r *Reloader) LoadAll() error {
	entries, err := os.ReadDir(r.certDir)
	if err != nil {
		return fmt.Errorf("os.ReadDir(%s): %w", r.certDir, err)
	}

	var loaded int
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".pem") {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".pem")
		path := filepath.Join(r.certDir, entry.Name())

		r.logger.Debug("loading TLS certificate", "name", name, "path", path)

		resp, err := r.varnishadm.TLSCertLoad(name, path, "")
		if err != nil {
			return fmt.Errorf("varnishadm.TLSCertLoad(%s, %s): %w", name, path, err)
		}
		if resp.StatusCode() != varnishadm.ClisOk {
			return fmt.Errorf("tls.cert.load %s failed (status %d): %s", name, resp.StatusCode(), resp.Payload())
		}
		loaded++
	}

	if loaded == 0 {
		r.logger.Warn("no .pem files found in TLS cert directory", "dir", r.certDir)
		return nil
	}

	// Commit all loaded certificates
	resp, err := r.varnishadm.TLSCertCommit()
	if err != nil {
		return fmt.Errorf("varnishadm.TLSCertCommit: %w", err)
	}
	if resp.StatusCode() != varnishadm.ClisOk {
		return fmt.Errorf("tls.cert.commit failed (status %d): %s", resp.StatusCode(), resp.Payload())
	}

	r.logger.Info("TLS certificates loaded and committed", "count", loaded)
	return nil
}

// reloadAllCerts performs a full TLS certificate reload cycle:
// list current certs, discard them, load all .pem files from disk, and commit.
// This handles additions, removals, and updates in a single transaction.
func (r *Reloader) reloadAllCerts() error {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	// Read all .pem files from disk first, before modifying Varnish state
	entries, err := os.ReadDir(r.certDir)
	if err != nil {
		return fmt.Errorf("os.ReadDir(%s): %w", r.certDir, err)
	}

	// List currently loaded certificates
	result, err := r.varnishadm.TLSCertListStructured()
	if err != nil {
		return fmt.Errorf("varnishadm.TLSCertListStructured: %w", err)
	}

	// Discard each currently loaded cert (starts a transaction)
	for _, entry := range result.Entries {
		resp, err := r.varnishadm.TLSCertDiscard(entry.CertificateID)
		if err != nil {
			return fmt.Errorf("varnishadm.TLSCertDiscard(%s): %w", entry.CertificateID, err)
		}
		if resp.StatusCode() != varnishadm.ClisOk {
			return fmt.Errorf("tls.cert.discard %s failed (status %d): %s",
				entry.CertificateID, resp.StatusCode(), resp.Payload())
		}
	}

	var loaded int
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pem") {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".pem")
		path := filepath.Join(r.certDir, entry.Name())

		r.logger.Debug("loading TLS certificate", "name", name, "path", path)

		resp, err := r.varnishadm.TLSCertLoad(name, path, "")
		if err != nil {
			if _, rbErr := r.varnishadm.TLSCertRollback(); rbErr != nil {
				r.logger.Error("TLS cert rollback failed after load error", "error", rbErr)
			}
			return fmt.Errorf("varnishadm.TLSCertLoad(%s, %s): %w", name, path, err)
		}
		if resp.StatusCode() != varnishadm.ClisOk {
			if _, rbErr := r.varnishadm.TLSCertRollback(); rbErr != nil {
				r.logger.Error("TLS cert rollback failed after load error", "error", rbErr)
			}
			return fmt.Errorf("tls.cert.load %s failed (status %d): %s",
				name, resp.StatusCode(), resp.Payload())
		}
		loaded++
	}

	// If nothing was discarded and nothing loaded, no transaction was started
	if len(result.Entries) == 0 && loaded == 0 {
		r.logger.Warn("no TLS certificates to load or discard")
		return nil
	}

	// Commit the transaction
	resp, err := r.varnishadm.TLSCertCommit()
	if err != nil {
		if _, rbErr := r.varnishadm.TLSCertRollback(); rbErr != nil {
			r.logger.Error("TLS cert rollback failed after commit error", "error", rbErr)
		}
		return fmt.Errorf("varnishadm.TLSCertCommit: %w", err)
	}
	if resp.StatusCode() != varnishadm.ClisOk {
		if _, rbErr := r.varnishadm.TLSCertRollback(); rbErr != nil {
			r.logger.Error("TLS cert rollback failed after commit error", "error", rbErr)
		}
		return fmt.Errorf("tls.cert.commit failed (status %d): %s", resp.StatusCode(), resp.Payload())
	}

	r.logger.Info("TLS certificates reloaded via load/commit cycle", "count", loaded)
	return nil
}

// Run starts watching the cert directory for changes and reloads certs when files change.
// It blocks until the context is cancelled.
func (r *Reloader) Run(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify.NewWatcher: %w", err)
	}
	defer watcher.Close()

	if err := watcher.Add(r.certDir); err != nil {
		return fmt.Errorf("watcher.Add(%s): %w", r.certDir, err)
	}

	r.logger.Info("TLS cert watcher started", "dir", r.certDir)

	var debounceTimer *time.Timer
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			// React to write, create, and remove events on .pem files
			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) && !event.Has(fsnotify.Remove) {
				continue
			}

			if !strings.HasSuffix(event.Name, ".pem") {
				continue
			}

			r.logger.Debug("TLS cert file changed, scheduling reload",
				"file", event.Name, "op", event.Op)

			// Debounce: reset timer on each event
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(r.debounceDelay, func() {
				r.logger.Info("reloading TLS certificates")
				if err := r.reloadAllCerts(); err != nil {
					r.logger.Error("TLS cert reload failed", "error", err)
					r.fatalErrOnce.Do(func() {
						r.fatalErrCh <- fmt.Errorf("TLS cert reload failed: %w", err)
					})
					return
				}
				r.logger.Info("TLS certificates reloaded successfully")
			})

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			r.logger.Error("TLS cert watcher error", "error", err)

		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			r.logger.Info("TLS cert watcher stopping")
			return ctx.Err()
		}
	}
}
