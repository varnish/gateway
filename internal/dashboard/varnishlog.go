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
	vslTagRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.]*$`)
	rateLimitRe = regexp.MustCompile(`^\d+/[smh]$`)
	validGroupings = map[string]bool{
		"request": true,
		"vxid":    true,
		"session": true,
		"raw":     true,
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

// buildVarnishlogArgs constructs the argument list for varnishlog-json.
func buildVarnishlogArgs(varnishDir, query, grouping, rateLimit string, includeTags, excludeTags []string) []string {
	var args []string

	if varnishDir != "" {
		args = append(args, "-n", varnishDir)
	}
	if grouping != "" {
		args = append(args, "-g", grouping)
	}
	if query != "" {
		args = append(args, "-q", query)
	}
	if rateLimit != "" {
		args = append(args, "-R", rateLimit)
	}
	for _, tag := range includeTags {
		args = append(args, "-i", tag)
	}
	for _, tag := range excludeTags {
		args = append(args, "-x", tag)
	}
	return args
}

// parseTags splits a comma-separated tag string and validates each tag.
func parseTags(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	tags := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		if !validateVSLTag(t) {
			return nil, fmt.Errorf("invalid VSL tag: %q", t)
		}
		tags = append(tags, t)
	}
	return tags, nil
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

	args := buildVarnishlogArgs(s.varnishDir, query, grouping, rateLimit, includeTags, excludeTags)
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

	// Reap the process and inspect exit status.
	cancel()
	waitErr := cmd.Wait()

	reason := "process exited"
	if ctx.Err() == context.DeadlineExceeded {
		reason = "session timeout"
	} else if ctx.Err() == context.Canceled {
		reason = "client disconnected"
	}

	if waitErr != nil && ctx.Err() == nil {
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
