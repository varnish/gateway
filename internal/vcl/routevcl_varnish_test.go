package vcl

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/varnish/gateway/internal/varnishtest"
)

// TestGeneratedVCLCompiles loads the placeholder, slot labels, master and a
// route VCL into a real varnishd and checks the jump end to end. It skips
// when varnishd/varnishadm or the built ghost VMOD are unavailable (CI's Go
// job); see internal/varnishtest.
func TestGeneratedVCLCompiles(t *testing.T) {
	v := varnishtest.Start(t)

	const (
		poolSize  = 64
		routeSlot = 2
		masterVCL = "master"
		routeVCL  = "route-2"
		testHost  = "test.example.com"
	)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if vals, ok := r.Header[http.CanonicalHeaderKey(DispatchHeader)]; ok {
			w.Header().Set("X-Backend-Saw-Slot", "present:"+vals[0])
		} else {
			w.Header().Set("X-Backend-Saw-Slot", "absent")
		}
		if r.Header.Get("X-Route-VCL-Recv") != "" {
			w.Header().Set("X-Backend-Saw-Route-Recv", "yes")
		}
		fmt.Fprintf(w, "path=%s\n", r.URL.Path)
	}))
	t.Cleanup(backend.Close)

	ghostPath := filepath.Join(t.TempDir(), "ghost.json")
	writeTestGhostConfig(t, ghostPath, testHost, backend.Listener.Addr().(*net.TCPAddr), routeSlot)

	master, err := generateMaster(ghostPath, poolSize, testGatewayUserVCL)
	if err != nil {
		t.Fatalf("generateMaster: %v", err)
	}
	route, err := generateRouteVCL(ghostPath, testGatewayUserVCL, testRouteUserVCL)
	if err != nil {
		t.Fatalf("generateRouteVCL: %v", err)
	}

	// Bootstrap order: placeholder, every slot label, then the master (which
	// references the labels and fails to compile without them).
	if err := v.LoadVCL(PlaceholderVCLName, GeneratePlaceholder()); err != nil {
		t.Fatalf("load placeholder: %v", err)
	}
	for slot := range poolSize {
		if err := v.Label(SlotLabel(slot), PlaceholderVCLName); err != nil {
			t.Fatalf("label %s: %v", SlotLabel(slot), err)
		}
	}
	if err := v.LoadVCL(masterVCL, master); err != nil {
		t.Fatalf("load master: %v", err)
	}
	if err := v.LoadVCL(routeVCL, route); err != nil {
		t.Fatalf("load route VCL: %v", err)
	}
	if err := v.Label(SlotLabel(routeSlot), routeVCL); err != nil {
		t.Fatalf("label %s -> %s: %v", SlotLabel(routeSlot), routeVCL, err)
	}

	// Negative: a route VCL with a syntax error in its user part must fail to
	// load and leave everything else in place.
	bad, err := generateRouteVCL(ghostPath, testGatewayUserVCL, "sub vcl_recv {\n    set req.http.X-Broken = ;\n}\n")
	if err != nil {
		t.Fatalf("generateRouteVCL(bad): %v", err)
	}
	if err := v.LoadVCL("route-bad", bad); err == nil {
		t.Fatal("route VCL with a syntax error loaded")
	}
	list := vclList(t, v)
	if _, ok := list["route-bad"]; ok {
		t.Error("failed route VCL appears in vcl.list")
	}
	if _, ok := list[masterVCL]; !ok {
		t.Error("master missing from vcl.list after failed load")
	}
	for slot := range poolSize {
		want := PlaceholderVCLName
		if slot == routeSlot {
			want = routeVCL
		}
		if got := list[SlotLabel(slot)]; got != want {
			t.Errorf("label %s -> %q, want %q", SlotLabel(slot), got, want)
		}
	}

	// End to end.
	if err := v.Use(masterVCL); err != nil {
		t.Fatalf("vcl.use master: %v", err)
	}
	if err := v.Start(); err != nil {
		t.Fatalf("start child: %v\n%s", err, v.Log())
	}
	base := "http://" + v.ListenAddr()
	client := &http.Client{Timeout: 5 * time.Second}

	get := func(path string, hdr map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatalf("http.NewRequest(%s): %v", path, err)
		}
		req.Host = testHost
		for k, val := range hdr {
			req.Header.Set(k, val)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		return resp
	}

	t.Run("slot route jumps to route VCL", func(t *testing.T) {
		resp := get("/slot", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if got := resp.Header.Get("X-Which-VCL"); got != "route" {
			t.Errorf("X-Which-VCL = %q, want route", got)
		}
		if got := resp.Header.Get("X-Backend-Saw-Slot"); got != "absent" {
			t.Errorf("backend saw X-Ghost-VCL-Slot: %q", got)
		}
		if got := resp.Header.Get("X-Backend-Saw-Route-Recv"); got != "yes" {
			t.Errorf("route VCL vcl_recv did not run (X-Backend-Saw-Route-Recv = %q)", got)
		}
		if got := resp.Header.Get("X-Route-Backend-Response"); got != "1" {
			t.Errorf("route VCL vcl_backend_response did not run (%q)", got)
		}
		// G5 compose: gateway user VCL runs inside the route VCL too.
		if got := resp.Header.Get("X-Gateway-User-VCL"); got != "yes" {
			t.Errorf("gateway user VCL did not run in route VCL (%q)", got)
		}
	})

	t.Run("route without slot stays in master", func(t *testing.T) {
		resp := get("/plain", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if got := resp.Header.Get("X-Which-VCL"); got != "" {
			t.Errorf("X-Which-VCL = %q, want none", got)
		}
		if got := resp.Header.Get("X-Gateway-User-VCL"); got != "yes" {
			t.Errorf("gateway user VCL did not run in master (%q)", got)
		}
		if got := resp.Header.Get("X-Backend-Saw-Slot"); got != "absent" {
			t.Errorf("backend saw X-Ghost-VCL-Slot: %q", got)
		}
	})

	t.Run("client-supplied slot header does not jump", func(t *testing.T) {
		resp := get("/plain", map[string]string{DispatchHeader: strconv.Itoa(routeSlot)})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if got := resp.Header.Get("X-Which-VCL"); got != "" {
			t.Errorf("X-Which-VCL = %q, want none", got)
		}
		if got := resp.Header.Get("X-Backend-Saw-Slot"); got != "absent" {
			t.Errorf("backend saw X-Ghost-VCL-Slot: %q", got)
		}
	})

	t.Run("client-supplied slot header on slot route is replaced", func(t *testing.T) {
		resp := get("/slot", map[string]string{DispatchHeader: "0"})
		if got := resp.Header.Get("X-Which-VCL"); got != "route" {
			t.Errorf("X-Which-VCL = %q, want route (ghost's slot must win)", got)
		}
		if got := resp.Header.Get("X-Backend-Saw-Slot"); got != "absent" {
			t.Errorf("backend saw X-Ghost-VCL-Slot: %q", got)
		}
	})

	t.Run("placeholder slot answers 503", func(t *testing.T) {
		resp := get("/unprogrammed", nil)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status %d, want 503", resp.StatusCode)
		}
		if got := resp.Header.Get("X-Ghost-Route-VCL"); got != "unprogrammed" {
			t.Errorf("X-Ghost-Route-VCL = %q, want unprogrammed", got)
		}
	})

	t.Run("slot outside the pool stays in master", func(t *testing.T) {
		resp := get("/outofrange", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if got := resp.Header.Get("X-Which-VCL"); got != "" {
			t.Errorf("X-Which-VCL = %q, want none", got)
		}
		if got := resp.Header.Get("X-Backend-Saw-Slot"); got != "absent" {
			t.Errorf("backend saw X-Ghost-VCL-Slot: %q", got)
		}
	})
}

// writeTestGhostConfig writes a ghost.json with four exact-path routes on
// host: /slot (routeSlot), /plain (no slot), /unprogrammed (slot 1, left on
// the placeholder) and /outofrange (slot 999, outside any pool).
func writeTestGhostConfig(t *testing.T, path, host string, addr *net.TCPAddr, routeSlot int) {
	t.Helper()
	type pathMatch struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	type backend struct {
		Address string `json:"address"`
		Port    int    `json:"port"`
	}
	type group struct {
		Weight   int       `json:"weight"`
		Backends []backend `json:"backends"`
	}
	type route struct {
		PathMatch     pathMatch `json:"path_match"`
		BackendGroups []group   `json:"backend_groups"`
		VCLSlot       *int      `json:"vcl_slot,omitempty"`
		Priority      int       `json:"priority"`
	}
	mk := func(p string, slot *int) route {
		return route{
			PathMatch:     pathMatch{Type: "Exact", Value: p},
			BackendGroups: []group{{Weight: 100, Backends: []backend{{Address: addr.IP.String(), Port: addr.Port}}}},
			VCLSlot:       slot,
			Priority:      100,
		}
	}
	one, outOfRange := 1, 999
	cfg := map[string]any{
		"version": 2,
		"vhosts": map[string]any{
			host: map[string]any{
				"routes": []route{
					mk("/slot", &routeSlot),
					mk("/plain", nil),
					mk("/unprogrammed", &one),
					mk("/outofrange", &outOfRange),
				},
			},
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("json.Marshal(ghost.json): %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("os.WriteFile(%s): %v", path, err)
	}
}

// vclList returns vcl.list -j as name -> label target ("" for plain VCLs).
func vclList(t *testing.T, v *varnishtest.Varnishd) map[string]string {
	t.Helper()
	out, err := v.Adm("vcl.list", "-j")
	if err != nil {
		t.Fatalf("vcl.list: %v", err)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		t.Fatalf("json.Unmarshal(vcl.list -j): %v\n%s", err, out)
	}
	res := make(map[string]string)
	for _, raw := range arr[min(3, len(arr)):] {
		var e struct {
			Name  string `json:"name"`
			Label *struct {
				Name string `json:"name"`
			} `json:"label"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("json.Unmarshal(vcl.list entry): %v", err)
		}
		target := ""
		if e.Label != nil {
			target = e.Label.Name
		}
		res[e.Name] = target
	}
	return res
}
