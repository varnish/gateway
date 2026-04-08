package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	maxVarnishlogSessions = 2
	defaultSessionTimeout = 600 * time.Second
	varnishlogBufSize     = 256 * 1024 // 256KB scanner buffer
)

var (
	vslTagRe    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.]*$`)
	rateLimitRe = regexp.MustCompile(`^\d+/[smh]$`)
	validGroupings = map[string]bool{
		"request": true,
		"vxid":    true,
		"session": true,
		"raw":     true,
	}
	validModes = map[string]bool{
		"":  true,
		"b": true,
		"c": true,
	}
)

func validateVSLTag(tag string) bool {
	return vslTagRe.MatchString(tag)
}

func validateGrouping(g string) bool {
	return validGroupings[g]
}

func validateRateLimit(r string) bool {
	return rateLimitRe.MatchString(r)
}

func validateMode(m string) bool {
	return validModes[m]
}

// splitCSV splits a comma-separated string, trims whitespace, skips empties,
// and runs validate on each entry.
func splitCSV(raw string, validate func(string) error) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		if err := validate(s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// parseTags splits a comma-separated tag string and validates each tag.
func parseTags(raw string) ([]string, error) {
	return splitCSV(raw, func(s string) error {
		if !validateVSLTag(s) {
			return fmt.Errorf("invalid VSL tag: %q", s)
		}
		return nil
	})
}

// parseTagFilters splits a comma-separated filter string and validates each entry.
// Entries can be "Tag:regex" (tag prefix validated) or a bare regex (no colon,
// matches all tags — this is how varnishlog -I/-X work without a tag prefix).
func parseTagFilters(raw string) ([]string, error) {
	return splitCSV(raw, func(s string) error {
		if idx := strings.IndexByte(s, ':'); idx >= 0 {
			if !validateVSLTag(s[:idx]) {
				return fmt.Errorf("invalid VSL tag in filter: %q", s)
			}
		}
		return nil
	})
}

// varnishlogParams holds all parameters for building a varnishlog-json command.
type varnishlogParams struct {
	VarnishDir     string
	Query          string
	Grouping       string
	RateLimit      string
	Mode           string
	IncludeTags    []string
	ExcludeTags    []string
	IncludeFilters []string
	ExcludeFilters []string
}

// buildVarnishlogArgs constructs the argument list for varnishlog-json.
func buildVarnishlogArgs(p varnishlogParams) []string {
	var args []string

	if p.VarnishDir != "" {
		args = append(args, "-n", p.VarnishDir)
	}
	if p.Mode == "b" {
		args = append(args, "-b")
	} else if p.Mode == "c" {
		args = append(args, "-c")
	}
	if p.Grouping != "" {
		args = append(args, "-g", p.Grouping)
	}
	if p.Query != "" {
		args = append(args, "-q", p.Query)
	}
	if p.RateLimit != "" {
		args = append(args, "-R", p.RateLimit)
	}
	for _, tag := range p.IncludeTags {
		args = append(args, "-i", tag)
	}
	for _, tag := range p.ExcludeTags {
		args = append(args, "-x", tag)
	}
	for _, f := range p.IncludeFilters {
		args = append(args, "-I", f)
	}
	for _, f := range p.ExcludeFilters {
		args = append(args, "-X", f)
	}
	return args
}

func (s *Server) handleVarnishlog(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Enforce concurrent session limit.
	if current := s.activeSessions.Add(1); current > maxVarnishlogSessions {
		s.activeSessions.Add(-1)
		http.Error(w, fmt.Sprintf("too many active varnishlog sessions (max %d)", maxVarnishlogSessions), http.StatusTooManyRequests)
		return
	}
	defer s.activeSessions.Add(-1)

	// Parse and validate parameters.
	query := r.URL.Query().Get("q")
	grouping := r.URL.Query().Get("g")
	if grouping == "" {
		grouping = "request"
	}
	if !validateGrouping(grouping) {
		http.Error(w, fmt.Sprintf("invalid grouping %q; must be one of: request, vxid, session, raw", grouping), http.StatusBadRequest)
		return
	}

	rateLimit := r.URL.Query().Get("R")
	if rateLimit != "" && !validateRateLimit(rateLimit) {
		http.Error(w, fmt.Sprintf("invalid rate limit %q; expected format like 10/s, 100/m", rateLimit), http.StatusBadRequest)
		return
	}

	mode := r.URL.Query().Get("mode")
	if !validateMode(mode) {
		http.Error(w, fmt.Sprintf("invalid mode %q; must be empty, \"b\" (backend), or \"c\" (client)", mode), http.StatusBadRequest)
		return
	}

	includeTags, err := parseTags(r.URL.Query().Get("i"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	excludeTags, err := parseTags(r.URL.Query().Get("x"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	includeFilters, err := parseTagFilters(r.URL.Query().Get("I"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	excludeFilters, err := parseTagFilters(r.URL.Query().Get("X"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	args := buildVarnishlogArgs(varnishlogParams{
		VarnishDir:     s.varnishDir,
		Query:          query,
		Grouping:       grouping,
		RateLimit:      rateLimit,
		Mode:           mode,
		IncludeTags:    includeTags,
		ExcludeTags:    excludeTags,
		IncludeFilters: includeFilters,
		ExcludeFilters: excludeFilters,
	})
	s.logger.Info("starting varnishlog-json session", "args", args)

	// Create context with timeout; also cancelled on client disconnect.
	ctx, cancel := context.WithTimeout(r.Context(), defaultSessionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "varnishlog-json", args...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to create stdout pipe: %v", err), http.StatusInternalServerError)
		return
	}

	if err := cmd.Start(); err != nil {
		http.Error(w, fmt.Sprintf("failed to start varnishlog: %v", err), http.StatusInternalServerError)
		return
	}

	// Set SSE headers after successful process start.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	flusher.Flush()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, varnishlogBufSize), varnishlogBufSize)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		fmt.Fprintf(w, "data: %s\n\n", line)
		flusher.Flush()
	}

	// Capture the context state before cancelling, so we can distinguish
	// client disconnect / timeout from a process failure.
	ctxErr := ctx.Err()
	cancel()
	waitErr := cmd.Wait()

	reason := "process exited"
	if ctxErr == context.DeadlineExceeded {
		reason = "session timeout"
	} else if ctxErr == context.Canceled {
		reason = "client disconnected"
	}

	if waitErr != nil && ctxErr == nil {
		// Process failed on its own (not due to timeout or client disconnect).
		stderr := strings.TrimSpace(stderrBuf.String())
		s.logger.Error("varnishlog-json exited with error", "error", waitErr, "stderr", stderr, "args", args)
		if stderr != "" {
			reason = stderr
		} else {
			reason = fmt.Sprintf("varnishlog-json error: %v", waitErr)
		}
	} else {
		s.logger.Info("varnishlog-json session ended", "reason", reason)
	}

	fmt.Fprintf(w, "event: done\ndata: {\"reason\":%q}\n\n", reason)
	flusher.Flush()
}
