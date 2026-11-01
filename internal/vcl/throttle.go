package vcl

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Defaults for the route VCL compile throttle. A route VCL costs roughly
// 0.2 s of CPU to compile (cc + vmod load), so 4/s keeps a populated pool
// from monopolising a 100m CPU request for long while the burst of 8 keeps
// a single policy change instant. The bucket starts full.
const (
	DefaultRouteVCLCompileRate  = 4.0
	DefaultRouteVCLCompileBurst = 8
)

// compileThrottle is a token bucket in front of route VCL vcl.inline calls.
// A nil limiter disables throttling. now and sleep are injectable for tests.
type compileThrottle struct {
	mu    sync.Mutex
	lim   *rate.Limiter
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// newCompileThrottle returns a throttle allowing perSecond compiles per
// second with the given burst. perSecond <= 0 disables it; a burst below 1
// is raised to 1 (a limiter with no burst would never allow anything).
func newCompileThrottle(perSecond float64, burst int) *compileThrottle {
	t := &compileThrottle{now: time.Now, sleep: sleepCtx}
	if perSecond > 0 {
		if burst < 1 {
			burst = 1
		}
		t.lim = rate.NewLimiter(rate.Limit(perSecond), burst)
	}
	return t
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// wait blocks until a compile may start. It returns ctx.Err() when the
// context ends first; the reserved token is then given back.
func (t *compileThrottle) wait(ctx context.Context) error {
	if t == nil || t.lim == nil {
		return nil
	}
	t.mu.Lock()
	now := t.now()
	res := t.lim.ReserveN(now, 1)
	t.mu.Unlock()
	delay := res.DelayFrom(now)
	if delay <= 0 {
		return nil
	}
	if err := t.sleep(ctx, delay); err != nil {
		res.CancelAt(t.now())
		return err
	}
	return nil
}
