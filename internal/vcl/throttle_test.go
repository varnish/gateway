package vcl

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/varnish/gateway/internal/dashboard"
)

// fakeSleeper drives a compileThrottle on a virtual clock: sleeping advances
// the clock instead of blocking, and every sleep is recorded.
type fakeSleeper struct {
	t      time.Time
	sleeps []time.Duration
}

func newFakeSleeper() *fakeSleeper {
	return &fakeSleeper{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (f *fakeSleeper) install(th *compileThrottle) {
	th.now = func() time.Time { return f.t }
	th.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		f.sleeps = append(f.sleeps, d)
		f.t = f.t.Add(d)
		return nil
	}
}

func TestCompileThrottle_BurstThenRate(t *testing.T) {
	f := newFakeSleeper()
	th := newCompileThrottle(2, 3) // 2/s, bucket starts with 3 tokens
	f.install(th)

	for i := range 7 {
		if err := th.wait(context.Background()); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
	// The first 3 are free, each of the other 4 waits one token (500 ms).
	if len(f.sleeps) != 4 {
		t.Fatalf("sleeps = %v, want 4 waits", f.sleeps)
	}
	var total time.Duration
	for _, d := range f.sleeps {
		total += d
	}
	if total != 2*time.Second {
		t.Errorf("total wait = %v, want 2s", total)
	}
}

func TestCompileThrottle_DisabledAndNil(t *testing.T) {
	for name, th := range map[string]*compileThrottle{
		"rate 0":  newCompileThrottle(0, 8),
		"nil":     nil,
		"neg":     newCompileThrottle(-1, 8),
		"burst 0": newCompileThrottle(0, 0),
	} {
		f := newFakeSleeper()
		if th != nil {
			f.install(th)
		}
		for range 100 {
			if err := th.wait(context.Background()); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if len(f.sleeps) != 0 {
			t.Errorf("%s: slept %v", name, f.sleeps)
		}
	}
}

func TestCompileThrottle_BurstBelowOneIsRaised(t *testing.T) {
	f := newFakeSleeper()
	th := newCompileThrottle(1, 0)
	f.install(th)
	for range 3 {
		if err := th.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.sleeps) != 2 {
		t.Errorf("sleeps = %v, want 2 (burst raised to 1)", f.sleeps)
	}
}

func TestCompileThrottle_CancelledWaitReturnsToken(t *testing.T) {
	th := newCompileThrottle(1, 1) // real clock and sleep
	if err := th.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := th.wait(ctx) // needs ~1 s, the context ends first
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("wait ignored the context for %v", time.Since(start))
	}
}

// TestReconcile_RouteCompilesAreThrottled checks that N route loads in one
// pass wait N-burst times, and that the master and the placeholder never wait.
func TestReconcile_RouteCompilesAreThrottled(t *testing.T) {
	r, mock := newTestReloader(t)
	f := newFakeSleeper()
	r.SetCompileThrottle(2, 4)
	f.install(r.throttle)

	r.setVCLConfigMap(vclConfigMap(10, ""))
	docs := map[int]string{}
	for i := range 10 {
		docs[i] = routeDoc(fmt.Sprint("v", i))
	}

	// Pool with no policies: placeholder and master compile, nothing waits.
	r.setRouteConfigMap(routeConfigMap(nil))
	mustReconcile(t, r)
	if len(f.sleeps) != 0 {
		t.Fatalf("master/placeholder were throttled: %v", f.sleeps)
	}
	if n := countPrefix(commandNames(mock.GetCallHistory()), "vcl.inline "); n != 2 {
		t.Fatalf("inlines = %d, want placeholder + master", n)
	}

	// Ten route VCLs in one pass: 4 from the burst, 6 waits of 500 ms.
	r.setRouteConfigMap(routeConfigMap(docs))
	mustReconcile(t, r)
	if len(f.sleeps) != 6 {
		t.Fatalf("sleeps = %v, want 6", f.sleeps)
	}
	for _, d := range f.sleeps {
		if d != 500*time.Millisecond {
			t.Errorf("sleep %v, want 500ms", d)
		}
	}
	for i := range 10 {
		if res := r.SlotResults()[fmt.Sprintf("default/p%d", i)]; !res.Programmed {
			t.Errorf("slot %d not programmed: %+v", i, res)
		}
	}

	// A master change recompiles the master without waiting.
	f.sleeps = nil
	r.setVCLConfigMap(vclConfigMap(10, "vcl 4.1;\nsub vcl_deliver { set resp.http.X-M = \"2\"; }\n"))
	mustReconcile(t, r)
	if len(f.sleeps) != 0 {
		t.Errorf("master recompile throttled: %v", f.sleeps)
	}
}

func TestReconcile_ThrottleHonoursContext(t *testing.T) {
	r, _ := newTestReloader(t)
	r.SetCompileThrottle(1, 1)
	r.setVCLConfigMap(vclConfigMap(4, ""))
	r.setRouteConfigMap(routeConfigMap(map[int]string{0: routeDoc("a"), 1: routeDoc("b")}))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := r.reconcile(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reconcile err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 700*time.Millisecond {
		t.Errorf("reconcile ignored the context for %v", time.Since(start))
	}
}

// routeManifestWithRoutes is routeConfigMap's manifest with the route recorded
// ("default/r<slot>").
func routeManifestWithRoutes(docs map[int]string) string {
	var parts []string
	for slot, doc := range docs {
		parts = append(parts, fmt.Sprintf(`"%d":{"policy":"default/p%d","route":"default/r%d","generation":1,"hash":%q}`,
			slot, slot, slot, RouteVCLHash(doc)))
	}
	return `{"slots":{` + strings.Join(parts, ",") + `}}`
}

func lastSlotsEvent(bus *dashboard.EventBus) (dashboard.RouteVCLState, int) {
	var st dashboard.RouteVCLState
	n := 0
	for _, ev := range bus.Recent() {
		if s, ok := ev.RouteVCLState(); ok {
			st = s
			n++
		}
	}
	return st, n
}

func TestReconcile_PublishesSlotSnapshot(t *testing.T) {
	r, mock := newTestReloader(t)
	mock.SetVCLCompileError("BROKEN", vccError)
	bus := dashboard.NewEventBus(64)
	r.SetDashboard(bus)

	r.setVCLConfigMap(vclConfigMap(4, ""))
	good, bad := routeDoc("good"), routeDoc("BROKEN")
	cm := routeConfigMap(map[int]string{1: good, 2: bad})
	cm.Data[ManifestKey] = routeManifestWithRoutes(map[int]string{1: good, 2: bad})
	r.setRouteConfigMap(cm)
	mustReconcile(t, r)

	st, n := lastSlotsEvent(bus)
	if n != 1 {
		t.Fatalf("snapshot events = %d, want 1", n)
	}
	if st.PoolSize != 4 || !strings.HasPrefix(st.MasterVCL, "vcl_") || len(st.Slots) != 2 {
		t.Fatalf("snapshot %+v", st)
	}
	s1, s2 := st.Slots[0], st.Slots[1]
	if s1.Slot != 1 || s1.Policy != "default/p1" || s1.Route != "default/r1" ||
		s1.State != dashboard.RouteVCLProgrammed || s1.VCLName != routeVCLName(1, RouteVCLHash(good)) || s1.Message != "" {
		t.Errorf("slot 1: %+v", s1)
	}
	if s2.Slot != 2 || s2.State != dashboard.RouteVCLFailed || !strings.Contains(s2.Message, "VCC-compiler") {
		t.Errorf("slot 2: %+v", s2)
	}
	if p, f, ph := st.Counts(); p != 1 || f != 1 || ph != 2 {
		t.Errorf("counts = %d/%d/%d, want 1/1/2", p, f, ph)
	}

	// An unchanged pass publishes nothing.
	mustReconcile(t, r)
	if _, n := lastSlotsEvent(bus); n != 1 {
		t.Errorf("snapshot events after no-op pass = %d, want 1", n)
	}

	// Fixing slot 2 publishes a new snapshot.
	fixed := routeDoc("fixed")
	cm = routeConfigMap(map[int]string{1: good, 2: fixed})
	cm.Data[ManifestKey] = routeManifestWithRoutes(map[int]string{1: good, 2: fixed})
	r.setRouteConfigMap(cm)
	mustReconcile(t, r)
	st, n = lastSlotsEvent(bus)
	if n != 2 {
		t.Fatalf("snapshot events = %d, want 2", n)
	}
	if p, f, ph := st.Counts(); p != 2 || f != 0 || ph != 2 {
		t.Errorf("counts after fix = %d/%d/%d, want 2/0/2", p, f, ph)
	}
}

// Route VCL loads publish route events, not master ones.
func TestReconcile_RouteLoadsUseRouteEvents(t *testing.T) {
	r, mock := newTestReloader(t)
	mock.SetVCLCompileError("BROKEN", vccError)
	bus := dashboard.NewEventBus(64)
	r.SetDashboard(bus)
	r.setVCLConfigMap(vclConfigMap(2, ""))
	r.setRouteConfigMap(routeConfigMap(map[int]string{0: routeDoc("ok"), 1: routeDoc("BROKEN")}))
	mustReconcile(t, r)

	count := map[dashboard.EventType]int{}
	var failResult string
	for _, ev := range bus.Recent() {
		count[ev.Type]++
		if ev.Type == dashboard.EventRouteVCLReloadFail {
			failResult = ev.Data["result"]
		}
	}
	if count[dashboard.EventVCLReload] != 1 || count[dashboard.EventVCLReloadFail] != 0 {
		t.Errorf("master events: %v", count)
	}
	if count[dashboard.EventRouteVCLReload] != 1 || count[dashboard.EventRouteVCLReloadFail] != 1 || failResult != dashboard.RouteVCLCompileError {
		t.Errorf("route events: %v result=%q", count, failResult)
	}
}
