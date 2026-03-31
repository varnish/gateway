package dashboard

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestValidateVSLTag(t *testing.T) {
	valid := []string{"ReqURL", "RespStatus", "ReqHeader", "Timestamp", "Begin", "VCL_Log", "Req.Header"}
	for _, tag := range valid {
		if !validateVSLTag(tag) {
			t.Errorf("expected %q to be valid", tag)
		}
	}
	invalid := []string{"", "123", "-ReqURL", "Req URL", "Req;URL", "Req\nURL"}
	for _, tag := range invalid {
		if validateVSLTag(tag) {
			t.Errorf("expected %q to be invalid", tag)
		}
	}
}

func TestValidateGrouping(t *testing.T) {
	for _, g := range []string{"request", "vxid", "session", "raw"} {
		if !validateGrouping(g) {
			t.Errorf("expected %q to be valid", g)
		}
	}
	for _, g := range []string{"", "invalid", "Request", "GROUP"} {
		if validateGrouping(g) {
			t.Errorf("expected %q to be invalid", g)
		}
	}
}

func TestValidateRateLimit(t *testing.T) {
	valid := []string{"10/s", "100/m", "1/h", "999/s"}
	for _, r := range valid {
		if !validateRateLimit(r) {
			t.Errorf("expected %q to be valid", r)
		}
	}
	invalid := []string{"", "10", "10/", "/s", "10/x", "abc/s", "10/ss"}
	for _, r := range invalid {
		if validateRateLimit(r) {
			t.Errorf("expected %q to be invalid", r)
		}
	}
}

func TestBuildVarnishlogArgs(t *testing.T) {
	tests := []struct {
		name        string
		varnishDir  string
		query       string
		grouping    string
		rateLimit   string
		includeTags []string
		excludeTags []string
		want        []string
	}{
		{
			name: "minimal",
			want: nil,
		},
		{
			name:       "with varnish dir",
			varnishDir: "/var/lib/varnish/gw",
			want:       []string{"-n", "/var/lib/varnish/gw"},
		},
		{
			name:     "with grouping",
			grouping: "request",
			want:     []string{"-g", "request"},
		},
		{
			name:  "with query",
			query: "ReqURL ~ /api",
			want:  []string{"-q", "ReqURL ~ /api"},
		},
		{
			name:      "with rate limit",
			rateLimit: "10/s",
			want:      []string{"-R", "10/s"},
		},
		{
			name:        "with include tags",
			includeTags: []string{"ReqURL", "RespStatus"},
			want:        []string{"-i", "ReqURL", "-i", "RespStatus"},
		},
		{
			name:        "with exclude tags",
			excludeTags: []string{"VCL_Log"},
			want:        []string{"-x", "VCL_Log"},
		},
		{
			name:        "full",
			varnishDir:  "/tmp/vsm",
			query:       "RespStatus == 503",
			grouping:    "request",
			rateLimit:   "5/s",
			includeTags: []string{"ReqURL"},
			excludeTags: []string{"Debug"},
			want:        []string{"-n", "/tmp/vsm", "-g", "request", "-q", "RespStatus == 503", "-R", "5/s", "-i", "ReqURL", "-x", "Debug"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildVarnishlogArgs(tt.varnishDir, tt.query, tt.grouping, tt.rateLimit, tt.includeTags, tt.excludeTags)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildVarnishlogArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseTags(t *testing.T) {
	tags, err := parseTags("ReqURL,RespStatus,ReqHeader")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"ReqURL", "RespStatus", "ReqHeader"}
	if !reflect.DeepEqual(tags, want) {
		t.Errorf("parseTags() = %v, want %v", tags, want)
	}

	// Empty returns nil.
	tags, err = parseTags("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tags != nil {
		t.Errorf("expected nil, got %v", tags)
	}

	// Spaces trimmed.
	tags, err = parseTags("ReqURL , RespStatus")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want = []string{"ReqURL", "RespStatus"}
	if !reflect.DeepEqual(tags, want) {
		t.Errorf("parseTags() = %v, want %v", tags, want)
	}

	// Invalid tag.
	_, err = parseTags("ReqURL,-invalid")
	if err == nil {
		t.Error("expected error for invalid tag")
	}
}

func TestHandleVarnishlog_SessionLimit(t *testing.T) {
	bus := NewEventBus(256)
	state := NewStateTracker(bus, "test")
	srv := NewServer(":0", state, bus, nil, "")

	// Simulate maxVarnishlogSessions already active.
	srv.activeSessions.Store(int32(maxVarnishlogSessions))

	req := httptest.NewRequest(http.MethodGet, "/api/varnishlog", nil)
	w := httptest.NewRecorder()
	srv.handleVarnishlog(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", w.Code)
	}
}

func TestHandleVarnishlog_InvalidGrouping(t *testing.T) {
	bus := NewEventBus(256)
	state := NewStateTracker(bus, "test")
	srv := NewServer(":0", state, bus, nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/varnishlog?g=bogus", nil)
	w := httptest.NewRecorder()
	srv.handleVarnishlog(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleVarnishlog_InvalidRateLimit(t *testing.T) {
	bus := NewEventBus(256)
	state := NewStateTracker(bus, "test")
	srv := NewServer(":0", state, bus, nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/varnishlog?R=bogus", nil)
	w := httptest.NewRecorder()
	srv.handleVarnishlog(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleVarnishlog_InvalidTags(t *testing.T) {
	bus := NewEventBus(256)
	state := NewStateTracker(bus, "test")
	srv := NewServer(":0", state, bus, nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/varnishlog?i=-bad", nil)
	w := httptest.NewRecorder()
	srv.handleVarnishlog(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}
