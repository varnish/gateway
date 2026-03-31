package controller

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestDefaultRateLimiter_ExponentialBackoff(t *testing.T) {
	rl := defaultRateLimiter()
	item := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"}}

	// First failure: 1s base delay
	d1 := rl.When(item)
	if d1 < time.Second {
		t.Errorf("first failure delay = %v, want >= 1s", d1)
	}

	// Second failure: 2s (doubled)
	d2 := rl.When(item)
	if d2 < 2*time.Second {
		t.Errorf("second failure delay = %v, want >= 2s", d2)
	}

	// Third failure: 4s
	d3 := rl.When(item)
	if d3 < 4*time.Second {
		t.Errorf("third failure delay = %v, want >= 4s", d3)
	}

	// Fourth failure: capped at 8s
	d4 := rl.When(item)
	if d4 > 9*time.Second {
		t.Errorf("fourth failure delay = %v, want <= 8s (cap)", d4)
	}

	// Fifth failure: still capped at 8s
	d5 := rl.When(item)
	if d5 > 9*time.Second {
		t.Errorf("fifth failure delay = %v, want <= 8s (cap)", d5)
	}
}

func TestDefaultRateLimiter_ResetAfterForget(t *testing.T) {
	rl := defaultRateLimiter()
	item := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"}}

	// Drive up the backoff
	rl.When(item)
	rl.When(item)
	rl.When(item)

	// Forget resets per-item backoff
	rl.Forget(item)
	d := rl.When(item)
	if d < time.Second || d > 2*time.Second {
		t.Errorf("after Forget, delay = %v, want ~1s (reset to base)", d)
	}
}

func TestDefaultRateLimiter_IndependentItems(t *testing.T) {
	rl := defaultRateLimiter()
	itemA := ctrl.Request{NamespacedName: types.NamespacedName{Name: "a", Namespace: "default"}}
	itemB := ctrl.Request{NamespacedName: types.NamespacedName{Name: "b", Namespace: "default"}}

	// Drive up backoff for item A
	rl.When(itemA)
	rl.When(itemA)
	rl.When(itemA)

	// Item B should still be at base delay
	d := rl.When(itemB)
	if d < time.Second || d > 2*time.Second {
		t.Errorf("item B delay = %v, want ~1s (independent of item A)", d)
	}
}

func TestDefaultRateLimiter_NumRequeues(t *testing.T) {
	rl := defaultRateLimiter()
	item := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"}}

	if n := rl.NumRequeues(item); n != 0 {
		t.Errorf("initial NumRequeues = %d, want 0", n)
	}

	rl.When(item)
	rl.When(item)
	if n := rl.NumRequeues(item); n != 2 {
		t.Errorf("after 2 failures NumRequeues = %d, want 2", n)
	}

	rl.Forget(item)
	if n := rl.NumRequeues(item); n != 0 {
		t.Errorf("after Forget NumRequeues = %d, want 0", n)
	}
}
