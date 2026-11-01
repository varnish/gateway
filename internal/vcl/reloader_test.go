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

// TestReconcile_ManifestWithOperatorFields checks that the chaperone accepts
// a manifest carrying the operator-side fields (slot route, unassigned list)
// and fields it does not know about yet: they are ignored and the slot is
// programmed as usual.
func TestReconcile_ManifestWithOperatorFields(t *testing.T) {
	doc := routeDoc("a")
	raw := fmt.Sprintf(`{
  "slots": {"0": {"policy": "default/p0", "route": "default/r0", "generation": 3, "hash": %q, "future": true}},
  "unassigned": [{"policy": "default/p9", "route": "default/r9", "reason": %q}],
  "somethingNew": {"x": 1}
}`, RouteVCLHash(doc), UnassignedNoSlotAvailable)

	m, err := ParseRouteVCLManifest(raw)
	if err != nil {
		t.Fatalf("ParseRouteVCLManifest: %v", err)
	}
	if s := m.Slots["0"]; s.Policy != "default/p0" || s.Route != "default/r0" || s.Generation != 3 {
		t.Errorf("slot 0 = %+v", s)
	}
	if len(m.Unassigned) != 1 || m.Unassigned[0].Reason != UnassignedNoSlotAvailable {
		t.Errorf("unassigned = %+v", m.Unassigned)
	}

	r, mock := newTestReloader(t)
	r.setVCLConfigMap(vclConfigMap(2, ""))
	r.setRouteConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-route-vcl", Namespace: "default"},
		Data:       map[string]string{ManifestKey: raw, SlotKey(0): doc},
	})
	mustReconcile(t, r)
	if res := r.SlotResults()["default/p0"]; !res.Programmed {
		t.Errorf("slot 0 not programmed: %+v", res)
	}
	if _, ok := r.SlotResults()["default/p9"]; ok {
		t.Error("an unassigned policy got a slot result")
	}
	labels, _, _ := mock.VCLState()
	if labels["gwvcl-0"] == PlaceholderVCLName {
		t.Errorf("gwvcl-0 still on the placeholder: %v", labels)
	}
}

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

// vccError is a varnishd-style VCC failure payload for a vcl.inline.
const vccError = "Message from VCC-compiler:\nExpected ';' got '}'\nRunning VCC-compiler failed, exited with 2\nVCL compilation failed"

// fakeClock replaces the reloader's clock.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func useFakeClock(r *Reloader) *fakeClock {
	c := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	r.now = c.now
	return c
}

// nonListCommands is the call history without vcl.list and inline bodies.
func nonListCommands(mock *varnishadm.MockVarnishadm) []string {
	var out []string
	for _, c := range commandNames(mock.GetCallHistory()) {
		if !strings.HasPrefix(c, "vcl.list") {
			out = append(out, c)
		}
	}
	return out
}

func countPrefix(cmds []string, prefix string) int {
	n := 0
	for _, c := range cmds {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestReconcile_MasterCompileFailure(t *testing.T) {
	r, mock := newTestReloader(t)
	clock := useFakeClock(r)
	r.setVCLConfigMap(vclConfigMap(2, ""))
	mustReconcile(t, r)
	_, _, before := mock.VCLState()

	mock.SetVCLCompileError("BROKEN-MASTER", vccError)
	r.setVCLConfigMap(vclConfigMap(4, "vcl 4.1;\n# BROKEN-MASTER\n"))
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("reconcile succeeded with a broken master")
	}
	_, _, after := mock.VCLState()
	if after != before {
		t.Errorf("active master changed after a failed load: %s -> %s", before, after)
	}

	// The failure is memoised: the retry (and later resyncs) do not
	// recompile the same main.vcl, but still report the error.
	mock.SetVCLCompileError("BROKEN-MASTER", "")
	mock.ClearCallHistory()
	if err := r.reconcile(context.Background()); err == nil {
		t.Error("memoised master failure not reported")
	}
	if n := countPrefix(nonListCommands(mock), "vcl.inline vcl_"); n != 0 {
		t.Errorf("memoised master recompiled %d times", n)
	}

	// After the TTL the same main.vcl is compiled again (and now succeeds).
	clock.advance(failureMemoTTL + time.Second)
	mustReconcile(t, r)
	if _, _, after = mock.VCLState(); after == before {
		t.Error("master not reloaded after the memo expired")
	}

	// A changed main.vcl is tried right away, memo or not.
	_, _, before = mock.VCLState()
	mock.SetVCLCompileError("BROKEN-MASTER", vccError)
	r.setVCLConfigMap(vclConfigMap(4, "vcl 4.1;\n# BROKEN-MASTER 2\n"))
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("reconcile succeeded with a broken master")
	}
	mock.SetVCLCompileError("BROKEN-MASTER", "")
	r.setVCLConfigMap(vclConfigMap(4, "vcl 4.1;\n# fixed\n"))
	mustReconcile(t, r)
	if _, _, after = mock.VCLState(); after == before {
		t.Error("changed main.vcl not loaded while an older one is memoised")
	}
}

// A transient master failure (not VCC) is not memoised.
func TestReconcile_MasterTransientFailureNotMemoised(t *testing.T) {
	r, mock := newTestReloader(t)
	r.setVCLConfigMap(vclConfigMap(0, ""))
	mustReconcile(t, r)
	mock.SetVCLCompileError("NEW-MASTER", "Too many (12) VCLs already loaded")
	r.setVCLConfigMap(vclConfigMap(0, "vcl 4.1;\n# NEW-MASTER\n"))
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("reconcile succeeded")
	}
	mock.SetVCLCompileError("NEW-MASTER", "")
	mustReconcile(t, r)
}

// A master that fails to compile must not freeze the slots: slots both the
// desired and the active master dispatch to are still programmed, and the
// broken master is not recompiled on every pass.
func TestReconcile_FailingMasterStillProgramsSlots(t *testing.T) {
	r, mock := newTestReloader(t)
	r.setVCLConfigMap(vclConfigMap(2, ""))
	mustReconcile(t, r)
	_, _, active := mock.VCLState()

	mock.SetVCLCompileError("BROKEN-MASTER", vccError)
	r.setVCLConfigMap(vclConfigMap(4, "vcl 4.1;\n# BROKEN-MASTER\n"))
	d1, d3 := routeDoc("one"), routeDoc("three")
	r.setRouteConfigMap(routeConfigMap(map[int]string{1: d1, 3: d3}))
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("reconcile hid the master failure")
	}
	labels, _, nowActive := mock.VCLState()
	if nowActive != active {
		t.Fatalf("active master changed: %s -> %s", active, nowActive)
	}
	if labels["gwvcl-1"] != routeVCLName(1, RouteVCLHash(d1)) {
		t.Errorf("slot 1 not programmed beside a failing master: %v", labels)
	}
	// Slot 3 exists only for the new (failed) master: its label is created
	// but stays on the placeholder.
	if labels["gwvcl-3"] != PlaceholderVCLName {
		t.Errorf("gwvcl-3 -> %q, want placeholder", labels["gwvcl-3"])
	}
	if res := r.SlotResults()["default/p1"]; !res.Programmed {
		t.Errorf("slot 1 result %+v", res)
	}
	if res := r.SlotResults()["default/p3"]; res.Programmed || !strings.Contains(res.Message, "waits for the master") {
		t.Errorf("slot 3 result %+v", res)
	}

	// Next pass: the master is not recompiled; a slot change still lands.
	mock.ClearCallHistory()
	d1b := routeDoc("one-b")
	r.setRouteConfigMap(routeConfigMap(map[int]string{1: d1b, 3: d3}))
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("memoised master failure not reported")
	}
	cmds := nonListCommands(mock)
	if n := countPrefix(cmds, "vcl.inline vcl_"); n != 0 {
		t.Errorf("broken master recompiled: %v", cmds)
	}
	labels, _, _ = mock.VCLState()
	if labels["gwvcl-1"] != routeVCLName(1, RouteVCLHash(d1b)) {
		t.Errorf("slot 1 change not applied beside a failing master: %v", labels)
	}

	// Fixed master: slot 3 gets programmed too.
	mock.SetVCLCompileError("BROKEN-MASTER", "")
	r.setVCLConfigMap(vclConfigMap(4, "vcl 4.1;\n# fixed\n"))
	mustReconcile(t, r)
	labels, _, _ = mock.VCLState()
	if labels["gwvcl-3"] != routeVCLName(3, RouteVCLHash(d3)) {
		t.Errorf("slot 3 after the master loaded: %v", labels)
	}
}

// With no master ever loaded, a failing master returns before the slot pass
// and SlotsReady stays open.
func TestReconcile_FirstMasterFailure(t *testing.T) {
	r, mock := newTestReloader(t)
	mock.SetVCLCompileError("BROKEN-MASTER", vccError)
	r.setVCLConfigMap(vclConfigMap(2, "vcl 4.1;\n# BROKEN-MASTER\n"))
	r.setRouteConfigMap(routeConfigMap(map[int]string{0: routeDoc("x")}))
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("reconcile succeeded")
	}
	if isClosed(r.Ready()) || isClosed(r.SlotsReady()) {
		t.Error("Ready/SlotsReady closed without a master")
	}
}

func TestReconcile_InvalidPoolSize(t *testing.T) {
	// No master loaded yet: main.vcl is loaded as pool 0 so the gateway
	// comes up.
	r, mock := newTestReloader(t)
	cm := vclConfigMap(0, "")
	cm.Data[PoolSizeKey] = "lots"
	r.setVCLConfigMap(cm)
	mustReconcile(t, r)
	if !isClosed(r.Ready()) || !isClosed(r.SlotsReady()) {
		t.Error("first load with an invalid pool size did not come up")
	}
	labels, _, _ := mock.VCLState()
	if len(labels) != 0 {
		t.Errorf("labels created for an invalid pool size: %v", labels)
	}

	// Once a master is applied, an invalid value is ignored.
	mock.ClearCallHistory()
	cm = vclConfigMap(0, "vcl 4.1;\n# changed\n")
	cm.Data[PoolSizeKey] = "-3"
	r.setVCLConfigMap(cm)
	mustReconcile(t, r)
	if len(mock.GetCallHistory()) != 0 {
		t.Errorf("invalid pool size issued commands: %v", mock.GetCallHistory())
	}
}

// A non-VCC vcl.inline failure (max_vcl refusal, ENOMEM, ...) is retried on
// the next pass, and the reconcile reports it so the backoff kicks in.
func TestReconcile_TransientRouteFailureRetried(t *testing.T) {
	r, mock := newTestReloader(t)
	r.setVCLConfigMap(vclConfigMap(2, ""))
	doc := routeDoc("TRANSIENT")
	name := routeVCLName(0, RouteVCLHash(doc))
	mock.SetVCLCompileError("TRANSIENT", "Too many (12) VCLs already loaded\nMaybe discard some? (Or increase max_vcl)")
	r.setRouteConfigMap(routeConfigMap(map[int]string{0: doc}))
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("transient route VCL failure not reported for retry")
	}
	if res := r.SlotResults()["default/p0"]; res.Programmed || !strings.Contains(res.Message, "Too many") {
		t.Errorf("result %+v", res)
	}
	if !isClosed(r.SlotsReady()) {
		t.Error("a transient slot failure must not hold SlotsReady")
	}

	mock.ClearCallHistory()
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("still failing, but not reported")
	}
	if n := countPrefix(nonListCommands(mock), "vcl.inline "+name); n != 1 {
		t.Errorf("transient failure not retried: %v", nonListCommands(mock))
	}

	mock.SetVCLCompileError("TRANSIENT", "")
	mustReconcile(t, r)
	labels, _, _ := mock.VCLState()
	if labels["gwvcl-0"] != name || !r.SlotResults()["default/p0"].Programmed {
		t.Errorf("slot 0 not programmed after the cause went away: %v", labels)
	}
}

// A VCC failure is not recompiled within the TTL, is after it, and the memo
// is cleared by a master change or a pool size change.
func TestReconcile_CompileFailureMemo(t *testing.T) {
	r, mock := newTestReloader(t)
	clock := useFakeClock(r)
	r.setVCLConfigMap(vclConfigMap(2, ""))
	bad := routeDoc("BROKEN")
	inline := "vcl.inline " + routeVCLName(0, RouteVCLHash(bad))
	mock.SetVCLCompileError("BROKEN", vccError)
	r.setRouteConfigMap(routeConfigMap(map[int]string{0: bad}))
	mustReconcile(t, r) // a VCC failure is not a reconcile error

	recompiled := func(what string, want int) {
		t.Helper()
		if n := countPrefix(nonListCommands(mock), inline); n != want {
			t.Errorf("%s: %d compiles of the bad document, want %d", what, n, want)
		}
		mock.ClearCallHistory()
	}
	mock.ClearCallHistory()
	clock.advance(failureMemoTTL - time.Second)
	mustReconcile(t, r)
	recompiled("within the TTL", 0)

	clock.advance(2 * time.Second)
	mustReconcile(t, r)
	recompiled("after the TTL", 1)

	mustReconcile(t, r)
	recompiled("re-memoised", 0)

	r.setVCLConfigMap(vclConfigMap(2, "vcl 4.1;\n# new master\n"))
	mustReconcile(t, r)
	recompiled("after a master change", 1)

	r.setVCLConfigMap(vclConfigMap(3, "vcl 4.1;\n# new master\n"))
	mock.SetVCLCompileError("BROKEN", vccError) // keep failing
	mustReconcile(t, r)
	recompiled("after a pool change", 1)
	if res := r.SlotResults()["default/p0"]; res.Programmed || !strings.Contains(res.Message, "VCC-compiler") {
		t.Errorf("result %+v", res)
	}
}

// A vcl.label refused because it would create a return(vcl) loop depends on
// another slot's document: it is retried (not memoised, the compiled VCL is
// kept) and succeeds once the other document changes.
func TestReconcile_LabelLoopRetried(t *testing.T) {
	r, mock := newTestReloader(t)
	r.setVCLConfigMap(vclConfigMap(2, ""))
	toSlot := func(label, tag string) string {
		return GenerateRouteVCL("", "vcl 4.1;\nsub vcl_recv {\n    # "+tag+"\n    return (vcl("+label+"));\n}\n")
	}
	d0, d1 := toSlot("gwvcl-1", "a"), toSlot("gwvcl-0", "b")
	name0, name1 := routeVCLName(0, RouteVCLHash(d0)), routeVCLName(1, RouteVCLHash(d1))
	r.setRouteConfigMap(routeConfigMap(map[int]string{0: d0, 1: d1}))
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("refused label not reported for retry")
	}
	labels, vcls, _ := mock.VCLState()
	if labels["gwvcl-0"] != name0 || labels["gwvcl-1"] != PlaceholderVCLName {
		t.Fatalf("labels %v", labels)
	}
	if !slices.Contains(vcls, name1) {
		t.Errorf("compiled slot 1 VCL was discarded after the refused label: %v", vcls)
	}
	if res := r.SlotResults()["default/p1"]; res.Programmed || !strings.Contains(res.Message, "loop") {
		t.Errorf("slot 1 result %+v", res)
	}

	// Next pass: the label is retried, the document is not recompiled.
	mock.ClearCallHistory()
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("still refused, but not reported")
	}
	cmds := nonListCommands(mock)
	if countPrefix(cmds, "vcl.label gwvcl-1 "+name1) != 1 || countPrefix(cmds, "vcl.inline") != 0 {
		t.Errorf("retry pass = %v, want one vcl.label and no vcl.inline", cmds)
	}

	// Slot 0 stops returning to gwvcl-1: slot 1's label now succeeds.
	d0b := routeDoc("plain")
	r.setRouteConfigMap(routeConfigMap(map[int]string{0: d0b, 1: d1}))
	mustReconcile(t, r)
	labels, _, _ = mock.VCLState()
	if labels["gwvcl-1"] != name1 || !r.SlotResults()["default/p1"].Programmed {
		t.Errorf("slot 1 not programmed after the loop went away: %v", labels)
	}
}

// failLabel fails VCLLabel for one label with a transport error a number of
// times, then defers to the mock.
type failLabel struct {
	*varnishadm.MockVarnishadm
	label    string
	failures int
}

func (f *failLabel) VCLLabel(label, name string) (varnishadm.VarnishResponse, error) {
	if label == f.label && f.failures > 0 {
		f.failures--
		return varnishadm.VarnishResponse{}, errors.New("connection reset")
	}
	return f.MockVarnishadm.VCLLabel(label, name)
}

// A transport error between vcl.inline and vcl.label leaves an unlabelled
// route VCL. The next pass adopts it (no recompile); if the document is no
// longer wanted, discardOrphans removes it.
func TestReconcile_TransportErrorBetweenInlineAndLabel(t *testing.T) {
	mock := varnishadm.NewMock(6082, "secret", testLogger())
	mock.SetStrictVCL(true)
	flaky := &failLabel{MockVarnishadm: mock, label: "gwvcl-0"}
	r := New(flaky, 0, fake.NewSimpleClientset(), "gw-vcl", "default", testLogger())
	r.setVCLConfigMap(vclConfigMap(2, ""))
	mustReconcile(t, r)

	doc := routeDoc("orphan")
	name := routeVCLName(0, RouteVCLHash(doc))
	flaky.failures = 1
	r.setRouteConfigMap(routeConfigMap(map[int]string{0: doc}))
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("transport error not reported")
	}
	labels, vcls, _ := mock.VCLState()
	if !slices.Contains(vcls, name) || labels["gwvcl-0"] != PlaceholderVCLName {
		t.Fatalf("after the transport error: labels=%v vcls=%v", labels, vcls)
	}

	mock.ClearCallHistory()
	mustReconcile(t, r)
	cmds := nonListCommands(mock)
	if countPrefix(cmds, "vcl.inline") != 0 || countPrefix(cmds, "vcl.label gwvcl-0 "+name) != 1 {
		t.Errorf("adopting pass = %v, want the orphan labelled without a recompile", cmds)
	}
	if !r.SlotResults()["default/p0"].Programmed {
		t.Error("slot 0 not programmed")
	}

	// Again, but the slot is detached before the next pass: the orphan goes.
	doc2 := routeDoc("orphan-2")
	name2 := routeVCLName(0, RouteVCLHash(doc2))
	flaky.failures = 1
	r.setRouteConfigMap(routeConfigMap(map[int]string{0: doc2}))
	if err := r.reconcile(context.Background()); err == nil {
		t.Fatal("transport error not reported")
	}
	r.setRouteConfigMap(routeConfigMap(nil))
	mustReconcile(t, r)
	labels, vcls, _ = mock.VCLState()
	if rv := routeVCLs(vcls); len(rv) != 0 {
		t.Errorf("route VCLs left after detach: %v (orphan %s)", rv, name2)
	}
	if labels["gwvcl-0"] != PlaceholderVCLName {
		t.Errorf("gwvcl-0 -> %q", labels["gwvcl-0"])
	}
}

// A placeholder discarded while busy keeps its name; re-enabling the pool
// loads it under a free name, and every placeholder comparison accepts it.
func TestReconcile_PlaceholderRenamedWhileBusy(t *testing.T) {
	r, mock := newTestReloader(t)
	r.setVCLConfigMap(vclConfigMap(2, ""))
	mustReconcile(t, r)
	mock.HoldVCL(PlaceholderVCLName)
	r.setVCLConfigMap(vclConfigMap(0, ""))
	mustReconcile(t, r)

	r.setVCLConfigMap(vclConfigMap(2, ""))
	mustReconcile(t, r)
	ph2 := PlaceholderVCLName + "_2"
	labels, _, _ := mock.VCLState()
	if labels["gwvcl-0"] != ph2 || labels["gwvcl-1"] != ph2 {
		t.Fatalf("labels %v, want both on %s", labels, ph2)
	}

	// Attach and detach slot 0: it goes back to the renamed placeholder, and
	// an idle pass leaves it alone.
	doc := routeDoc("x")
	r.setRouteConfigMap(routeConfigMap(map[int]string{0: doc}))
	mustReconcile(t, r)
	r.setRouteConfigMap(routeConfigMap(nil))
	mustReconcile(t, r)
	labels, _, _ = mock.VCLState()
	if labels["gwvcl-0"] != ph2 {
		t.Errorf("gwvcl-0 -> %q after detach, want %s", labels["gwvcl-0"], ph2)
	}
	mock.ClearCallHistory()
	mustReconcile(t, r)
	if cmds := nonListCommands(mock); len(cmds) != 0 {
		t.Errorf("idle pass with a renamed placeholder issued %v", cmds)
	}

	// Shrink with a slot attached repoints to the renamed placeholder;
	// disabling discards it.
	r.setRouteConfigMap(routeConfigMap(map[int]string{1: doc}))
	mustReconcile(t, r)
	r.setVCLConfigMap(vclConfigMap(1, ""))
	mustReconcile(t, r)
	r.setVCLConfigMap(vclConfigMap(0, ""))
	mustReconcile(t, r)
	mock.ReleaseVCL(PlaceholderVCLName)
	labels, vcls, active := mock.VCLState()
	if len(labels) != 0 || len(vcls) != 1 || vcls[0] != active {
		t.Errorf("after disable: labels=%v vcls=%v", labels, vcls)
	}
}

// One slot per policy is the contract; if the manifest lists a policy twice,
// the programmed result wins over an invalid or failed one.
func TestReconcile_DuplicatePolicyKeepsBestResult(t *testing.T) {
	r, mock := newTestReloader(t)
	mock.SetVCLCompileError("BROKEN", vccError)
	r.setVCLConfigMap(vclConfigMap(4, ""))
	good, bad := routeDoc("good"), routeDoc("BROKEN")
	cm := routeConfigMap(map[int]string{0: bad, 1: good, 2: good})
	m, _ := ParseRouteVCLManifest(cm.Data[ManifestKey])
	// p-a: slot 0 fails to compile, slot 1 is programmed.
	// p-b: slot 2 programmed, slot 3 has a hash mismatch (invalid).
	// p-c: slots 5 and 7, both outside the pool (invalid).
	m.Slots["0"] = RouteVCLSlot{Policy: "default/p-a", Hash: RouteVCLHash(bad)}
	m.Slots["1"] = RouteVCLSlot{Policy: "default/p-a", Hash: RouteVCLHash(good)}
	m.Slots["2"] = RouteVCLSlot{Policy: "default/p-b", Hash: RouteVCLHash(good)}
	m.Slots["3"] = RouteVCLSlot{Policy: "default/p-b", Hash: RouteVCLHash("other")}
	m.Slots["5"] = RouteVCLSlot{Policy: "default/p-c", Hash: RouteVCLHash(good)}
	m.Slots["7"] = RouteVCLSlot{Policy: "default/p-c", Hash: RouteVCLHash(good)}
	cm.Data[ManifestKey] = mustJSON(m)
	cm.Data[SlotKey(3)] = good
	r.setRouteConfigMap(cm)
	mustReconcile(t, r)

	res := r.SlotResults()
	if p := res["default/p-a"]; !p.Programmed || p.Slot != 1 {
		t.Errorf("p-a: %+v, want slot 1 programmed", p)
	}
	if p := res["default/p-b"]; !p.Programmed || p.Slot != 2 {
		t.Errorf("p-b: %+v, want slot 2 programmed", p)
	}
	if p := res["default/p-c"]; p.Programmed || p.Slot != 5 {
		t.Errorf("p-c: %+v, want the first invalid entry", p)
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
