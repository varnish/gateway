// Package varnishstat provides a Go interface to the varnishstat CLI tool.
// It executes varnishstat -1 -j and parses the JSON output into structured Go types.
package varnishstat

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// Stat represents a single varnishstat counter.
type Stat struct {
	Name        string  // full name, e.g. "MAIN.sess_conn"
	Value       float64 // current value
	Flag        string  // "c" = counter, "g" = gauge, "b" = bitmap
	Description string  // human-readable description from varnishstat
}

// varnishstatOutput mirrors the JSON structure from varnishstat -1 -j.
type varnishstatOutput struct {
	Version   int                    `json:"version"`
	Timestamp string                 `json:"timestamp"`
	Counters  map[string]counterJSON `json:"counters"`
}

type counterJSON struct {
	Description string  `json:"description"`
	Flag        string  `json:"flag"`
	Format      string  `json:"format"`
	Value       float64 `json:"value"`
}

const fetchTimeout = 5 * time.Second

// Fetch runs varnishstat and returns all counters.
// varnishDir may be empty to use the default Varnish instance.
func Fetch(ctx context.Context, varnishDir string) ([]Stat, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	args := []string{"-1", "-j"}
	if varnishDir != "" {
		args = append([]string{"-n", varnishDir}, args...)
	}

	out, err := exec.CommandContext(ctx, "varnishstat", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("exec varnishstat: %w", err)
	}

	return parseOutput(out)
}

// ActiveSessions returns the number of currently active sessions by computing
// sess_conn - sess_closed - sess_dropped. Returns -1 if any required counter
// is missing (e.g., varnishd not yet started).
func ActiveSessions(ctx context.Context, varnishDir string) (int64, error) {
	stats, err := Fetch(ctx, varnishDir)
	if err != nil {
		return -1, err
	}
	return activeSessions(stats)
}

// activeSessions computes active sessions from a slice of stats.
func activeSessions(stats []Stat) (int64, error) {
	var conn, closed, dropped float64
	var found int
	for _, s := range stats {
		switch s.Name {
		case "MAIN.sess_conn":
			conn = s.Value
			found++
		case "MAIN.sess_closed":
			closed = s.Value
			found++
		case "MAIN.sess_dropped":
			dropped = s.Value
			found++
		}
	}
	if found < 3 {
		return -1, fmt.Errorf("missing session counters (found %d/3)", found)
	}
	active := int64(conn - closed - dropped)
	if active < 0 {
		active = 0
	}
	return active, nil
}

// parseOutput parses raw varnishstat JSON into a slice of Stat.
func parseOutput(data []byte) ([]Stat, error) {
	var raw varnishstatOutput
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %w", err)
	}

	stats := make([]Stat, 0, len(raw.Counters))
	for name, entry := range raw.Counters {
		stats = append(stats, Stat{
			Name:        name,
			Value:       entry.Value,
			Flag:        entry.Flag,
			Description: entry.Description,
		})
	}
	return stats, nil
}
