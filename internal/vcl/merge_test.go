package vcl

import (
	"strings"
	"testing"
)

func TestMerge_EmptyUserVCL(t *testing.T) {
	generated := `vcl 4.1;

import ghost;

sub vcl_init {
}
`
	result := Merge(generated, "")

	if result != generated {
		t.Errorf("expected generated VCL unchanged, got:\n%s", result)
	}
}

func TestMerge_WithUserVCL(t *testing.T) {
	generated := `vcl 4.1;

import ghost;

sub vcl_init {
}

# --- User VCL concatenated below ---
`
	userVCL := `sub vcl_recv {
    if (req.url ~ "^/health") {
        return (synth(200, "OK"));
    }
}
`
	result := Merge(generated, userVCL)

	if !strings.Contains(result, "vcl 4.1;") {
		t.Error("expected VCL version header")
	}
	if !strings.Contains(result, "import ghost;") {
		t.Error("expected ghost import")
	}
	if !strings.Contains(result, "sub vcl_init") {
		t.Error("expected vcl_init from generated VCL")
	}
	if !strings.Contains(result, "sub vcl_recv") {
		t.Error("expected vcl_recv from user VCL")
	}
	if !strings.Contains(result, "/health") {
		t.Error("expected user VCL content")
	}

	// Generated VCL should come before user VCL
	initIdx := strings.Index(result, "sub vcl_init")
	recvIdx := strings.Index(result, "sub vcl_recv")
	if initIdx > recvIdx {
		t.Error("expected generated VCL before user VCL")
	}
}

func TestMerge_UserVCLOverridesSubroutine(t *testing.T) {
	generated := `vcl 4.1;

import ghost;

sub vcl_backend_fetch {
    call gateway_backend_fetch;
}

# --- User VCL concatenated below ---
`
	userVCL := `sub vcl_backend_fetch {
    set bereq.http.X-Custom = "value";
}
`
	result := Merge(generated, userVCL)

	// Both vcl_backend_fetch definitions should be present
	// VCL concatenates them at compile time
	count := strings.Count(result, "sub vcl_backend_fetch")
	if count != 2 {
		t.Errorf("expected 2 vcl_backend_fetch definitions, got %d", count)
	}
}

func TestMerge_UserVCLWithImports(t *testing.T) {
	generated := `vcl 4.1;

import ghost;

sub vcl_init {
    ghost.init("/var/run/varnish/ghost.json");
}
`
	userVCL := `vcl 4.1;

import std;
import directors;

sub vcl_recv {
    std.log("request received");
}
`
	result := Merge(generated, userVCL)

	// Should have single vcl 4.1;
	count := strings.Count(result, "vcl 4.1;")
	if count != 1 {
		t.Errorf("expected 1 vcl version declaration, got %d", count)
	}

	// All imports should be present
	if !strings.Contains(result, "import ghost;") {
		t.Error("expected ghost import")
	}
	if !strings.Contains(result, "import std;") {
		t.Error("expected std import")
	}
	if !strings.Contains(result, "import directors;") {
		t.Error("expected directors import")
	}

	// Imports should come before subroutines
	lastImport := strings.LastIndex(result, "import ")
	firstSub := strings.Index(result, "sub ")
	if lastImport > firstSub {
		t.Error("imports should come before subroutines")
	}
}

func TestMerge_DuplicateImport(t *testing.T) {
	generated := `vcl 4.1;

import ghost;

sub vcl_init {
}
`
	userVCL := `import ghost;
import std;

sub vcl_recv {
}
`
	result := Merge(generated, userVCL)

	// Should have only one ghost import (no duplicates)
	count := strings.Count(result, "import ghost;")
	if count != 1 {
		t.Errorf("expected 1 ghost import, got %d", count)
	}

	// std import should still be present
	if !strings.Contains(result, "import std;") {
		t.Error("expected std import")
	}
}

func TestMerge_UserVCLNoVersion(t *testing.T) {
	generated := `vcl 4.1;

import ghost;

sub vcl_init {
}
`
	userVCL := `sub vcl_recv {
    return (pass);
}
`
	result := Merge(generated, userVCL)

	// Should have vcl version from generated
	if !strings.Contains(result, "vcl 4.1;") {
		t.Error("expected VCL version header")
	}

	// User subroutine should be present
	if !strings.Contains(result, "sub vcl_recv") {
		t.Error("expected user vcl_recv")
	}
}

func TestMerge_EndsWithNewline(t *testing.T) {
	generated := "vcl 4.1;\n\nimport ghost;\n\nsub vcl_init {}\n"
	userVCL := "sub vcl_recv {}"

	result := Merge(generated, userVCL)

	if !strings.HasSuffix(result, "\n") {
		t.Error("expected trailing newline")
	}
}

func TestParseVCL(t *testing.T) {
	vcl := `vcl 4.1;

import std;
import directors;

sub vcl_recv {
    std.log("test");
}
`
	parts := parseVCL(vcl)

	if parts.version != "vcl 4.1;" {
		t.Errorf("expected version 'vcl 4.1;', got %q", parts.version)
	}

	if len(parts.imports) != 2 {
		t.Errorf("expected 2 imports, got %d", len(parts.imports))
	}

	if !strings.Contains(parts.body, "sub vcl_recv") {
		t.Error("expected body to contain vcl_recv subroutine")
	}
}

func TestParseVCL_NoVersion(t *testing.T) {
	vcl := `import std;

sub vcl_recv {
}
`
	parts := parseVCL(vcl)

	if parts.version != "" {
		t.Errorf("expected empty version, got %q", parts.version)
	}

	if len(parts.imports) != 1 {
		t.Errorf("expected 1 import, got %d", len(parts.imports))
	}
}
