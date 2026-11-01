package varnishadm

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

// vclListJSONVarnish9 is `vcl.list -j` captured from varnishd 9.0.0 with the
// child running (busy on route_1 edited to 3): a placeholder labelled once,
// one label referenced by the active master, one label pointing at a route
// VCL.
const vclListJSONVarnish9 = `[ 2, ["vcl.list", "-j"], 1793536042.371,
  {
    "status": "available",
    "state": "auto",
    "temperature": "warm",
    "busy": 0,
    "name": "ph",
    "labels": 1
},
  {
    "status": "available",
    "state": "label",
    "temperature": "warm",
    "busy": 0,
    "name": "gwvcl-0",
    "label": {
      "name": "ph",
"refs": 1
      }
},
  {
    "status": "available",
    "state": "label",
    "temperature": "warm",
    "busy": 0,
    "name": "gwvcl-1",
    "label": {
      "name": "route_1"
      }
},
  {
    "status": "available",
    "state": "auto",
    "temperature": "warm",
    "busy": 3,
    "name": "route_1",
    "labels": 1
},
  {
    "status": "active",
    "state": "auto",
    "temperature": "warm",
    "busy": 0,
    "name": "m2"
}
]
`

func TestParseVCLListJSON(t *testing.T) {
	got, err := ParseVCLListJSON(vclListJSONVarnish9)
	if err != nil {
		t.Fatalf("ParseVCLListJSON: %v", err)
	}
	want := []VCLEntry{
		{Name: "ph", Status: "available", State: "auto", Temperature: "warm", Labels: 1},
		{Name: "gwvcl-0", Status: "available", State: "label", Temperature: "warm", LabelTarget: "ph", LabelRefs: 1},
		{Name: "gwvcl-1", Status: "available", State: "label", Temperature: "warm", LabelTarget: "route_1"},
		{Name: "route_1", Status: "available", State: "auto", Temperature: "warm", Busy: 3, Labels: 1},
		{Name: "m2", Status: "active", State: "auto", Temperature: "warm"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !got[1].IsLabel() || got[0].IsLabel() {
		t.Error("IsLabel() wrong")
	}
}

func TestParseVCLListJSON_Empty(t *testing.T) {
	// No VCL loaded yet (manager started with -f '').
	got, err := ParseVCLListJSON("[ 2, [\"vcl.list\", \"-j\"], 1793536041.611\n]\n")
	if err != nil {
		t.Fatalf("ParseVCLListJSON: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want no entries", got)
	}
}

func TestParseVCLListJSON_Malformed(t *testing.T) {
	for _, payload := range []string{
		"",
		"active   auto    warm   0   boot",
		"[2]",
		`[2, ["vcl.list","-j"], 1.0, {"status": "active"}]`,
		`[2, ["vcl.list","-j"], 1.0, "x"]`,
	} {
		if _, err := ParseVCLListJSON(payload); err == nil {
			t.Errorf("ParseVCLListJSON(%q): expected error", payload)
		}
	}
}

func TestMockVCLState_Strict(t *testing.T) {
	m := NewMock(0, "", slog.New(slog.NewTextHandler(os.Stderr, nil)))
	m.SetStrictVCL(true)
	ok := func(resp VarnishResponse, err error) {
		t.Helper()
		if err != nil || resp.StatusCode() != ClisOk {
			t.Fatalf("expected OK, got %d %q %v", resp.StatusCode(), resp.Payload(), err)
		}
	}
	refused := func(resp VarnishResponse, err error) {
		t.Helper()
		if err != nil || resp.StatusCode() == ClisOk {
			t.Fatalf("expected refusal, got %d %q %v", resp.StatusCode(), resp.Payload(), err)
		}
	}
	master := "vcl 4.1; sub vcl_recv { return (vcl(gwvcl-0)); }"
	refused(m.VCLInline("m1", master)) // label missing
	ok(m.VCLInline("ph", "vcl 4.1;"))
	ok(m.VCLLabel("gwvcl-0", "ph"))
	ok(m.VCLInline("m1", master))
	ok(m.VCLUse("m1"))
	refused(m.VCLDiscard("m1"))      // active
	refused(m.VCLDiscard("ph"))      // labelled
	refused(m.VCLDiscard("gwvcl-0")) // referenced by m1
	refused(m.VCLLabel("gwvcl-1", "nope"))
	refused(m.VCLInline("ph", "vcl 4.1;")) // duplicate

	entries, err := m.VCLListJSON()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]VCLEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if byName["gwvcl-0"].LabelTarget != "ph" || byName["gwvcl-0"].LabelRefs != 1 || byName["ph"].Labels != 1 || byName["m1"].Status != "active" {
		t.Errorf("unexpected state: %+v", entries)
	}

	// return(vcl) loops: direct (a VCL returning to the label pointed at it)
	// and through another label.
	ok(m.VCLLabel("gwvcl-1", "ph"))
	ok(m.VCLInline("self", "vcl 4.1; sub vcl_recv { return (vcl(gwvcl-1)); }"))
	resp, _ := m.VCLLabel("gwvcl-1", "self")
	if resp.StatusCode() != ClisCant || !strings.Contains(resp.Payload(), "would create a loop") {
		t.Errorf("direct loop not refused: %d %q", resp.StatusCode(), resp.Payload())
	}
	ok(m.VCLInline("a", "vcl 4.1; sub vcl_recv { return (vcl(gwvcl-1)); }"))
	ok(m.VCLInline("b", "vcl 4.1; sub vcl_recv { return (vcl(gwvcl-0)); }"))
	ok(m.VCLLabel("gwvcl-0", "a"))
	refused(m.VCLLabel("gwvcl-1", "b")) // b -> gwvcl-0 -> a -> gwvcl-1
	ok(m.VCLLabel("gwvcl-0", "ph"))
	ok(m.VCLLabel("gwvcl-1", "b")) // b -> gwvcl-0 -> ph: no loop

	m.SetVCLCompileError("BROKEN", "Message from VCC-compiler:\nsyntax error")
	resp, _ = m.VCLInline("r", "vcl 4.1; BROKEN")
	if resp.StatusCode() != ClisParam || !strings.Contains(resp.Payload(), "VCC") {
		t.Errorf("compile error not simulated: %d %q", resp.StatusCode(), resp.Payload())
	}
}
