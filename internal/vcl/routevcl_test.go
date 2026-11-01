package vcl

import (
	"fmt"
	"strings"
	"testing"
)

const testGatewayUserVCL = `vcl 4.1;

import std;

sub vcl_deliver {
    set resp.http.X-Gateway-User-VCL = "yes";
}
`

const testRouteUserVCL = `vcl 4.1;

import std;
import directors;

sub vcl_recv {
    set req.http.X-Route-VCL-Recv = "1";
}

sub vcl_backend_response {
    set beresp.http.X-Route-Backend-Response = "1";
}

sub vcl_deliver {
    set resp.http.X-Which-VCL = "route";
}
`

func TestSlotLabel(t *testing.T) {
	cases := map[int]string{0: "gwvcl-0", 7: "gwvcl-7", 10: "gwvcl-10", 255: "gwvcl-255"}
	for slot, want := range cases {
		if got := SlotLabel(slot); got != want {
			t.Errorf("SlotLabel(%d) = %q, want %q", slot, got, want)
		}
	}
	if DispatchHeader != "X-Ghost-VCL-Slot" {
		t.Errorf("DispatchHeader = %q", DispatchHeader)
	}
}

func TestGenerate_NoDispatchSentinel(t *testing.T) {
	out := Generate()
	if strings.Contains(out, "{{") || strings.Contains(out, "ROUTE_VCL_DISPATCH") {
		t.Error("Generate() leaked the dispatch sentinel")
	}
	if strings.Contains(out, "return (vcl(") {
		t.Error("Generate() must not contain a dispatch block")
	}
	// The recv() line must be followed directly by the closing brace.
	if !strings.Contains(out, "    set req.backend_hint = router.recv();\n}\n") {
		t.Error("expected router.recv() to be the last statement of the preamble vcl_recv")
	}
	if !strings.Contains(out, "unset bereq.http.X-Ghost-VCL-Slot;") {
		t.Error("expected vcl_backend_fetch to strip X-Ghost-VCL-Slot from bereq")
	}
}

func TestGenerateMaster_ZeroSlotsIsFeatureOffPath(t *testing.T) {
	for _, user := range []string{"", testGatewayUserVCL, testRouteUserVCL, "sub vcl_recv { }\n"} {
		got := GenerateMaster(0, user)
		want := Merge(Generate(), user)
		if got != want {
			t.Errorf("GenerateMaster(0, %q) differs from Merge(Generate(), user)", user)
		}
	}
	// Negative pool sizes behave like zero.
	if GenerateMaster(-1, testGatewayUserVCL) != Merge(Generate(), testGatewayUserVCL) {
		t.Error("GenerateMaster(-1, ...) should equal the feature-off output")
	}
}

func TestGenerateMaster_DispatchTreeLeaves(t *testing.T) {
	for _, n := range []int{1, 2, 3, 7, 64, 256} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			out := GenerateMaster(n, testGatewayUserVCL)
			for slot := range n {
				leaf := fmt.Sprintf("== %d) { return (vcl(%s)); }", slot, SlotLabel(slot))
				if c := strings.Count(out, leaf); c != 1 {
					t.Errorf("slot %d leaf appears %d times, want 1", slot, c)
				}
			}
			if c := strings.Count(out, "return (vcl("); c != n {
				t.Errorf("%d return (vcl(...)) statements, want %d", c, n)
			}
			if strings.Contains(out, "return (vcl("+SlotLabel(n)+"))") {
				t.Errorf("label %s is outside the pool", SlotLabel(n))
			}
			if c := strings.Count(out, "unset req.http.X-Ghost-VCL-Slot;\n    }\n"); c != 1 {
				t.Errorf("expected exactly one trailing unset closing the dispatch block, got %d", c)
			}
			if strings.Count(out, "{") != strings.Count(out, "}") {
				t.Error("unbalanced braces in master VCL")
			}

			// Dispatch sits right after router.recv(), before user VCL and
			// before the postamble.
			recv := strings.Index(out, "set req.backend_hint = router.recv();")
			guard := strings.Index(out, "if (req.http.X-Ghost-VCL-Slot && req.restarts == 0 && req.esi_level == 0) {")
			hash := strings.Index(out, "sub vcl_hash {")
			user := strings.Index(out, `X-Gateway-User-VCL = "yes"`)
			if recv < 0 || guard < 0 || hash < 0 || user < 0 {
				t.Fatalf("missing marker: recv=%d guard=%d hash=%d user=%d", recv, guard, hash, user)
			}
			if !(recv < guard && guard < hash && hash < user) {
				t.Errorf("unexpected order: recv=%d guard=%d hash=%d user=%d", recv, guard, hash, user)
			}
			if !strings.HasSuffix(out, postambleVCL) {
				t.Error("master must end with the postamble")
			}
		})
	}
}

func TestGenerateMaster_DispatchTreeShape(t *testing.T) {
	got := dispatchBlock(3)
	want := `
    # Route VCL dispatch: ghost set X-Ghost-VCL-Slot for the matched route.
    # return(vcl) is illegal after a restart (503) and ESI subrequests always
    # run in the master, so guard both.
    if (req.http.X-Ghost-VCL-Slot && req.restarts == 0 && req.esi_level == 0) {
        if (std.integer(req.http.X-Ghost-VCL-Slot, -1) < 1) {
            if (std.integer(req.http.X-Ghost-VCL-Slot, -1) == 0) { return (vcl(gwvcl-0)); }
        } else {
            if (std.integer(req.http.X-Ghost-VCL-Slot, -1) < 2) {
                if (std.integer(req.http.X-Ghost-VCL-Slot, -1) == 1) { return (vcl(gwvcl-1)); }
            } else {
                if (std.integer(req.http.X-Ghost-VCL-Slot, -1) == 2) { return (vcl(gwvcl-2)); }
            }
        }
        # Slot outside the pool: stay in the master and do not leak the
        # header to user VCL or the backend.
        unset req.http.X-Ghost-VCL-Slot;
    }
`
	if got != want {
		t.Errorf("dispatchBlock(3):\n%s\nwant:\n%s", got, want)
	}
	if dispatchBlock(0) != "" {
		t.Error("dispatchBlock(0) must be empty")
	}
}

func TestGenerateMaster_TreeDepthIsLogarithmic(t *testing.T) {
	out := dispatchBlock(256)
	maxIndent := 0
	for _, line := range strings.Split(out, "\n") {
		indent := len(line) - len(strings.TrimLeft(line, " "))
		maxIndent = max(maxIndent, indent)
	}
	// Guard at depth 1, tree root at depth 2, 8 more levels for 256 leaves.
	if want := 4 * (2 + 8); maxIndent != want {
		t.Errorf("max indent %d, want %d", maxIndent, want)
	}
}

func TestGenerateRouteVCL_Composition(t *testing.T) {
	out := GenerateRouteVCL(testGatewayUserVCL, testRouteUserVCL)

	// Security requirement: the jump from the master rolls req back to the
	// client's headers, so the route VCL must strip internal headers again.
	for _, line := range []string{
		"unset req.http.X-Ghost-Pass;",
		"unset req.http.X-Ghost-VCL-Slot;",
		"unset req.http.X-Gateway-Route;",
		"unset bereq.http.X-Ghost-VCL-Slot;",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("route VCL missing %q", line)
		}
	}
	// The trust-boundary unset must precede ghost's recv().
	if strings.Index(out, "unset req.http.X-Ghost-VCL-Slot;") > strings.Index(out, "router.recv()") {
		t.Error("trust-boundary unset must come before router.recv()")
	}
	if strings.Contains(out, "return (vcl(") {
		t.Error("route VCL must not contain a dispatch block")
	}
	if c := strings.Count(out, "vcl 4."); c != 1 {
		t.Errorf("expected exactly one vcl version line, got %d", c)
	}
	if !strings.HasPrefix(out, "vcl 4.1;\n\nimport ghost;\nimport std;\nimport directors;\n\n") {
		t.Errorf("unexpected header:\n%s", out[:min(len(out), 120)])
	}
	if c := strings.Count(out, "import std;"); c != 1 {
		t.Errorf("import std; appears %d times, want 1", c)
	}

	gw := strings.Index(out, `X-Gateway-User-VCL = "yes"`)
	rt := strings.Index(out, `X-Which-VCL = "route"`)
	if gw < 0 || rt < 0 || gw > rt {
		t.Errorf("expected gateway user VCL before route VCL (gw=%d route=%d)", gw, rt)
	}
	if !strings.HasSuffix(out, postambleVCL) {
		t.Error("route VCL must end with the postamble")
	}
	if strings.Count(out, postambleVCL) != 1 {
		t.Error("postamble must appear once")
	}

	// Without gateway user VCL the route VCL is preamble + route + postamble.
	if GenerateRouteVCL("", testRouteUserVCL) != Merge(Generate(), testRouteUserVCL) {
		t.Error("GenerateRouteVCL with empty gateway VCL should equal Merge(Generate(), route)")
	}
}

func TestGeneratePlaceholder(t *testing.T) {
	out := GeneratePlaceholder()
	if strings.Contains(out, "import ") {
		t.Error("placeholder must not import VMODs")
	}
	for _, s := range []string{
		"vcl 4.1;",
		"backend dummy none;",
		`return (synth(503, "Route VCL not loaded"));`,
		`set resp.http.X-Ghost-Route-VCL = "unprogrammed";`,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("placeholder missing %q", s)
		}
	}
}

func TestGeneratePreamble_RejectsUnsafePath(t *testing.T) {
	for _, p := range []string{"", `/tmp/a"b`, `/tmp/a\b`, "/tmp/a\nb", "/tmp/a\rb"} {
		if _, err := generatePreamble(p, 0); err == nil {
			t.Errorf("generatePreamble(%q) accepted an unsafe path", p)
		}
		if _, err := generateMaster(p, 4, ""); err == nil {
			t.Errorf("generateMaster(%q) accepted an unsafe path", p)
		}
		if _, err := generateRouteVCL(p, "", ""); err == nil {
			t.Errorf("generateRouteVCL(%q) accepted an unsafe path", p)
		}
	}
	out, err := generatePreamble("/tmp/x/ghost.json", 0)
	if err != nil {
		t.Fatalf("generatePreamble: %v", err)
	}
	if !strings.Contains(out, `ghost.ghost_backend("/tmp/x/ghost.json")`) {
		t.Error("custom path not interpolated")
	}
	if want := strings.Replace(Generate(), DefaultGhostConfigPath, "/tmp/x/ghost.json", 1); out != want {
		t.Error("custom path must change nothing but the path")
	}
}
