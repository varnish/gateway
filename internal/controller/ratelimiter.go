package controller

import (
	"time"

	"golang.org/x/time/rate"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
)

// defaultRateLimiter returns the standard rate limiter used by all controllers.
// Per-item: exponential backoff 1s → 8s. Global: 10 req/s with burst of 20.
// Keep the cap low — persistent errors need operator attention, not long backoff.
func defaultRateLimiter() workqueue.TypedRateLimiter[ctrl.Request] {
	return workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](time.Second, 8*time.Second),
		&workqueue.TypedBucketRateLimiter[ctrl.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 20)},
	)
}
