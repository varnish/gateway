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
	"github.com/varnish/gateway/internal/dashboard"
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

	// Dashboard integration (optional)
	dashBus *dashboard.EventBus
}

// isCommsError reports whether err originated from a varnishadm CLI
// connection-level failure (drop, comms timeout, server shutdown) — as
// opposed to a status-level rejection from varnishd. Such errors are
// ambiguous about server-side state: the command may have actually landed
// before the response was lost, so transaction-rollback is unsafe.
func isCommsError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "varnishadm communication failed") ||
		strings.Contains(msg, "varnishadm connection lost") ||
		strings.Contains(msg, "varnishadm server stop")
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

// SetDashboard connects the reloader to the dashboard event bus.
func (r *Reloader) SetDashboard(bus *dashboard.EventBus) {
	r.dashBus = bus
}

// FatalError returns a channel that receives fatal errors from TLS reload failures.
func (r *Reloader) FatalError() <-chan error {
	return r.fatalErrCh
}

// rollbackAfterTransportError issues a tls.cert.rollback after a transport
// (RPC-level) error from tls.cert.load or tls.cert.commit, unless the error
// is comms-class. Comms errors are ambiguous about server-side state — the
// command may have actually landed before the response was lost — so issuing
// a rollback then would either no-op or, worse, undo a transaction that
// actually committed. op is used only to label the log message (e.g.
// "load", "commit"). Shared by LoadAll and reloadAllCerts so the rollback
// policy cannot diverge between the startup and hot-reload paths (M-23).
func (r *Reloader) rollbackAfterTransportError(err error, op string) {
	if isCommsError(err) {
		r.logger.Warn("TLS cert " + op + " comms-error; skipping rollback to avoid state divergence")
		return
	}
	if _, rbErr := r.varnishadm.TLSCertRollback(); rbErr != nil {
		r.logger.Error("TLS cert rollback failed after "+op+" error", "error", rbErr)
	}
}

// rollbackAfterStatusError issues a tls.cert.rollback after a status-level
// rejection (the RPC succeeded but varnishd reported failure, e.g. a
// malformed cert). The server-side transaction is in a known state here, so
// rollback is always safe — no comms-error branching needed.
func (r *Reloader) rollbackAfterStatusError(op string) {
	if _, rbErr := r.varnishadm.TLSCertRollback(); rbErr != nil {
		r.logger.Error("TLS cert rollback failed after "+op+" error", "error", rbErr)
	}
}

// loadCertFiles loads each .pem file in entries into Varnish via
// tls.cert.load, staging them into whatever transaction is currently open
// (LoadAll starts none explicitly; reloadAllCerts opens one via discards).
// On error it applies the same comms-aware rollback policy used for commit
// errors and returns the count loaded so far alongside the error.
func (r *Reloader) loadCertFiles(entries []os.DirEntry) (int, error) {
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
			r.rollbackAfterTransportError(err, "load")
			return loaded, fmt.Errorf("varnishadm.TLSCertLoad(%s, %s): %w", name, path, err)
		}
		if err := resp.CheckOK("tls.cert.load %s failed", name); err != nil {
			r.rollbackAfterStatusError("load")
			return loaded, err
		}
		loaded++
	}
	return loaded, nil
}

// commitCerts issues tls.cert.commit for the currently open transaction,
// applying the same comms-aware rollback policy as loadCertFiles.
func (r *Reloader) commitCerts() error {
	resp, err := r.varnishadm.TLSCertCommit()
	if err != nil {
		r.rollbackAfterTransportError(err, "commit")
		return fmt.Errorf("varnishadm.TLSCertCommit: %w", err)
	}
	if err := resp.CheckOK("tls.cert.commit failed"); err != nil {
		r.rollbackAfterStatusError("commit")
		return err
	}
	return nil
}

// LoadAll reads all .pem files from the cert directory and loads them into Varnish.
// Should be called once during startup before the Varnish child starts.
func (r *Reloader) LoadAll() error {
	entries, err := os.ReadDir(r.certDir)
	if err != nil {
		return fmt.Errorf("os.ReadDir(%s): %w", r.certDir, err)
	}

	loaded, err := r.loadCertFiles(entries)
	if err != nil {
		return err
	}

	if loaded == 0 {
		r.logger.Warn("no .pem files found in TLS cert directory", "dir", r.certDir)
		return nil
	}

	if err := r.commitCerts(); err != nil {
		return err
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
		if err := resp.CheckOK("tls.cert.discard %s failed", entry.CertificateID); err != nil {
			return err
		}
	}

	loaded, err := r.loadCertFiles(entries)
	if err != nil {
		return err
	}

	// If nothing was discarded and nothing loaded, no transaction was started
	if len(result.Entries) == 0 && loaded == 0 {
		r.logger.Warn("no TLS certificates to load or discard")
		return nil
	}

	// Commit the transaction. Uses the same isCommsError-aware rollback
	// policy as LoadAll (via commitCerts/rollbackAfterTransportError) — a
	// comms error here is ambiguous about whether the commit landed
	// server-side, so unconditionally rolling back would risk undoing a
	// transaction that actually committed (M-23).
	if err := r.commitCerts(); err != nil {
		return err
	}

	r.logger.Info("TLS certificates reloaded via load/commit cycle", "count", loaded)
	dashboard.PublishTLSReload(r.dashBus, loaded)
	return nil
}

// shouldReload reports whether an fsnotify event should trigger a certificate
// reload. Two kinds of change are relevant:
//
//  1. Direct .pem writes — used by non-Kubernetes deployments and tests.
//  2. Kubernetes Secret volume rotation. K8s populates mounted Secret volumes
//     with the atomic-writer pattern: it writes cert data into a new
//     "..<timestamp>" directory and atomically swaps the "..data" symlink to
//     point at it (surfacing as a Create/Rename on "..data"). The visible
//     foo.pem entries are symlinks created only at initial volume setup and
//     never touched again, so no .pem event ever fires on renewal. Watching
//     "..data" is what makes cert-manager renewals / Secret updates actually
//     trigger a reload instead of serving the stale cert until pod restart.
func shouldReload(event fsnotify.Event) bool {
	// Only act on ops that can change cert contents on disk.
	if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) && !event.Has(fsnotify.Remove) && !event.Has(fsnotify.Rename) {
		return false
	}
	// event.Name is a full path; the atomic-writer symlink is the base "..data".
	return filepath.Base(event.Name) == "..data" || strings.HasSuffix(event.Name, ".pem")
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

			if !shouldReload(event) {
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
					dashboard.PublishTLSReloadFail(r.dashBus, err)
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
