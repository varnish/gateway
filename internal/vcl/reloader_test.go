package vcl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/varnish/gateway/internal/varnishadm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// newTestReloader returns a Reloader on a strict mock (it refuses what
// varnishd refuses) with no kube client in use.
func newTestReloader(t *testing.T) (*Reloader, *varnishadm.MockVarnishadm) {
	t.Helper()
	mock := varnishadm.NewMock(6082, "secret", testLogger())
	mock.SetStrictVCL(true)
	r := New(mock, 0, fake.NewSimpleClientset(), "gw-vcl", "default", testLogger())
	return r, mock
}

func vclConfigMap(pool int, userVCL string) *corev1.ConfigMap {
	data := map[string]string{MainVCLKey: GenerateMaster(pool, userVCL)}
	if pool > 0 {
		data[PoolSizeKey] = fmt.Sprint(pool)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-vcl", Namespace: "default"},
		Data:       data,
	}
}

// routeConfigMap builds a "{gateway}-route-vcl" ConfigMap with one document
// per slot; policies are named "default/p<slot>".
func routeConfigMap(docs map[int]string) *corev1.ConfigMap {
	manifest := RouteVCLManifest{Slots: map[string]RouteVCLSlot{}}
	data := map[string]string{}
	for slot, doc := range docs {
		manifest.Slots[fmt.Sprint(slot)] = RouteVCLSlot{
			Policy:     fmt.Sprintf("default/p%d", slot),
			Generation: 1,
			Hash:       RouteVCLHash(doc),
		}
		data[SlotKey(slot)] = doc
	}
	data[ManifestKey] = mustJSON(manifest)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-route-vcl", Namespace: "default"},
		Data:       data,
	}
}

func mustJSON(m RouteVCLManifest) string {
	var sb strings.Builder
	sb.WriteString(`{"slots":{`)
	first := true
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range m.Slots {
			if !yield(k) {
				return
			}
		}
	}) {
		s := m.Slots[k]
		if !first {
			sb.WriteString(",")
		}
		first = false
		fmt.Fprintf(&sb, `%q:{"policy":%q,"generation":%d,"hash":%q}`, k, s.Policy, s.Generation, s.Hash)
	}
	sb.WriteString(`}}`)
	return sb.String()
}

func routeDoc(tag string) string {
	return GenerateRouteVCL("", "vcl 4.1;\nsub vcl_deliver {\n    set resp.http.X-Tag = \""+tag+"\";\n}\n")
}

func mustReconcile(t *testing.T, r *Reloader) {
	t.Helper()
	if err := r.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// commandNames reduces the call history to "verb arg1 [arg2]" lines, dropping
// inline VCL bodies.
func commandNames(history []string) []string {
	out := make([]string, 0, len(history))
	for _, cmd := range history {
		first, _, _ := strings.Cut(cmd, "\n")
		first = strings.TrimSuffix(first, " << EOF")
		out = append(out, first)
	}
	return out
}

func indexOf(t *testing.T, cmds []string, pred func(string) bool, what string) int {
	t.Helper()
	for i, c := range cmds {
		if pred(c) {
			return i
		}
	}
	t.Fatalf("no %s in history %v", what, cmds)
	return -1
}

func masters(vcls []string) []string {
	var out []string
	for _, v := range vcls {
		if strings.HasPrefix(v, vclPrefix) {
			out = append(out, v)
		}
	}
	return out
}

func routeVCLs(vcls []string) []string {
	var out []string
	for _, v := range vcls {
		if strings.HasPrefix(v, routeVCLPrefix) {
			out = append(out, v)
		}
	}
	return out
}

func TestReconcile_PoolZeroIsMasterOnly(t *testing.T) {
	r, mock := newTestReloader(t)
	r.setVCLConfigMap(vclConfigMap(0, ""))
	mustReconcile(t, r)

	cmds := commandNames(mock.GetCallHistory())
	for _, c := range cmds {
		if strings.HasPrefix(c, "param.set") || strings.HasPrefix(c, "vcl.label") || strings.Contains(c, PlaceholderVCLName) {
			t.Errorf("pool 0 issued %q; history %v", c, cmds)
		}
	}
	inline := indexOf(t, cmds, func(c string) bool { return strings.HasPrefix(c, "vcl.inline vcl_") }, "master vcl.inline")
	use := indexOf(t, cmds, func(c string) bool { return strings.HasPrefix(c, "vcl.use vcl_") }, "vcl.use")
	if use < inline {
		t.Errorf("vcl.use before vcl.inline: %v", cmds)
	}
	if !isClosed(r.Ready()) || !isClosed(r.SlotsReady()) {
		t.Error("Ready and SlotsReady must be closed after the first master load with pool 0")
	}
	labels, vcls, active := mock.VCLState()
	if len(labels) != 0 || len(vcls) != 1 || active != vcls[0] {
		t.Errorf("state labels=%v vcls=%v active=%q", labels, vcls, active)
	}

	// Same ConfigMap again (e.g. a resync): no new compile.
	mock.ClearCallHistory()
	r.setVCLConfigMap(vclConfigMap(0, ""))
	mustReconcile(t, r)
	for _, c := range commandNames(mock.GetCallHistory()) {
		if strings.HasPrefix(c, "vcl.inline") || strings.HasPrefix(c, "vcl.use") {
			t.Errorf("unchanged ConfigMap issued %q", c)
		}
	}

	// Changed content: reload.
	mock.ClearCallHistory()
	r.setVCLConfigMap(vclConfigMap(0, "vcl 4.1;\nsub vcl_deliver { set resp.http.X = \"1\"; }\n"))
	mustReconcile(t, r)
	indexOf(t, commandNames(mock.GetCallHistory()), func(c string) bool { return strings.HasPrefix(c, "vcl.inline vcl_") }, "reload")
}

func TestReconcile_BootstrapOrder(t *testing.T) {
	r, mock := newTestReloader(t)
	r.setVCLConfigMap(vclConfigMap(4, ""))
	mustReconcile(t, r)

	cmds := commandNames(mock.GetCallHistory())
	maxVCL := indexOf(t, cmds, func(c string) bool { return c == "param.set max_vcl 18" }, "param.set max_vcl 18")
	ph := indexOf(t, cmds, func(c string) bool { return c == "vcl.inline "+PlaceholderVCLName }, "placeholder load")
	last := ph
	for i := range 4 {
		want := fmt.Sprintf("vcl.label gwvcl-%d %s", i, PlaceholderVCLName)
		idx := indexOf(t, cmds, func(c string) bool { return c == want }, want)
		if idx < last {
			t.Errorf("%q out of order: %v", want, cmds)
		}
		last = idx
	}
	master := indexOf(t, cmds, func(c string) bool { return strings.HasPrefix(c, "vcl.inline vcl_") }, "master load")
	use := indexOf(t, cmds, func(c string) bool { return strings.HasPrefix(c, "vcl.use vcl_") }, "vcl.use")
	if !(maxVCL < ph && ph < last && last < master && master < use) {
		t.Errorf("bootstrap order wrong (max_vcl %d, placeholder %d, last label %d, master %d, use %d): %v",
			maxVCL, ph, last, master, use, cmds)
	}
	if !isClosed(r.Ready()) || !isClosed(r.SlotsReady()) {
		t.Error("Ready/SlotsReady not closed")
	}
	labels, _, _ := mock.VCLState()
	for i := range 4 {
		if labels[SlotLabel(i)] != PlaceholderVCLName {
			t.Errorf("%s -> %q", SlotLabel(i), labels[SlotLabel(i)])
		}
	}

	// A second reconcile changes nothing.
	mock.ClearCallHistory()
	mustReconcile(t, r)
	for _, c := range commandNames(mock.GetCallHistory()) {
		if c != "vcl.list -j" {
			t.Errorf("idle reconcile issued %q", c)
		}
	}
}

func TestReconcile_SlotAttachChangeDetach(t *testing.T) {
	r, mock := newTestReloader(t)
	r.setVCLConfigMap(vclConfigMap(4, ""))

	// Attach.
	v1 := routeDoc("v1")
	name1 := routeVCLName(1, RouteVCLHash(v1))
	r.setRouteConfigMap(routeConfigMap(map[int]string{1: v1}))
	mustReconcile(t, r)
	labels, _, _ := mock.VCLState()
	if labels["gwvcl-1"] != name1 {
		t.Fatalf("gwvcl-1 -> %q, want %q", labels["gwvcl-1"], name1)
	}
	if res := r.SlotResults()["default/p1"]; !res.Programmed || res.Slot != 1 || res.Hash != RouteVCLHash(v1) {
		t.Errorf("result %+v", res)
	}
	cmds := commandNames(mock.GetCallHistory())
	in := indexOf(t, cmds, func(c string) bool { return c == "vcl.inline "+name1 }, "route inline")
	lb := indexOf(t, cmds, func(c string) bool { return c == "vcl.label gwvcl-1 "+name1 }, "route label")
	use := indexOf(t, cmds, func(c string) bool { return strings.HasPrefix(c, "vcl.use vcl_") }, "vcl.use")
	if !(use < in && in < lb) {
		t.Errorf("slot programmed before master active or label before load: %v", cmds)
	}

	// Change the text.
	mock.ClearCallHistory()
	v2 := routeDoc("v2")
	name2 := routeVCLName(1, RouteVCLHash(v2))
	r.setRouteConfigMap(routeConfigMap(map[int]string{1: v2}))
	mustReconcile(t, r)
	cmds = commandNames(mock.GetCallHistory())
	want := []string{"vcl.inline " + name2, "vcl.label gwvcl-1 " + name2, "vcl.discard " + name1}
	var got []string
	for _, c := range cmds {
		if !strings.HasPrefix(c, "vcl.list") {
			got = append(got, c)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("change sequence = %v, want %v", got, want)
	}
	if strings.Contains(strings.Join(cmds, "|"), "vcl.use") {
		t.Error("route VCL change reloaded the master")
	}

	// Detach.
	mock.ClearCallHistory()
	r.setRouteConfigMap(routeConfigMap(nil))
	mustReconcile(t, r)
	got = nil
	for _, c := range commandNames(mock.GetCallHistory()) {
		if !strings.HasPrefix(c, "vcl.list") {
			got = append(got, c)
		}
	}
	want = []string{"vcl.label gwvcl-1 " + PlaceholderVCLName, "vcl.discard " + name2}
	if !slices.Equal(got, want) {
		t.Errorf("detach sequence = %v, want %v", got, want)
	}
	if len(r.SlotResults()) != 0 {
		t.Errorf("results after detach: %v", r.SlotResults())
	}

	// Route ConfigMap deleted: same as empty.
	r.setRouteConfigMap(routeConfigMap(map[int]string{2: v1}))
	mustReconcile(t, r)
	r.setRouteConfigMap(nil)
	mustReconcile(t, r)
	labels, vcls, _ := mock.VCLState()
	if labels["gwvcl-2"] != PlaceholderVCLName || len(routeVCLs(vcls)) != 0 {
		t.Errorf("after ConfigMap delete: labels=%v vcls=%v", labels, vcls)
	}
}

func TestReconcile_SlotMove(t *testing.T) {
	r, mock := newTestReloader(t)
	r.setVCLConfigMap(vclConfigMap(4, ""))
	doc := routeDoc("x")
	r.setRouteConfigMap(routeConfigMap(map[int]string{1: doc}))
	mustReconcile(t, r)
	r.setRouteConfigMap(routeConfigMap(map[int]string{2: doc}))
	mustReconcile(t, r)
	labels, vcls, _ := mock.VCLState()
	if labels["gwvcl-1"] != PlaceholderVCLName || labels["gwvcl-2"] != routeVCLName(2, RouteVCLHash(doc)) {
		t.Errorf("labels after move: %v", labels)
	}
	if rv := routeVCLs(vcls); len(rv) != 1 {
		t.Errorf("route VCLs after move: %v", rv)
	}
}

// A VCL discarded while busy stays listed as "discarded" and keeps its name:
// going back to that document must load it under a new name, and the
// lingering entry must not count as an old master or an orphan.
func TestReconcile_DiscardedBusyVCLs(t *testing.T) {
	r, mock := newTestReloader(t)
	r.setVCLConfigMap(vclConfigMap(2, ""))
	docA, docB := routeDoc("A"), routeDoc("B")
	nameA := routeVCLName(0, RouteVCLHash(docA))
	r.setRouteConfigMap(routeConfigMap(map[int]string{0: docA}))
	mustReconcile(t, r)
	_, _, firstMaster := mock.VCLState()
	mock.HoldVCL(nameA)
	mock.HoldVCL(firstMaster)

	r.setRouteConfigMap(routeConfigMap(map[int]string{0: docB}))
	mustReconcile(t, r)
	r.setRouteConfigMap(routeConfigMap(map[int]string{0: docA}))
	mustReconcile(t, r)
	labels, _, _ := mock.VCLState()
	if labels["gwvcl-0"] != nameA+"_2" {
		t.Errorf("gwvcl-0 -> %q, want %q (renamed load)", labels["gwvcl-0"], nameA+"_2")
	}
	if res := r.SlotResults()["default/p0"]; !res.Programmed {
		t.Errorf("result %+v", res)
	}

	// Master reloads: the held first master is discarded but lingers; the
	// keep count applies to live masters only and nothing retries the
	// lingering one.
	for i := range 3 {
		r.setVCLConfigMap(vclConfigMap(2, fmt.Sprintf("vcl 4.1;\nsub vcl_deliver { set resp.http.N = \"%d\"; }\n", i)))
		mustReconcile(t, r)
	}
	mock.ClearCallHistory()
	mustReconcile(t, r)
	for _, c := range commandNames(mock.GetCallHistory()) {
		if c != "vcl.list -j" {
			t.Errorf("idle reconcile with lingering discarded VCLs issued %q", c)
		}
	}
	mock.ReleaseVCL(nameA)
	mock.ReleaseVCL(firstMaster)
	_, vcls, _ := mock.VCLState()
	if got := len(masters(vcls)); got != 1+DefaultKeepCount {
		t.Errorf("masters = %v", masters(vcls))
	}
}

func TestReconcile_CompileFailureLeavesLabel(t *testing.T) {
	r, mock := newTestReloader(t)
	mock.SetVCLCompileError("BROKEN", "Message from VCC-compiler:\nExpected ';' got '}'\nRunning VCC-compiler failed, exited with 2\nVCL compilation failed")
	r.setVCLConfigMap(vclConfigMap(4, ""))

	good := routeDoc("good")
	bad := routeDoc("BROKEN")
	r.setRouteConfigMap(routeConfigMap(map[int]string{0: bad, 1: good}))
	mustReconcile(t, r)

	labels, vcls, _ := mock.VCLState()
	if labels["gwvcl-0"] != PlaceholderVCLName {
		t.Errorf("failed slot 0 label -> %q, want placeholder", labels["gwvcl-0"])
	}
	if labels["gwvcl-1"] != routeVCLName(1, RouteVCLHash(good)) {
		t.Errorf("slot 1 label -> %q", labels["gwvcl-1"])
	}
	res := r.SlotResults()["default/p0"]
	if res.Programmed || !strings.Contains(res.Message, "VCC-compiler") || res.Slot != 0 || res.Hash != RouteVCLHash(bad) {
		t.Errorf("slot 0 result %+v", res)
	}
	if !r.SlotResults()["default/p1"].Programmed {
		t.Error("slot 1 not programmed")
	}
	if !isClosed(r.SlotsReady()) {
		t.Error("a compile failure must not hold SlotsReady")
	}
	for _, v := range vcls {
		if v == routeVCLName(0, RouteVCLHash(bad)) {
			t.Error("failed route VCL is loaded")
		}
	}

	// Replace the working slot 1 VCL with a broken one: the old one keeps
	// serving.
	bad1 := routeDoc("BROKEN again")
	r.setRouteConfigMap(routeConfigMap(map[int]string{0: bad, 1: bad1}))
	mustReconcile(t, r)
	labels, _, _ = mock.VCLState()
	if labels["gwvcl-1"] != routeVCLName(1, RouteVCLHash(good)) {
		t.Errorf("slot 1 lost its previous VCL after a failed update: %q", labels["gwvcl-1"])
	}
	if res := r.SlotResults()["default/p1"]; res.Programmed || res.Hash != RouteVCLHash(bad1) {
		t.Errorf("slot 1 result %+v", res)
	}

	// A resync must not recompile documents already known to fail.
	mock.ClearCallHistory()
	mustReconcile(t, r)
	for _, c := range commandNames(mock.GetCallHistory()) {
		if strings.HasPrefix(c, "vcl.inline") {
			t.Errorf("resync recompiled a known-bad document: %q", c)
		}
	}
	if res := r.SlotResults()["default/p0"]; res.Programmed || res.Message == "" {
		t.Errorf("memoised failure lost its message: %+v", res)
	}
}

func TestReconcile_ManifestValidation(t *testing.T) {
	r, mock := newTestReloader(t)
	r.setVCLConfigMap(vclConfigMap(2, ""))
	doc := routeDoc("a")
	cm := routeConfigMap(map[int]string{0: doc, 5: doc})
	// Slot 1 in the manifest with a document whose hash does not match.
	m, _ := ParseRouteVCLManifest(cm.Data[ManifestKey])
	m.Slots["1"] = RouteVCLSlot{Policy: "default/p1", Hash: RouteVCLHash("other")}
	m.Slots["x"] = RouteVCLSlot{Policy: "default/px", Hash: RouteVCLHash(doc)}
	cm.Data[ManifestKey] = mustJSON(m)
	cm.Data[SlotKey(1)] = doc
	r.setRouteConfigMap(cm)
	mustReconcile(t, r)

	res := r.SlotResults()
	if !res["default/p0"].Programmed {
		t.Errorf("slot 0: %+v", res["default/p0"])
	}
	if p := res["default/p5"]; p.Programmed || !strings.Contains(p.Message, "outside the pool") {
		t.Errorf("slot 5: %+v", p)
	}
	if p := res["default/p1"]; p.Programmed || !strings.Contains(p.Message, "hash") {
		t.Errorf("slot 1: %+v", p)
	}
	if p := res["default/px"]; p.Programmed || p.Slot != -1 {
		t.Errorf("slot x: %+v", p)
	}
	labels, _, _ := mock.VCLState()
	if labels["gwvcl-1"] != PlaceholderVCLName {
		t.Errorf("hash-mismatched slot was programmed: %q", labels["gwvcl-1"])
	}

	// A malformed manifest changes nothing and still lets SlotsReady close.
	r2, mock2 := newTestReloader(t)
	r2.setVCLConfigMap(vclConfigMap(2, ""))
	r2.setRouteConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-route-vcl"},
		Data:       map[string]string{ManifestKey: "{not json"},
	})
	mustReconcile(t, r2)
	if !isClosed(r2.SlotsReady()) {
		t.Error("SlotsReady not closed with a malformed manifest")
	}
	labels, _, _ = mock2.VCLState()
	if labels["gwvcl-0"] != PlaceholderVCLName || labels["gwvcl-1"] != PlaceholderVCLName {
		t.Errorf("labels: %v", labels)
	}
}

func TestReconcile_ShrinkAndGrow(t *testing.T) {
	r, mock := newTestReloader(t)
	r.setVCLConfigMap(vclConfigMap(4, ""))
	d1, d3 := routeDoc("1"), routeDoc("3")
	r.setRouteConfigMap(routeConfigMap(map[int]string{1: d1, 3: d3}))
	mustReconcile(t, r)

	// Two more master reloads at pool 4 so there are old masters to pin
	// labels 2 and 3.
	r.setVCLConfigMap(vclConfigMap(4, "vcl 4.1;\nsub vcl_deliver { set resp.http.A = \"1\"; }\n"))
	mustReconcile(t, r)
	r.setVCLConfigMap(vclConfigMap(4, "vcl 4.1;\nsub vcl_deliver { set resp.http.A = \"2\"; }\n"))
	mustReconcile(t, r)
	_, vcls, _ := mock.VCLState()
	if got := len(masters(vcls)); got != 3 {
		t.Fatalf("masters before shrink = %d (%v), want active + 2 kept", got, vcls)
	}

	// Shrink to 2: slot 3 is in the manifest still (outside the pool now).
	r.setVCLConfigMap(vclConfigMap(2, ""))
	mustReconcile(t, r)
	labels, vcls, active := mock.VCLState()
	for _, l := range []string{"gwvcl-2", "gwvcl-3"} {
		if _, ok := labels[l]; ok {
			t.Errorf("%s survived the shrink", l)
		}
	}
	if labels["gwvcl-1"] != routeVCLName(1, RouteVCLHash(d1)) {
		t.Errorf("slot 1 lost its VCL in the shrink: %q", labels["gwvcl-1"])
	}
	if rv := routeVCLs(vcls); len(rv) != 1 {
		t.Errorf("route VCLs after shrink: %v", rv)
	}
	if m := masters(vcls); len(m) != 1 || m[0] != active {
		t.Errorf("old masters not discarded on shrink: %v (active %s)", m, active)
	}
	if res := r.SlotResults()["default/p3"]; res.Programmed {
		t.Errorf("slot 3 outside the pool reported programmed: %+v", res)
	}

	// Grow to 8: labels 2..7 created on the placeholder, slot 3 programmed again.
	mock.ClearCallHistory()
	r.setVCLConfigMap(vclConfigMap(8, ""))
	mustReconcile(t, r)
	labels, _, _ = mock.VCLState()
	for i := 2; i < 8; i++ {
		if _, ok := labels[SlotLabel(i)]; !ok {
			t.Errorf("%s missing after grow", SlotLabel(i))
		}
	}
	if labels["gwvcl-3"] != routeVCLName(3, RouteVCLHash(d3)) {
		t.Errorf("slot 3 after grow: %q", labels["gwvcl-3"])
	}
	cmds := commandNames(mock.GetCallHistory())
	if indexOf(t, cmds, func(c string) bool { return c == "param.set max_vcl 26" }, "max_vcl 26") >
		indexOf(t, cmds, func(c string) bool { return strings.HasPrefix(c, "vcl.inline vcl_") }, "master") {
		t.Errorf("max_vcl set after the master: %v", cmds)
	}

	// Disable: everything route-VCL related goes away.
	r.setVCLConfigMap(vclConfigMap(0, ""))
	mustReconcile(t, r)
	labels, vcls, active = mock.VCLState()
	if len(labels) != 0 {
		t.Errorf("labels left with pool 0: %v", labels)
	}
	if len(vcls) != 1 || vcls[0] != active {
		t.Errorf("VCLs left with pool 0: %v (active %s)", vcls, active)
	}
}

func TestReconcile_KeepsTwoOldMasters(t *testing.T) {
	r, mock := newTestReloader(t)
	for i := range 5 {
		r.setVCLConfigMap(vclConfigMap(0, fmt.Sprintf("vcl 4.1;\nsub vcl_deliver { set resp.http.N = \"%d\"; }\n", i)))
		mustReconcile(t, r)
	}
	_, vcls, active := mock.VCLState()
	m := masters(vcls)
	if len(m) != 1+DefaultKeepCount || m[len(m)-1] != active {
		t.Errorf("masters = %v (active %s), want the active one plus %d", m, active, DefaultKeepCount)
	}
}

func TestReconcile_SlotsReadyWithoutRouteConfigMap(t *testing.T) {
	r, _ := newTestReloader(t)
	r.SetRouteVCLConfigMap("gw-route-vcl") // watched, but does not exist
	r.setVCLConfigMap(vclConfigMap(4, ""))
	if isClosed(r.SlotsReady()) {
		t.Fatal("SlotsReady closed before any reconcile")
	}
	mustReconcile(t, r)
	if !isClosed(r.SlotsReady()) {
		t.Error("SlotsReady not closed with a missing route VCL ConfigMap")
	}
}

func TestReconcile_MasterCompileFailure(t *testing.T) {
	r, mock := newTestReloader(t)
	r.setVCLConfigMap(vclConfigMap(2, ""))
	mustReconcile(t, r)
	_, _, before := mock.VCLState()

	mock.SetVCLCompileError("BROKEN-MASTER", "VCL compilation failed")
	r.setVCLConfigMap(vclConfigMap(4, "vcl 4.1;\n# BROKEN-MASTER\n"))
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("reconcile succeeded with a broken master")
	}
	_, _, after := mock.VCLState()
	if after != before {
		t.Errorf("active master changed after a failed load: %s -> %s", before, after)
	}

	// The same ConfigMap is retried (and succeeds once the cause is gone).
	mock.SetVCLCompileError("BROKEN-MASTER", "")
	mustReconcile(t, r)
	_, _, after = mock.VCLState()
	if after == before {
		t.Error("master not reloaded on retry")
	}
}

func TestReconcile_InvalidPoolSizeIgnored(t *testing.T) {
	r, mock := newTestReloader(t)
	cm := vclConfigMap(0, "")
	cm.Data[PoolSizeKey] = "lots"
	r.setVCLConfigMap(cm)
	mustReconcile(t, r)
	if len(mock.GetCallHistory()) != 0 {
		t.Errorf("invalid pool size issued commands: %v", mock.GetCallHistory())
	}
}

// flakyList fails VCLListJSON a number of times, then defers to the mock.
type flakyList struct {
	*varnishadm.MockVarnishadm
	failures int
}

func (f *flakyList) VCLListJSON() ([]varnishadm.VCLEntry, error) {
	if f.failures > 0 {
		f.failures--
		return nil, errors.New("connection reset")
	}
	return f.MockVarnishadm.VCLListJSON()
}

func TestLoop_RetriesAfterFailure(t *testing.T) {
	mock := varnishadm.NewMock(6082, "secret", testLogger())
	flaky := &flakyList{MockVarnishadm: mock, failures: 2}
	r := New(flaky, 0, fake.NewSimpleClientset(), "gw-vcl", "default", testLogger())
	r.retryInitial = 10 * time.Millisecond
	r.retryMax = 20 * time.Millisecond
	r.setVCLConfigMap(vclConfigMap(0, ""))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		r.loop(ctx)
		close(done)
	}()
	select {
	case <-r.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile was not retried after transient failures")
	}
	cancel()
	<-done
}

func TestRun_ConfigMapEvents(t *testing.T) {
	mock := varnishadm.NewMock(6082, "secret", testLogger())
	mock.SetStrictVCL(true)
	client := fake.NewSimpleClientset(vclConfigMap(2, ""))
	r := New(mock, 0, client, "gw-vcl", "default", testLogger())
	r.SetRouteVCLConfigMap("gw-route-vcl")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- r.Run(ctx) }()

	select {
	case <-r.SlotsReady():
	case <-time.After(5 * time.Second):
		t.Fatal("SlotsReady not closed")
	}

	doc := routeDoc("run")
	name := routeVCLName(0, RouteVCLHash(doc))
	if _, err := client.CoreV1().ConfigMaps("default").Create(ctx, routeConfigMap(map[int]string{0: doc}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		labels, _, _ := mock.VCLState()
		return labels["gwvcl-0"] == name
	}, "slot 0 programmed after route ConfigMap create")

	if err := client.CoreV1().ConfigMaps("default").Delete(ctx, "gw-route-vcl", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		labels, _, _ := mock.VCLState()
		return labels["gwvcl-0"] == PlaceholderVCLName
	}, "slot 0 cleared after route ConfigMap delete")

	cm := vclConfigMap(2, "vcl 4.1;\nsub vcl_deliver { set resp.http.V = \"2\"; }\n")
	if _, err := client.CoreV1().ConfigMaps("default").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, vcls, _ := mock.VCLState()
		return len(masters(vcls)) == 2
	}, "master reloaded after ConfigMap update")

	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Run did not exit")
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestNew_Defaults(t *testing.T) {
	mock := varnishadm.NewMock(6082, "secret", testLogger())
	for _, keep := range []int{0, -5} {
		r := New(mock, keep, fake.NewSimpleClientset(), "gw-vcl", "default", nil)
		if r.keepCount != DefaultKeepCount {
			t.Errorf("keepCount(%d) = %d, want %d", keep, r.keepCount, DefaultKeepCount)
		}
	}
}

func TestGenerateVCLName(t *testing.T) {
	r, _ := newTestReloader(t)
	name1 := r.generateVCLName()
	name2 := r.generateVCLName()
	if !strings.HasPrefix(name1, vclPrefix) {
		t.Errorf("name %q lacks prefix %q", name1, vclPrefix)
	}
	if name1 == name2 || name1 >= name2 {
		t.Errorf("names not unique and increasing: %q, %q", name1, name2)
	}
}

func TestTrimMessage(t *testing.T) {
	if got := trimMessage("  hello\n"); got != "hello" {
		t.Errorf("trimMessage = %q", got)
	}
	long := strings.Repeat("é", 3000)
	got := trimMessage(long)
	if len(got) > maxResultMessage || !strings.HasSuffix(got, "(truncated)") {
		t.Errorf("len %d, suffix %q", len(got), got[len(got)-20:])
	}
	if !strings.HasPrefix(got, "é") || strings.ContainsRune(got, '�') {
		t.Error("truncation split a rune")
	}
}

func TestParseSlotLabel(t *testing.T) {
	for name, want := range map[string]int{"gwvcl-0": 0, "gwvcl-17": 17} {
		if got, ok := parseSlotLabel(name); !ok || got != want {
			t.Errorf("parseSlotLabel(%q) = %d, %v", name, got, ok)
		}
	}
	for _, name := range []string{"gwvcl-placeholder", "gwvcl-", "gwvcl-01", "gwvcl--1", "vcl_1", "gwvcl-1x"} {
		if _, ok := parseSlotLabel(name); ok {
			t.Errorf("parseSlotLabel(%q) accepted", name)
		}
	}
}
