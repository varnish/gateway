package controller

import (
	"context"
	"log/slog"
	"time"

	v1alpha1 "github.com/varnish/gateway/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	gcInterval = 5 * time.Minute
	defaultTTL = 1 * time.Hour
)

// CacheInvalidationGC periodically deletes completed or failed CacheInvalidation
// resources whose TTL has expired. It implements manager.Runnable.
type CacheInvalidationGC struct {
	Client client.Client
	Logger *slog.Logger
}

// Start runs the GC loop until the context is cancelled.
func (gc *CacheInvalidationGC) Start(ctx context.Context) error {
	gc.Logger.Info("starting CacheInvalidation GC", "interval", gcInterval, "defaultTTL", defaultTTL)

	ticker := time.NewTicker(gcInterval)
	defer ticker.Stop()

	// Run once immediately at startup.
	gc.collect(ctx)

	for {
		select {
		case <-ctx.Done():
			gc.Logger.Info("stopping CacheInvalidation GC")
			return nil
		case <-ticker.C:
			gc.collect(ctx)
		}
	}
}

// collect lists all CacheInvalidation resources and deletes those eligible for GC.
func (gc *CacheInvalidationGC) collect(ctx context.Context) {
	var list v1alpha1.CacheInvalidationList
	if err := gc.Client.List(ctx, &list); err != nil {
		gc.Logger.Error("failed to list CacheInvalidation resources", "error", err)
		return
	}

	now := time.Now()
	deleted := 0

	for i := range list.Items {
		ci := &list.Items[i]

		if !gc.isEligible(ci, now) {
			continue
		}

		if err := gc.Client.Delete(ctx, ci); err != nil {
			gc.Logger.Error("failed to delete CacheInvalidation",
				"name", ci.Name,
				"namespace", ci.Namespace,
				"error", err)
			continue
		}

		gc.Logger.Info("deleted expired CacheInvalidation",
			"name", ci.Name,
			"namespace", ci.Namespace,
			"phase", ci.Status.Phase)
		deleted++
	}

	if deleted > 0 {
		gc.Logger.Info("GC sweep complete", "deleted", deleted, "total", len(list.Items))
	}
}

// isEligible returns true if the CacheInvalidation is in a terminal phase
// and its TTL has expired since completion.
func (gc *CacheInvalidationGC) isEligible(ci *v1alpha1.CacheInvalidation, now time.Time) bool {
	// Only collect terminal resources.
	if ci.Status.Phase != v1alpha1.CacheInvalidationComplete &&
		ci.Status.Phase != v1alpha1.CacheInvalidationFailed {
		return false
	}

	// Must have a completion timestamp.
	if ci.Status.CompletedAt == nil {
		return false
	}

	ttl := defaultTTL
	if ci.Spec.TTL != nil {
		ttl = ci.Spec.TTL.Duration
	}

	expiry := ci.Status.CompletedAt.Time.Add(ttl)
	return now.After(expiry)
}

// NeedLeaderElection returns true so the GC only runs on the leader.
func (gc *CacheInvalidationGC) NeedLeaderElection() bool {
	return true
}

