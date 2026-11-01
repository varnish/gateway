package vcl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/varnish/gateway/internal/dashboard"
	"github.com/varnish/gateway/internal/varnishadm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// The Reloader converges varnishd's set of loaded VCLs on what the gateway's
// ConfigMaps describe:
//
//	gwvcl-placeholder   GeneratePlaceholder(), loaded while the pool is > 0
//	gwvcl-0..N-1        one label per slot: a route VCL or the placeholder
//	route_<slot>_<hash> route VCLs, each behind exactly one label
//	vcl_<timestamp>     masters; the newest is active
//
// Both ConfigMap informers only record the latest object and nudge a channel.
// A single goroutine runs reconcile(), which reads `vcl.list -j` (the source
// of truth, not memory) and issues the CLI commands that close the gap, so
// no two VCL command sequences ever interleave.
//
// VCL compilation failure is intentionally non-fatal. A master that fails to
// compile leaves the previous master active; a route VCL that fails leaves its
// label where it was (the previous route VCL or the placeholder).

const (
	// DefaultKeepCount is the number of inactive masters kept after a master
	// reload. It is small on purpose: every old master keeps its references
	// to the slot labels, and a referenced label cannot be discarded, so old
	// masters would block shrinking the pool. Discarding a master that still
	// has requests in flight is safe: varnishd finishes them and frees it.
	// When the pool shrinks, all inactive masters are discarded.
	DefaultKeepCount = 2

	// vclPrefix is the prefix for master VCL names.
	vclPrefix = "vcl_"

	// vclStatusDiscarded is the vcl.list status of a VCL that was discarded
	// but is still referenced (busy) in the child.
	vclStatusDiscarded = "discarded"

	// maxPoolSize is a sanity cap on PoolSizeKey. The operator caps
	// GatewayClassParameters.spec.routeVCL.slotPoolSize at 256; this only
	// stops a corrupt value from creating labels without bound.
	maxPoolSize = 1024

	// maxResultMessage caps the CLI output recorded for a failed slot.
	maxResultMessage = 2048

	// failureMemoTTL is how long a VCC compile failure (route VCL or master)
	// is remembered before the same document is compiled again.
	failureMemoTTL = 5 * time.Minute

	resyncPeriod      = 30 * time.Second
	retryInitialDelay = 5 * time.Second
	retryMaxDelay     = 30 * time.Second
)

// failureMemo is a remembered VCC compile failure.
type failureMemo struct {
	msg string    // CLI output (trimmed) of the failed vcl.inline
	at  time.Time // when it failed
}

// masterFailureMemo is a remembered VCC compile failure of a master.
type masterFailureMemo struct {
	content string // main.vcl that failed
	err     error
	at      time.Time
}

// SlotResult is the outcome of the last reconcile for one manifest slot.
type SlotResult struct {
	Policy     string // "<namespace>/<name>" from the manifest
	Slot       int    // -1 when the manifest key is not a slot number
	Hash       string // manifest hash the result refers to
	Programmed bool
	Message    string
}

// Reloader watches the gateway's ConfigMaps and keeps varnishd's VCLs in sync.
type Reloader struct {
	varnishadm varnishadm.VarnishadmInterface
	keepCount  int
	logger     *slog.Logger

	kubeClient         kubernetes.Interface
	configMapName      string
	configMapNamespace string
	routeConfigMapName string // empty: route VCL ConfigMap not watched

	reporter *statusReporter // nil: no VarnishVCLPolicy status writes

	// Latest ConfigMap snapshots, written by the informers.
	snapMu  sync.Mutex
	vclCM   *corev1.ConfigMap
	routeCM *corev1.ConfigMap
	nudge   chan struct{}

	// Delays before retrying a failed reconcile (doubling from the first).
	retryInitial time.Duration
	retryMax     time.Duration

	// State below is owned by the reconcile goroutine.
	appliedMaster    string // main.vcl content of the active master
	activeMasterPool int    // pool size the active master was compiled for; 0 = none applied
	maxVCLSet        int    // last max_vcl value set, 0 = never

	// Failure memo. Only genuine VCC compile failures are remembered (they
	// cannot succeed on retry); anything else is retried with the backoff.
	// Entries expire after memoTTL. The route VCL memo is cleared whenever
	// the applied master or the pool size changes: a document that returns
	// to another slot's label can start compiling after a grow.
	now             func() time.Time
	memoTTL         time.Duration
	compileFailures map[string]failureMemo // route VCL base name -> its VCC failure
	memoPool        int                    // pool size compileFailures was built for
	masterFailure   *masterFailureMemo     // last master that failed VCC, nil = none

	resultsMu sync.Mutex
	results   map[string]SlotResult // keyed by policy

	// vclSeq is a monotonic counter appended to generated VCL names so two
	// reloads within the same millisecond can't collide on the same name.
	vclSeq atomic.Uint64

	// firstLoadDone is closed after the first master is active.
	firstLoadDone chan struct{}
	firstLoadOnce sync.Once
	firstLoadAt   time.Time

	// slotsDone is closed after the first full slot pass that follows the
	// first master load.
	slotsDone chan struct{}
	slotsOnce sync.Once

	// Dashboard integration (optional)
	dashBus *dashboard.EventBus
}

// New creates a VCL reloader for the "{gateway}-vcl" ConfigMap configMapName.
func New(
	v varnishadm.VarnishadmInterface,
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
		keepCount:          keepCount,
		kubeClient:         kubeClient,
		configMapName:      configMapName,
		configMapNamespace: configMapNamespace,
		logger:             logger,
		nudge:              make(chan struct{}, 1),
		retryInitial:       retryInitialDelay,
		retryMax:           retryMaxDelay,
		now:                time.Now,
		memoTTL:            failureMemoTTL,
		compileFailures:    make(map[string]failureMemo),
		firstLoadDone:      make(chan struct{}),
		slotsDone:          make(chan struct{}),
	}
}

// SetRouteVCLConfigMap sets the "{gateway}-route-vcl" ConfigMap to watch.
// Empty (the default) never watches one: every slot is empty.
func (r *Reloader) SetRouteVCLConfigMap(name string) {
	r.routeConfigMapName = name
}

// SetStatusWriter enables writing this pod's per-slot results into
// VarnishVCLPolicy status.podResults.
func (r *Reloader) SetStatusWriter(dyn dynamic.Interface, podName string) {
	if dyn == nil || podName == "" {
		r.reporter = nil
		return
	}
	r.reporter = newStatusReporter(dyn, podName, r.logger)
}

// Ready returns a channel closed once the first master VCL is active.
func (r *Reloader) Ready() <-chan struct{} {
	return r.firstLoadDone
}

// SlotsReady returns a channel closed after the first full slot pass that
// follows the first master load: every programmed slot has its route VCL
// labelled (or has failed to compile). Closed right after Ready when the pool
// is 0 or there is no route VCL ConfigMap.
func (r *Reloader) SlotsReady() <-chan struct{} {
	return r.slotsDone
}

// SlotResults returns the per-policy results of the last slot pass.
func (r *Reloader) SlotResults() map[string]SlotResult {
	r.resultsMu.Lock()
	defer r.resultsMu.Unlock()
	return maps.Clone(r.results)
}

// SetDashboard connects the reloader to the dashboard event bus.
func (r *Reloader) SetDashboard(bus *dashboard.EventBus) {
	r.dashBus = bus
}

// Run starts the ConfigMap informers and the reconcile loop. It blocks until
// the context is cancelled.
func (r *Reloader) Run(ctx context.Context) error {
	r.logger.Debug("VCL reloader started",
		"configMapName", r.configMapName,
		"routeVCLConfigMapName", r.routeConfigMapName,
		"namespace", r.configMapNamespace,
		"keepCount", r.keepCount)

	synced := []cache.InformerSynced{}
	hasSynced, err := r.watchConfigMap(ctx, r.configMapName, r.setVCLConfigMap)
	if err != nil {
		return err
	}
	synced = append(synced, hasSynced)
	if r.routeConfigMapName != "" {
		hasSynced, err := r.watchConfigMap(ctx, r.routeConfigMapName, r.setRouteConfigMap)
		if err != nil {
			return err
		}
		synced = append(synced, hasSynced)
	}
	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		return fmt.Errorf("failed to sync ConfigMap cache")
	}

	if r.reporter != nil {
		go r.reporter.run(ctx)
	}

	r.logger.Info("VCL reloader ready")
	r.loop(ctx)
	r.logger.Info("VCL reloader stopping")
	return ctx.Err()
}

// watchConfigMap starts an informer restricted to one ConfigMap by a field
// selector, so the informer doesn't cache every ConfigMap in the namespace.
// set receives the object on add/update and nil on delete.
func (r *Reloader) watchConfigMap(ctx context.Context, name string, set func(*corev1.ConfigMap)) (cache.InformerSynced, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(
		r.kubeClient,
		resyncPeriod,
		informers.WithNamespace(r.configMapNamespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = "metadata.name=" + name
		}),
	)
	informer := factory.Core().V1().ConfigMaps().Informer()
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == name {
				set(cm)
			}
		},
		UpdateFunc: func(_, obj any) {
			if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == name {
				set(cm)
			}
		},
		DeleteFunc: func(obj any) {
			if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == name {
				set(nil)
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("informer.AddEventHandler(%s): %w", name, err)
	}
	factory.Start(ctx.Done())
	return informer.HasSynced, nil
}

// setVCLConfigMap records the latest "{gateway}-vcl" ConfigMap. A delete is
// ignored: varnishd keeps serving what it has.
func (r *Reloader) setVCLConfigMap(cm *corev1.ConfigMap) {
	if cm == nil {
		r.logger.Warn("VCL ConfigMap deleted, keeping the loaded VCL", "name", r.configMapName)
		return
	}
	r.snapMu.Lock()
	r.vclCM = cm
	r.snapMu.Unlock()
	r.poke()
}

// setRouteConfigMap records the latest "{gateway}-route-vcl" ConfigMap; nil
// means it does not exist (an empty manifest).
func (r *Reloader) setRouteConfigMap(cm *corev1.ConfigMap) {
	r.snapMu.Lock()
	r.routeCM = cm
	r.snapMu.Unlock()
	r.poke()
}

func (r *Reloader) poke() {
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

// loop runs reconcile on every nudge. A failed reconcile is retried after a
// growing delay (5s doubling to 30s) in addition to the next event or resync.
func (r *Reloader) loop(ctx context.Context) {
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	var delay time.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.nudge:
		case <-retry.C:
		}
		if err := r.reconcile(ctx); err != nil {
			if delay == 0 {
				delay = r.retryInitial
			} else {
				delay = min(2*delay, r.retryMax)
			}
			r.logger.Error("VCL reconcile failed", "error", err, "retryIn", delay)
			retry.Reset(delay)
			continue
		}
		delay = 0
		retry.Stop()
	}
}

// reconcile converges varnishd on the latest ConfigMap snapshots.
func (r *Reloader) reconcile(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.snapMu.Lock()
	vclCM, routeCM := r.vclCM, r.routeCM
	r.snapMu.Unlock()
	if vclCM == nil {
		return nil
	}
	master, ok := vclCM.Data[MainVCLKey]
	if !ok {
		r.logger.Warn("ConfigMap missing key", "name", vclCM.Name, "key", MainVCLKey)
		return nil
	}
	pool, err := ParsePoolSize(vclCM.Data[PoolSizeKey])
	if err != nil {
		// An operator bug; retrying the same ConfigMap cannot help.
		if r.appliedMaster != "" {
			r.logger.Error("invalid route VCL pool size, ignoring ConfigMap", "name", vclCM.Name, "error", err)
			return nil
		}
		// Nothing loaded yet: bring the gateway up rather than hang the
		// first load. If main.vcl was generated for a pool, it fails to
		// compile (its slot labels do not exist) and that is reported.
		r.logger.Error("invalid route VCL pool size and no VCL loaded yet, loading main.vcl with route VCL disabled",
			"name", vclCM.Name, "error", err)
		pool = 0
	}
	if pool != r.memoPool {
		clear(r.compileFailures)
		r.memoPool = pool
	}

	st, err := r.list()
	if err != nil {
		return err
	}

	// 1. Placeholder and labels must exist before a master referencing them
	// compiles.
	if pool > 0 {
		if err := r.ensurePool(st, pool); err != nil {
			return err
		}
	}

	// 2. Master. A failing master leaves the previous one active; the slots
	// that master dispatches to are still reconciled (a broken gateway VCL
	// must not freeze route VCL changes), and the error is returned at the
	// end so the backoff retry still happens.
	var masterErr error
	if master != r.appliedMaster || !strings.HasPrefix(st.active, vclPrefix) {
		masterErr = r.applyMaster(master, pool)
		if masterErr != nil && !strings.HasPrefix(st.active, vclPrefix) {
			return masterErr // no master at all: nothing dispatches to slots
		}
	}
	if st, err = r.list(); err != nil {
		return err
	}

	slotPool := pool
	if masterErr != nil {
		// Only slots both the desired and the active master know about.
		// Old masters and labels >= pool stay until a master for pool
		// is active.
		slotPool = min(pool, r.activeMasterPool)
	} else {
		// 3. Old masters. When the pool shrank they pin labels >= pool, so
		// all go.
		keep := r.keepCount
		if len(st.slotLabelsFrom(pool)) > 0 {
			keep = 0
		}
		if r.discardOldMasters(st, keep) {
			if st, err = r.list(); err != nil {
				return err
			}
		}

		// 4. Shrink.
		if r.shrink(st, pool) {
			if st, err = r.list(); err != nil {
				return err
			}
		}
	}

	// 5. Slots.
	pass, err := r.reconcileSlots(st, pool, slotPool, routeCM)
	if err != nil {
		return errors.Join(masterErr, err)
	}
	if err := r.discardOrphans(pass.wanted); err != nil {
		return errors.Join(masterErr, err)
	}
	if pass.results != nil {
		r.resultsMu.Lock()
		r.results = pass.results
		r.resultsMu.Unlock()
		if r.reporter != nil {
			r.reporter.update(pass.results)
		}
	}
	if isClosed(r.firstLoadDone) {
		r.slotsOnce.Do(func() {
			programmed := 0
			for _, res := range pass.results {
				if res.Programmed {
					programmed++
				}
			}
			r.logger.Info("initial route VCL slot pass complete",
				"poolSize", pool, "slots", len(pass.results), "programmed", programmed,
				"duration", time.Since(r.firstLoadAt).Round(time.Millisecond))
			close(r.slotsDone)
		})
	}
	return errors.Join(masterErr, pass.retry)
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// vclState indexes one `vcl.list -j` reply.
//
// A VCL discarded while requests (or idle keep-alive sessions) still hold it
// stays listed with status "discarded" until varnishd frees it. Such entries
// are left out of entries and order (they are gone as far as reconciling is
// concerned) but their names stay taken in names.
type vclState struct {
	entries map[string]varnishadm.VCLEntry
	order   []string        // live names in list order
	names   map[string]bool // every listed name, discarded ones included
	active  string
}

func (r *Reloader) list() (*vclState, error) {
	entries, err := r.varnishadm.VCLListJSON()
	if err != nil {
		return nil, fmt.Errorf("varnishadm.VCLListJSON(): %w", err)
	}
	st := &vclState{
		entries: make(map[string]varnishadm.VCLEntry, len(entries)),
		names:   make(map[string]bool, len(entries)),
	}
	for _, e := range entries {
		st.names[e.Name] = true
		if e.Status == vclStatusDiscarded {
			continue
		}
		st.entries[e.Name] = e
		st.order = append(st.order, e.Name)
		if e.Status == "active" {
			st.active = e.Name
		}
	}
	return st, nil
}

func (s *vclState) has(name string) bool {
	_, ok := s.entries[name]
	return ok
}

// loadedAs returns a live VCL loaded for base (see matchesRouteVCL), or "".
func (s *vclState) loadedAs(base string) string {
	for _, name := range s.order {
		if e := s.entries[name]; !e.IsLabel() && matchesRouteVCL(name, base) {
			return name
		}
	}
	return ""
}

// freeName returns base, or base_<k> for the smallest k >= 2 whose name is
// not taken. A route VCL discarded while still busy keeps its name until
// varnishd frees it, so going back to an earlier document soon after needs
// a different name.
func (s *vclState) freeName(base string) string {
	if !s.names[base] {
		return base
	}
	for k := 2; ; k++ {
		if name := base + "_" + strconv.Itoa(k); !s.names[name] {
			return name
		}
	}
}

// matchesRouteVCL reports whether name is the route VCL base (route_<slot>_<hash>)
// or one of its renamed loads (base_<k>).
func matchesRouteVCL(name, base string) bool {
	if name == base {
		return true
	}
	rest, ok := strings.CutPrefix(name, base+"_")
	if !ok || rest == "" {
		return false
	}
	_, err := strconv.Atoi(rest)
	return err == nil
}

// isPlaceholderName reports whether name is the placeholder VCL or a renamed
// load of it (gwvcl-placeholder_<k>, used while a discarded-but-busy
// placeholder still holds the name).
func isPlaceholderName(name string) bool {
	return matchesRouteVCL(name, PlaceholderVCLName)
}

// placeholder returns the live placeholder VCL's name, or "".
func (s *vclState) placeholder() string {
	for _, name := range s.order {
		if !s.entries[name].IsLabel() && isPlaceholderName(name) {
			return name
		}
	}
	return ""
}

// isCompileFailure reports whether a failed vcl.inline reply is a VCC
// compile error, which the same document can never get past. varnishd
// reports those as "Message from VCC-compiler: ... VCL compilation failed".
// Everything else (max_vcl refusal, out of memory, a child-side failure) may
// succeed later.
func isCompileFailure(payload string) bool {
	return strings.Contains(payload, "VCC-compiler") || strings.Contains(payload, "VCL compilation failed")
}

// labelTarget returns the VCL a label points at, or "" if there is no such
// label.
func (s *vclState) labelTarget(label string) string {
	if e, ok := s.entries[label]; ok && e.IsLabel() {
		return e.LabelTarget
	}
	return ""
}

// slotLabelsFrom returns the slot numbers >= from that have a label, sorted.
func (s *vclState) slotLabelsFrom(from int) []int {
	var out []int
	for _, name := range s.order {
		if !s.entries[name].IsLabel() {
			continue
		}
		if n, ok := parseSlotLabel(name); ok && n >= from {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// cliErr turns a CLI call result into an error: transport errors and non-200
// replies alike.
func cliErr(op string, resp varnishadm.VarnishResponse, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if resp.StatusCode() != varnishadm.ClisOk {
		return fmt.Errorf("%s: status %d: %s", op, resp.StatusCode(), strings.TrimSpace(resp.Payload()))
	}
	return nil
}

// ensurePool sets max_vcl, loads the placeholder and creates missing slot
// labels pointing at it. Existing labels are never moved here: they may hold
// a route VCL.
func (r *Reloader) ensurePool(st *vclState, pool int) error {
	// Route VCLs count against max_vcl (labels do not); replaced generations
	// linger until discarded, hence 2x plus headroom for masters.
	if want := 2*pool + 10; r.maxVCLSet != want {
		if _, err := r.varnishadm.ParamSet("max_vcl", strconv.Itoa(want)); err != nil {
			return fmt.Errorf("varnishadm.ParamSet(max_vcl, %d): %w", want, err)
		}
		r.maxVCLSet = want
	}
	ph := st.placeholder()
	if ph == "" {
		// A placeholder discarded while busy keeps its name: load under a
		// free one, like route VCLs.
		ph = st.freeName(PlaceholderVCLName)
		resp, err := r.varnishadm.VCLInline(ph, GeneratePlaceholder())
		if err := cliErr("vcl.inline "+ph, resp, err); err != nil {
			return err
		}
		r.logger.Info("route VCL placeholder loaded", "name", ph)
	}
	created := 0
	for i := range pool {
		label := SlotLabel(i)
		if st.labelTarget(label) != "" {
			continue
		}
		resp, err := r.varnishadm.VCLLabel(label, ph)
		if err := cliErr("vcl.label "+label, resp, err); err != nil {
			return err
		}
		created++
	}
	if created > 0 {
		r.logger.Info("route VCL slot labels created", "created", created, "poolSize", pool)
	}
	return nil
}

// applyMaster loads master unless the same content failed to compile less
// than memoTTL ago.
func (r *Reloader) applyMaster(master string, pool int) error {
	if f := r.masterFailure; f != nil && f.content == master {
		if retryAt := f.at.Add(r.memoTTL); r.now().Before(retryAt) {
			return fmt.Errorf("master VCL failed to compile, not retried before %s or until main.vcl changes: %w",
				retryAt.Format(time.RFC3339), f.err)
		}
		r.masterFailure = nil
	}
	return r.loadMaster(master, pool)
}

// loadMaster compiles and activates a new master built for pool slots. On
// failure the previous master stays active; a VCC failure is memoised.
func (r *Reloader) loadMaster(master string, pool int) error {
	name := r.generateVCLName()
	r.logger.Debug("loading master VCL", "name", name)
	resp, err := r.varnishadm.VCLInline(name, master)
	if err != nil {
		return fmt.Errorf("varnishadm.VCLInline(%s): %w", name, err)
	}
	if err := resp.CheckOK("VCL compilation failed"); err != nil {
		r.logger.Error("VCL reload failed - keeping previous VCL", "name", name, "error", err)
		dashboard.PublishVCLReloadFail(r.dashBus, err)
		if isCompileFailure(resp.Payload()) {
			r.masterFailure = &masterFailureMemo{content: master, err: err, at: r.now()}
		}
		return err
	}
	resp, err = r.varnishadm.VCLUse(name)
	if err == nil {
		err = resp.CheckOK("VCL activation failed")
	}
	if err != nil {
		// Best-effort cleanup of the loaded-but-inactive VCL. If vcl.use
		// actually succeeded and only the reply was lost, the discard is
		// refused ("active") and the next reconcile sees the truth in
		// vcl.list.
		if _, dErr := r.varnishadm.VCLDiscard(name); dErr != nil {
			r.logger.Debug("VCL discard after activation error failed", "name", name, "error", dErr)
		}
		return fmt.Errorf("varnishadm.VCLUse(%s): %w", name, err)
	}
	r.appliedMaster = master
	r.activeMasterPool = pool
	r.masterFailure = nil
	clear(r.compileFailures)
	r.logger.Info("VCL reload complete", "name", name)
	dashboard.PublishVCLReload(r.dashBus, name)
	r.firstLoadOnce.Do(func() {
		r.firstLoadAt = time.Now()
		close(r.firstLoadDone)
	})
	return nil
}

// discardOldMasters discards inactive masters beyond the newest keep. It
// reports whether it discarded anything.
func (r *Reloader) discardOldMasters(st *vclState, keep int) bool {
	var masters []string
	for _, name := range st.order {
		e := st.entries[name]
		if e.IsLabel() || e.Status == "active" || !strings.HasPrefix(name, vclPrefix) {
			continue
		}
		masters = append(masters, name)
	}
	// Names embed a timestamp and sequence number, so they sort oldest first.
	sort.Strings(masters)
	changed := false
	for i := 0; i < len(masters)-keep; i++ {
		if r.discard(masters[i], slog.LevelWarn) {
			changed = true
		}
	}
	return changed
}

// shrink removes slot labels >= pool, and the placeholder when pool is 0. A
// label an old master still references cannot be discarded yet; that is
// logged and retried on the next reconcile. It reports whether it changed
// anything.
func (r *Reloader) shrink(st *vclState, pool int) bool {
	changed := false
	ph := st.placeholder()
	for _, slot := range st.slotLabelsFrom(pool) {
		label := SlotLabel(slot)
		target := st.labelTarget(label)
		routeTarget := strings.HasPrefix(target, routeVCLPrefix)
		if routeTarget && ph != "" {
			resp, err := r.varnishadm.VCLLabel(label, ph)
			if err := cliErr("vcl.label "+label, resp, err); err != nil {
				r.logger.Warn("cannot repoint removed slot to placeholder", "label", label, "error", err)
			} else {
				changed = true
				r.discard(target, slog.LevelWarn)
				routeTarget = false
			}
		}
		if r.discard(label, slog.LevelInfo) {
			changed = true
			if routeTarget {
				r.discard(target, slog.LevelWarn)
			}
		}
	}
	if pool == 0 && ph != "" {
		// Re-check: labels discarded above no longer point at it.
		if st2, err := r.list(); err == nil {
			for _, name := range st2.order {
				if e := st2.entries[name]; e.IsLabel() || e.Labels > 0 || !isPlaceholderName(name) {
					continue
				}
				if r.discard(name, slog.LevelInfo) {
					changed = true
					r.logger.Info("route VCL disabled, placeholder discarded", "name", name)
				}
			}
		}
	}
	if changed {
		r.logger.Info("route VCL pool shrunk", "poolSize", pool)
	}
	return changed
}

// discard runs vcl.discard and logs a refusal at the given level. It reports
// whether the VCL is gone.
func (r *Reloader) discard(name string, refusalLevel slog.Level) bool {
	resp, err := r.varnishadm.VCLDiscard(name)
	if err := cliErr("vcl.discard "+name, resp, err); err != nil {
		r.logger.Log(context.Background(), refusalLevel, "VCL discard refused, will retry on a later reconcile", "name", name, "error", err)
		return false
	}
	r.logger.Debug("VCL discarded", "name", name)
	return true
}

// desiredSlot is a valid manifest entry for a slot inside the pool.
type desiredSlot struct {
	policy string
	hash   string
	doc    string
}

// slotPass is the outcome of one reconcileSlots run.
type slotPass struct {
	// results are the per-policy results, nil when the manifest cannot be
	// read (then nothing was changed).
	results map[string]SlotResult
	// wanted holds the route VCL base names of the desired documents. A
	// loaded route VCL for one of them is kept even while unlabelled (its
	// label is retried).
	wanted map[string]bool
	// retry joins the failures worth retrying with the backoff: a
	// vcl.inline refused for a reason other than VCC, a refused vcl.label.
	retry error
}

// resultSet collects per-policy results. One slot per policy per gateway is
// the contract (the operator enforces it); should a manifest still list a
// policy for two slots, the better result is kept: programmed over valid but
// failed over invalid (ties keep the first).
type resultSet struct {
	m    map[string]SlotResult
	rank map[string]int
}

func (s *resultSet) set(res SlotResult, valid bool) {
	if res.Policy == "" {
		return
	}
	rank := 0
	if valid {
		rank = 1
	}
	if res.Programmed {
		rank = 2
	}
	if old, ok := s.rank[res.Policy]; ok && old >= rank {
		return
	}
	s.m[res.Policy] = res
	s.rank[res.Policy] = rank
}

// reconcileSlots points the labels of slots below limit at their desired
// route VCL, or the placeholder. limit is pool, or less while the master for
// pool failed to load (slots from limit on are reported as waiting). A
// non-nil error is a CLI transport failure; the reconcile is retried.
func (r *Reloader) reconcileSlots(st *vclState, pool, limit int, routeCM *corev1.ConfigMap) (*slotPass, error) {
	results := &resultSet{m: make(map[string]SlotResult), rank: make(map[string]int)}
	desired := make(map[int]desiredSlot)
	if routeCM != nil {
		manifest, err := ParseRouteVCLManifest(routeCM.Data[ManifestKey])
		if err != nil {
			r.logger.Error("invalid route VCL manifest, leaving slots unchanged", "name", routeCM.Name, "error", err)
			return &slotPass{}, nil
		}
		slotsOf := make(map[string][]string)
		for _, key := range slices.Sorted(maps.Keys(manifest.Slots)) {
			entry := manifest.Slots[key]
			if entry.Policy == "" {
				r.logger.Warn("route VCL manifest slot without policy", "slot", key)
			} else {
				slotsOf[entry.Policy] = append(slotsOf[entry.Policy], key)
			}
			res, d, ok := validateSlot(key, entry, pool, routeCM.Data)
			switch {
			case !ok:
				results.set(res, false)
			case res.Slot >= limit:
				res.Message = fmt.Sprintf("slot %d waits for the master VCL for a pool of %d slots, which failed to load", res.Slot, pool)
				results.set(res, true)
			default:
				desired[res.Slot] = d
			}
		}
		for policy, keys := range slotsOf {
			if len(keys) > 1 {
				r.logger.Warn("policy listed for more than one slot in the route VCL manifest; one slot per policy per gateway is the contract, reporting the best result",
					"policy", policy, "slots", keys)
			}
		}
	}

	ph := st.placeholder()
	if ph == "" && limit > 0 {
		return nil, errors.New("route VCL placeholder is not loaded")
	}
	wanted := make(map[string]bool)
	var retry []error
	for slot := range limit {
		label := SlotLabel(slot)
		cur := st.labelTarget(label)
		d, ok := desired[slot]
		if !ok {
			if isPlaceholderName(cur) {
				continue
			}
			resp, err := r.varnishadm.VCLLabel(label, ph)
			if err := cliErr("vcl.label "+label, resp, err); err != nil {
				return nil, err
			}
			r.logger.Info("route VCL slot cleared", "slot", slot, "previous", cur)
			if strings.HasPrefix(cur, routeVCLPrefix) {
				r.discard(cur, slog.LevelWarn)
			}
			continue
		}

		base := routeVCLName(slot, d.hash)
		wanted[base] = true
		res := SlotResult{Policy: d.policy, Slot: slot, Hash: d.hash}
		if matchesRouteVCL(cur, base) {
			res.Programmed = true
			results.set(res, true)
			continue
		}
		if msg, failed := r.memoisedFailure(base); failed {
			res.Message = msg
			results.set(res, true)
			continue
		}
		name := st.loadedAs(base)
		if name == "" {
			name = st.freeName(base)
			start := time.Now()
			resp, err := r.varnishadm.VCLInline(name, d.doc)
			if err != nil {
				return nil, fmt.Errorf("varnishadm.VCLInline(%s): %w", name, err)
			}
			if resp.StatusCode() != varnishadm.ClisOk {
				res.Message = trimMessage(resp.Payload())
				results.set(res, true)
				failErr := fmt.Errorf("route VCL %s (slot %d, %s): %s", name, slot, d.policy, res.Message)
				if isCompileFailure(resp.Payload()) {
					r.compileFailures[base] = failureMemo{msg: res.Message, at: r.now()}
					r.logger.Warn("route VCL failed to compile, slot keeps its previous VCL",
						"slot", slot, "name", name, "policy", d.policy, "keeping", cur, "status", resp.StatusCode())
				} else {
					retry = append(retry, failErr)
					r.logger.Warn("route VCL could not be loaded, slot keeps its previous VCL, will retry",
						"slot", slot, "name", name, "policy", d.policy, "keeping", cur, "status", resp.StatusCode(), "error", res.Message)
				}
				dashboard.PublishVCLReloadFail(r.dashBus, failErr)
				continue
			}
			r.logger.Info("route VCL compiled", "slot", slot, "name", name, "policy", d.policy,
				"duration", time.Since(start).Round(time.Millisecond))
		}
		resp, err := r.varnishadm.VCLLabel(label, name)
		if err != nil {
			return nil, fmt.Errorf("varnishadm.VCLLabel(%s, %s): %w", label, name, err)
		}
		if resp.StatusCode() != varnishadm.ClisOk {
			// E.g. "would create a loop": the document returns to a label
			// that leads back here. That depends on other slots' documents,
			// so it is not memoised: the compiled VCL stays loaded and the
			// label is retried on every pass.
			res.Message = trimMessage(resp.Payload())
			results.set(res, true)
			failErr := fmt.Errorf("route VCL %s (slot %d, %s): vcl.label %s: %s", name, slot, d.policy, label, res.Message)
			retry = append(retry, failErr)
			r.logger.Warn("route VCL could not be labelled, slot keeps its previous VCL, will retry",
				"slot", slot, "name", name, "policy", d.policy, "error", res.Message)
			dashboard.PublishVCLReloadFail(r.dashBus, failErr)
			continue
		}
		res.Programmed = true
		results.set(res, true)
		r.logger.Info("route VCL slot programmed", "slot", slot, "name", name, "policy", d.policy, "previous", cur)
		dashboard.PublishVCLReload(r.dashBus, name)
		if strings.HasPrefix(cur, routeVCLPrefix) {
			r.discard(cur, slog.LevelWarn)
		}
	}
	// Forget failures of documents no longer wanted, so the map stays small.
	for name := range r.compileFailures {
		if !wanted[name] {
			delete(r.compileFailures, name)
		}
	}
	return &slotPass{results: results.m, wanted: wanted, retry: errors.Join(retry...)}, nil
}

// memoisedFailure returns the remembered VCC failure of a route VCL base
// name, unless it has expired.
func (r *Reloader) memoisedFailure(base string) (string, bool) {
	f, ok := r.compileFailures[base]
	if !ok {
		return "", false
	}
	if !r.now().Before(f.at.Add(r.memoTTL)) {
		delete(r.compileFailures, base)
		return "", false
	}
	return f.msg, true
}

// validateSlot checks one manifest entry. On success it returns the slot's
// desired document; otherwise the result explains why the slot is skipped.
func validateSlot(key string, entry RouteVCLSlot, pool int, data map[string]string) (SlotResult, desiredSlot, bool) {
	res := SlotResult{Policy: entry.Policy, Slot: -1, Hash: entry.Hash}
	slot, err := strconv.Atoi(key)
	if err != nil || slot < 0 || strconv.Itoa(slot) != key {
		res.Message = fmt.Sprintf("invalid slot %q in route VCL manifest", key)
		return res, desiredSlot{}, false
	}
	res.Slot = slot
	if slot >= pool {
		res.Message = fmt.Sprintf("slot %d is outside the pool of %d slots", slot, pool)
		return res, desiredSlot{}, false
	}
	if !isHexHash(entry.Hash) {
		res.Message = fmt.Sprintf("invalid hash %q for slot %d in route VCL manifest", entry.Hash, slot)
		return res, desiredSlot{}, false
	}
	doc, ok := data[SlotKey(slot)]
	if !ok {
		res.Message = fmt.Sprintf("route VCL ConfigMap has no key %s", SlotKey(slot))
		return res, desiredSlot{}, false
	}
	if got := RouteVCLHash(doc); got != entry.Hash {
		res.Message = fmt.Sprintf("hash of %s is %s, manifest says %s", SlotKey(slot), got, entry.Hash)
		return res, desiredSlot{}, false
	}
	return res, desiredSlot{policy: entry.Policy, hash: entry.Hash, doc: doc}, true
}

func isHexHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// trimMessage trims CLI output and caps it at maxResultMessage bytes.
func trimMessage(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxResultMessage {
		return s
	}
	const suffix = "\n... (truncated)"
	cut := maxResultMessage - len(suffix)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// discardOrphans discards route VCLs no label points at, except loads of a
// wanted document (their label is being retried). Normal operation never
// leaves any; this catches leftovers from interrupted reconciles.
func (r *Reloader) discardOrphans(wanted map[string]bool) error {
	st, err := r.list()
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range st.order {
		e := st.entries[name]
		if e.IsLabel() || e.Labels > 0 || e.Status == "active" || !strings.HasPrefix(name, routeVCLPrefix) {
			continue
		}
		if isWanted(name, wanted) {
			continue
		}
		r.logger.Info("discarding orphaned route VCL", "name", name)
		if !r.discard(name, slog.LevelWarn) {
			errs = append(errs, fmt.Errorf("orphaned route VCL %s could not be discarded", name))
		}
	}
	if len(errs) > 0 {
		// Not worth a retry storm: the next reconcile tries again.
		r.logger.Warn("orphan cleanup incomplete", "error", errors.Join(errs...))
	}
	return nil
}

// isWanted reports whether name is a load of one of the wanted base names.
func isWanted(name string, wanted map[string]bool) bool {
	for base := range wanted {
		if matchesRouteVCL(name, base) {
			return true
		}
	}
	return false
}

// generateVCLName creates a unique timestamped master VCL name. A monotonic
// sequence number guarantees uniqueness even when two reloads land in the
// same millisecond (the timestamp alone has only millisecond resolution).
// The sequence is zero-padded so names stay sortable.
func (r *Reloader) generateVCLName() string {
	now := time.Now()
	return fmt.Sprintf("%s%s_%03d_%06d",
		vclPrefix,
		now.Format("20060102_150405"),
		now.Nanosecond()/1e6, // milliseconds
		r.vclSeq.Add(1),
	)
}
