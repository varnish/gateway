package vcl

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/varnish/gateway/internal/varnishadm"
	"github.com/varnish/gateway/internal/varnishtest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// TestReloaderAgainstVarnishd drives the Reloader against a real varnishd
// through the varnishadm CLI and checks the result both in `vcl.list -j` and
// over HTTP. It skips when varnishd/varnishadm or the built ghost VMOD are
// unavailable (CI's Go job); see internal/varnishtest.
func TestReloaderAgainstVarnishd(t *testing.T) {
	v := varnishtest.Start(t)
	cli := v.CLI()
	ctx := context.Background()

	const testHost = "test.example.com"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s\n", r.URL.Path)
	}))
	t.Cleanup(backend.Close)
	ghostPath := filepath.Join(t.TempDir(), "ghost.json")
	writeSlotGhostConfig(t, ghostPath, testHost, backend.Listener.Addr().(*net.TCPAddr))

	master := func(pool int) *corev1.ConfigMap {
		t.Helper()
		src, err := generateMaster(ghostPath, pool, testGatewayUserVCL)
		if err != nil {
			t.Fatalf("generateMaster: %v", err)
		}
		data := map[string]string{MainVCLKey: src}
		if pool > 0 {
			data[PoolSizeKey] = fmt.Sprint(pool)
		}
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gw-vcl", Namespace: "default"}, Data: data}
	}
	route := func(version string) string {
		t.Helper()
		src, err := generateRouteVCL(ghostPath, testGatewayUserVCL,
			testRouteUserVCL+"\nsub vcl_deliver {\n    set resp.http.X-Route-Version = \""+version+"\";\n}\n")
		if err != nil {
			t.Fatalf("generateRouteVCL: %v", err)
		}
		return src
	}

	var policies []runtime.Object
	for slot := range 8 {
		policies = append(policies, testVVP("default", fmt.Sprintf("p%d", slot)))
	}
	dyn := newFakeDynamic(policies...)

	r := New(cli, 0, fake.NewSimpleClientset(), "gw-vcl", "default", testLogger())
	r.SetStatusWriter(dyn, "gw-pod-0")
	apply := func(vclCM, routeCM *corev1.ConfigMap) {
		t.Helper()
		r.setVCLConfigMap(vclCM)
		r.setRouteConfigMap(routeCM)
		if err := r.reconcile(ctx); err != nil {
			t.Fatalf("reconcile: %v\nvarnishd log:\n%s", err, v.Log())
		}
		if err := r.reporter.flush(ctx); err != nil {
			t.Fatalf("status flush: %v", err)
		}
	}
	list := func() map[string]varnishadm.VCLEntry {
		t.Helper()
		entries, err := cli.VCLListJSON()
		if err != nil {
			t.Fatalf("VCLListJSON: %v", err)
		}
		m := make(map[string]varnishadm.VCLEntry, len(entries))
		for _, e := range entries {
			// A VCL discarded while idle keep-alive sessions still
			// reference it stays listed as "discarded" until it is freed.
			if e.Status == vclStatusDiscarded {
				t.Logf("still listed as discarded (busy %d): %s", e.Busy, e.Name)
				continue
			}
			m[e.Name] = e
		}
		return m
	}
	routeNames := func(l map[string]varnishadm.VCLEntry) []string {
		var out []string
		for name, e := range l {
			if !e.IsLabel() && strings.HasPrefix(name, routeVCLPrefix) {
				out = append(out, name)
			}
		}
		return out
	}

	// 1. Pool 4, slot 1 attached, before the child starts (pod startup).
	docA := route("a")
	apply(master(4), routeConfigMap(map[int]string{1: docA}))
	if !isClosed(r.Ready()) || !isClosed(r.SlotsReady()) {
		t.Fatal("Ready/SlotsReady not closed after the first reconcile")
	}
	l := list()
	nameA := routeVCLName(1, RouteVCLHash(docA))
	if got := l["gwvcl-1"].LabelTarget; got != nameA {
		t.Fatalf("gwvcl-1 -> %q, want %q", got, nameA)
	}
	for _, slot := range []int{0, 2, 3} {
		if got := l[SlotLabel(slot)].LabelTarget; got != PlaceholderVCLName {
			t.Errorf("%s -> %q, want placeholder", SlotLabel(slot), got)
		}
	}

	if _, err := cli.Start(); err != nil {
		t.Fatalf("start child: %v\n%s", err, v.Log())
	}
	base := "http://" + v.ListenAddr()
	client := &http.Client{Timeout: 5 * time.Second}
	get := func(path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = testHost
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		return resp
	}
	expectRoute := func(path, version string) {
		t.Helper()
		resp := get(path)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Which-VCL") != "route" || resp.Header.Get("X-Route-Version") != version {
			t.Errorf("GET %s: status %d, X-Which-VCL %q, X-Route-Version %q; want 200 route %s",
				path, resp.StatusCode, resp.Header.Get("X-Which-VCL"), resp.Header.Get("X-Route-Version"), version)
		}
	}
	expectPlaceholder := func(path string) {
		t.Helper()
		resp := get(path)
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("X-Ghost-Route-VCL") != "unprogrammed" {
			t.Errorf("GET %s: status %d, X-Ghost-Route-VCL %q; want 503 unprogrammed",
				path, resp.StatusCode, resp.Header.Get("X-Ghost-Route-VCL"))
		}
	}
	expectMaster := func(path string) {
		t.Helper()
		resp := get(path)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Which-VCL") != "" || resp.Header.Get("X-Gateway-User-VCL") != "yes" {
			t.Errorf("GET %s: status %d, X-Which-VCL %q; want 200 in the master", path, resp.StatusCode, resp.Header.Get("X-Which-VCL"))
		}
	}
	expectRoute("/s1", "a")
	expectPlaceholder("/s2")
	expectMaster("/plain")

	// 2. Change the text: new name, old one gone.
	docB := route("b")
	nameB := routeVCLName(1, RouteVCLHash(docB))
	apply(master(4), routeConfigMap(map[int]string{1: docB}))
	l = list()
	if got := l["gwvcl-1"].LabelTarget; got != nameB {
		t.Errorf("gwvcl-1 -> %q, want %q", got, nameB)
	}
	if _, ok := l[nameA]; ok {
		t.Errorf("%s not discarded after the change", nameA)
	}
	expectRoute("/s1", "b")

	// 3. Move to slot 2.
	apply(master(4), routeConfigMap(map[int]string{2: docB}))
	l = list()
	if got := l["gwvcl-1"].LabelTarget; got != PlaceholderVCLName {
		t.Errorf("gwvcl-1 -> %q after move, want placeholder", got)
	}
	if got := l["gwvcl-2"].LabelTarget; got != routeVCLName(2, RouteVCLHash(docB)) {
		t.Errorf("gwvcl-2 -> %q after move", got)
	}
	if rn := routeNames(l); len(rn) != 1 {
		t.Errorf("route VCLs after move: %v", rn)
	}
	expectPlaceholder("/s1")
	expectRoute("/s2", "b")

	// 4. Detach.
	apply(master(4), routeConfigMap(nil))
	l = list()
	if got := l["gwvcl-2"].LabelTarget; got != PlaceholderVCLName {
		t.Errorf("gwvcl-2 -> %q after detach", got)
	}
	if rn := routeNames(l); len(rn) != 0 {
		t.Errorf("route VCLs after detach: %v", rn)
	}
	expectPlaceholder("/s2")

	// 5. Grow 4 -> 8 with slot 7 attached.
	docC := route("c")
	apply(master(8), routeConfigMap(map[int]string{7: docC}))
	l = list()
	if got := l["gwvcl-7"].LabelTarget; got != routeVCLName(7, RouteVCLHash(docC)) {
		t.Errorf("gwvcl-7 -> %q after grow", got)
	}
	for slot := 4; slot < 7; slot++ {
		if got := l[SlotLabel(slot)].LabelTarget; got != PlaceholderVCLName {
			t.Errorf("%s -> %q after grow", SlotLabel(slot), got)
		}
	}
	if !strings.HasPrefix(l[activeName(l)].Name, vclPrefix) {
		t.Errorf("active VCL %q is not a master", activeName(l))
	}
	expectRoute("/s7", "c")
	expectMaster("/plain")

	// 6. Shrink 8 -> 2: labels 2..7 and the slot 7 route VCL go away.
	apply(master(2), routeConfigMap(map[int]string{7: docC}))
	l = list()
	for slot := 2; slot < 8; slot++ {
		if _, ok := l[SlotLabel(slot)]; ok {
			t.Errorf("%s survived the shrink", SlotLabel(slot))
		}
	}
	if rn := routeNames(l); len(rn) != 0 {
		t.Errorf("route VCLs after shrink: %v", rn)
	}
	masterCount := 0
	for name, e := range l {
		if !e.IsLabel() && strings.HasPrefix(name, vclPrefix) {
			masterCount++
		}
	}
	if masterCount != 1 {
		t.Errorf("%d masters after shrink, want only the active one: %+v", masterCount, l)
	}
	expectMaster("/s7") // slot 7 is outside the pool now: stays in the master
	if res := r.SlotResults()["default/p7"]; res.Programmed || !strings.Contains(res.Message, "outside the pool") {
		t.Errorf("slot 7 result after shrink: %+v", res)
	}

	// 7. Syntax error in slot 0 while slot 1 is attached.
	badDoc, err := generateRouteVCL(ghostPath, testGatewayUserVCL, "sub vcl_recv {\n    set req.http.X-Broken = ;\n}\n")
	if err != nil {
		t.Fatal(err)
	}
	apply(master(2), routeConfigMap(map[int]string{0: badDoc, 1: docA}))
	l = list()
	if got := l["gwvcl-0"].LabelTarget; got != PlaceholderVCLName {
		t.Errorf("gwvcl-0 -> %q after a failed compile, want placeholder", got)
	}
	if got := l["gwvcl-1"].LabelTarget; !matchesRouteVCL(got, nameA) {
		t.Errorf("gwvcl-1 -> %q, want %q (or a renamed load of it)", got, nameA)
	}
	expectRoute("/s1", "a")
	expectPlaceholder("/s0")
	res := r.SlotResults()["default/p0"]
	if res.Programmed || !strings.Contains(res.Message, "VCC-compiler") {
		t.Errorf("slot 0 result: %+v", res)
	}

	// Status written through the dynamic client.
	entry := podResultFor(t, dyn, "p0", "gw-pod-0")
	if entry["programmed"] != false || !strings.Contains(fmt.Sprint(entry["message"]), "VCC-compiler") || entry["hash"] != RouteVCLHash(badDoc) {
		t.Errorf("p0 status entry: %v", entry)
	}
	entry = podResultFor(t, dyn, "p1", "gw-pod-0")
	if entry["programmed"] != true || entry["hash"] != RouteVCLHash(docA) {
		t.Errorf("p1 status entry: %v", entry)
	}
}

func activeName(l map[string]varnishadm.VCLEntry) string {
	for name, e := range l {
		if e.Status == "active" {
			return name
		}
	}
	return ""
}

// podResultFor returns pod's entry in default/<name>'s status.podResults.
func podResultFor(t *testing.T, dyn *dynamicfake.FakeDynamicClient, name, pod string) map[string]any {
	t.Helper()
	for _, item := range podResults(t, dyn, "default", name) {
		if m, ok := item.(map[string]any); ok && m["podName"] == pod {
			return m
		}
	}
	t.Fatalf("no podResults entry for %s in default/%s", pod, name)
	return nil
}

// writeSlotGhostConfig writes a ghost.json with exact-path routes /s<n> on
// slot n for n in 0..7, and /plain without a slot.
func writeSlotGhostConfig(t *testing.T, path, host string, addr *net.TCPAddr) {
	t.Helper()
	var routes []string
	mk := func(p, slot string) string {
		return fmt.Sprintf(`{"path_match":{"type":"Exact","value":%q},"backend_groups":[{"weight":100,"backends":[{"address":%q,"port":%d}]}],%s"priority":100}`,
			p, addr.IP.String(), addr.Port, slot)
	}
	for slot := range 8 {
		routes = append(routes, mk(fmt.Sprintf("/s%d", slot), fmt.Sprintf(`"vcl_slot":%d,`, slot)))
	}
	routes = append(routes, mk("/plain", ""))
	cfg := fmt.Sprintf(`{"version":2,"vhosts":{%q:{"routes":[%s]}}}`, host, strings.Join(routes, ","))
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatalf("os.WriteFile(%s): %v", path, err)
	}
}
