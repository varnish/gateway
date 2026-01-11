package vcl

import (
	"strings"
	"testing"
)

func TestMerge_EmptyUserVCL(t *testing.T) {
	generated := `vcl 4.1;

import nodes;

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

func TestMerge_EnsuresNewlines(t *testing.T) {
	generated := "vcl 4.1;" // No trailing newline
	userVCL := "sub vcl_recv {}"

	result := Merge(generated, userVCL)

	// Should have newline between generated and user VCL
	if !strings.Contains(result, "vcl 4.1;\nsub vcl_recv") {
		t.Errorf("expected newline between generated and user VCL, got:\n%s", result)
	}

	// Should end with newline
	if !strings.HasSuffix(result, "\n") {
		t.Error("expected trailing newline")
	}
}

func TestMerge_PreservesUserVCLNewlines(t *testing.T) {
	generated := "vcl 4.1;\n"
	userVCL := "sub vcl_recv {\n}\n"

	result := Merge(generated, userVCL)

	// Should not add extra newlines if already present
	if strings.Contains(result, "\n\n\n") {
		t.Error("should not have triple newlines")
	}
}
