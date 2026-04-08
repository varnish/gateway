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
		name   string
		params varnishlogParams
		want   []string
	}{
		{
			name: "minimal",
			want: nil,
		},
		{
			name:   "with varnish dir",
			params: varnishlogParams{VarnishDir: "/var/lib/varnish/gw"},
			want:   []string{"-n", "/var/lib/varnish/gw"},
		},
		{
			name:   "with grouping",
			params: varnishlogParams{Grouping: "request"},
			want:   []string{"-g", "request"},
		},
		{
			name:   "with query",
			params: varnishlogParams{Query: "ReqURL ~ /api"},
			want:   []string{"-q", "ReqURL ~ /api"},
		},
		{
			name:   "with rate limit",
			params: varnishlogParams{RateLimit: "10/s"},
			want:   []string{"-R", "10/s"},
		},
		{
			name:   "with backend mode",
			params: varnishlogParams{Mode: "b"},
			want:   []string{"-b"},
		},
		{
			name:   "with client mode",
			params: varnishlogParams{Mode: "c"},
			want:   []string{"-c"},
		},
		{
			name:   "with include tags",
			params: varnishlogParams{IncludeTags: []string{"ReqURL", "RespStatus"}},
			want:   []string{"-i", "ReqURL", "-i", "RespStatus"},
		},
		{
			name:   "with exclude tags",
			params: varnishlogParams{ExcludeTags: []string{"VCL_Log"}},
			want:   []string{"-x", "VCL_Log"},
		},
		{
			name:   "with include filters",
			params: varnishlogParams{IncludeFilters: []string{"ReqHeader:Accept"}},
			want:   []string{"-I", "ReqHeader:Accept"},
		},
		{
			name:   "with exclude filters",
			params: varnishlogParams{ExcludeFilters: []string{"RespHeader:Set-Cookie"}},
			want:   []string{"-X", "RespHeader:Set-Cookie"},
		},
		{
			name: "full",
			params: varnishlogParams{
				VarnishDir:     "/tmp/vsm",
				Query:          "RespStatus == 503",
				Grouping:       "request",
				RateLimit:      "5/s",
				Mode:           "b",
				IncludeTags:    []string{"ReqURL"},
				ExcludeTags:    []string{"Debug"},
				IncludeFilters: []string{"ReqHeader:Host"},
				ExcludeFilters: []string{"RespHeader:X-Internal"},
			},
			want: []string{"-n", "/tmp/vsm", "-b", "-g", "request", "-q", "RespStatus == 503", "-R", "5/s", "-i", "ReqURL", "-x", "Debug", "-I", "ReqHeader:Host", "-X", "RespHeader:X-Internal"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildVarnishlogArgs(tt.params)
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

func TestParseTagFilters(t *testing.T) {
	// Valid: tag only
	filters, err := parseTagFilters("ReqHeader")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(filters, []string{"ReqHeader"}) {
		t.Errorf("got %v, want [ReqHeader]", filters)
	}

	// Valid: tag:regex
	filters, err = parseTagFilters("ReqHeader:Accept")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(filters, []string{"ReqHeader:Accept"}) {
		t.Errorf("got %v, want [ReqHeader:Accept]", filters)
	}

	// Valid: multiple
	filters, err = parseTagFilters("ReqHeader:Accept, RespHeader:Set-Cookie")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"ReqHeader:Accept", "RespHeader:Set-Cookie"}
	if !reflect.DeepEqual(filters, want) {
		t.Errorf("got %v, want %v", filters, want)
	}

	// Valid: tag with regex containing special chars
	filters, err = parseTagFilters("ReqURL:^/api/v[12]")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(filters, []string{"ReqURL:^/api/v[12]"}) {
		t.Errorf("got %v", filters)
	}

	// Empty returns nil
	filters, err = parseTagFilters("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filters != nil {
		t.Errorf("expected nil, got %v", filters)
	}

	// Valid: bare regex (no tag prefix, matches all tags)
	filters, err = parseTagFilters("example.com")
	if err != nil {
		t.Fatalf("unexpected error for bare regex: %v", err)
	}
	if !reflect.DeepEqual(filters, []string{"example.com"}) {
		t.Errorf("got %v, want [example.com]", filters)
	}

	// Valid: bare regex with special chars
	filters, err = parseTagFilters(`\.php$`)
	if err != nil {
		t.Fatalf("unexpected error for bare regex: %v", err)
	}
	if !reflect.DeepEqual(filters, []string{`\.php$`}) {
		t.Errorf("got %v", filters)
	}

	// Invalid tag part (colon present, so tag is validated)
	_, err = parseTagFilters("-bad:regex")
	if err == nil {
		t.Error("expected error for invalid tag")
	}
}

func TestValidateMode(t *testing.T) {
	for _, m := range []string{"", "b", "c"} {
		if !validateMode(m) {
			t.Errorf("expected %q to be valid", m)
		}
	}
	for _, m := range []string{"x", "bc", "B", "C"} {
		if validateMode(m) {
			t.Errorf("expected %q to be invalid", m)
		}
	}
}

func TestHandleVarnishlog_InvalidIncludeFilter(t *testing.T) {
	bus := NewEventBus(256)
	state := NewStateTracker(bus, "test")
	srv := NewServer(":0", state, bus, nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/varnishlog?I=-bad:foo", nil)
	w := httptest.NewRecorder()
	srv.handleVarnishlog(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleVarnishlog_InvalidExcludeFilter(t *testing.T) {
	bus := NewEventBus(256)
	state := NewStateTracker(bus, "test")
	srv := NewServer(":0", state, bus, nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/varnishlog?X=-bad:foo", nil)
	w := httptest.NewRecorder()
	srv.handleVarnishlog(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleVarnishlog_InvalidMode(t *testing.T) {
	bus := NewEventBus(256)
	state := NewStateTracker(bus, "test")
	srv := NewServer(":0", state, bus, nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/varnishlog?mode=x", nil)
	w := httptest.NewRecorder()
	srv.handleVarnishlog(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
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
