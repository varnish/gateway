package varnishstat

import (
	"testing"
)

func TestParseOutput(t *testing.T) {
	input := []byte(`{
		"version": 1,
		"timestamp": "2024-01-15T10:30:00",
		"counters": {
			"MAIN.sess_conn": {
				"description": "Sessions accepted",
				"flag": "c",
				"format": "i",
				"value": 12345
			},
			"MAIN.cache_hit": {
				"description": "Cache hits",
				"flag": "c",
				"format": "i",
				"value": 9000
			},
			"MAIN.n_object": {
				"description": "object structs made",
				"flag": "g",
				"format": "i",
				"value": 42
			},
			"SMA.s0.g_bytes": {
				"description": "Bytes outstanding",
				"flag": "g",
				"format": "B",
				"value": 1048576
			},
			"MAIN.bans": {
				"description": "Count of bans",
				"flag": "g",
				"format": "i",
				"value": 1
			},
			"VBE.boot.default.happy": {
				"description": "Happy health probes",
				"flag": "b",
				"format": "i",
				"value": 0
			}
		}
	}`)

	stats, err := parseOutput(input)
	if err != nil {
		t.Fatalf("parseOutput: %v", err)
	}

	if len(stats) != 6 {
		t.Fatalf("got %d stats, want 6", len(stats))
	}

	// Build a map for easier assertion.
	byName := make(map[string]Stat, len(stats))
	for _, s := range stats {
		byName[s.Name] = s
	}

	tests := []struct {
		name  string
		value float64
		flag  string
		desc  string
	}{
		{"MAIN.sess_conn", 12345, "c", "Sessions accepted"},
		{"MAIN.cache_hit", 9000, "c", "Cache hits"},
		{"MAIN.n_object", 42, "g", "object structs made"},
		{"SMA.s0.g_bytes", 1048576, "g", "Bytes outstanding"},
		{"MAIN.bans", 1, "g", "Count of bans"},
		{"VBE.boot.default.happy", 0, "b", "Happy health probes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ok := byName[tt.name]
			if !ok {
				t.Fatalf("stat %q not found", tt.name)
			}
			if s.Value != tt.value {
				t.Errorf("value = %v, want %v", s.Value, tt.value)
			}
			if s.Flag != tt.flag {
				t.Errorf("flag = %q, want %q", s.Flag, tt.flag)
			}
			if s.Description != tt.desc {
				t.Errorf("description = %q, want %q", s.Description, tt.desc)
			}
		})
	}
}

func TestParseOutputEmpty(t *testing.T) {
	input := []byte(`{"version": 1, "timestamp": "2024-01-15T10:30:00", "counters": {}}`)

	stats, err := parseOutput(input)
	if err != nil {
		t.Fatalf("parseOutput: %v", err)
	}
	if len(stats) != 0 {
		t.Fatalf("got %d stats, want 0", len(stats))
	}
}

func TestParseOutputInvalidJSON(t *testing.T) {
	_, err := parseOutput([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestActiveSessions(t *testing.T) {
	tests := []struct {
		name    string
		stats   []Stat
		want    int64
		wantErr bool
	}{
		{
			name: "active connections",
			stats: []Stat{
				{Name: "MAIN.sess_conn", Value: 100},
				{Name: "MAIN.sess_closed", Value: 80},
				{Name: "MAIN.sess_dropped", Value: 5},
			},
			want: 15,
		},
		{
			name: "fully drained",
			stats: []Stat{
				{Name: "MAIN.sess_conn", Value: 100},
				{Name: "MAIN.sess_closed", Value: 97},
				{Name: "MAIN.sess_dropped", Value: 3},
			},
			want: 0,
		},
		{
			name: "negative clamped to zero",
			stats: []Stat{
				{Name: "MAIN.sess_conn", Value: 10},
				{Name: "MAIN.sess_closed", Value: 8},
				{Name: "MAIN.sess_dropped", Value: 5},
			},
			want: 0,
		},
		{
			name: "missing counters",
			stats: []Stat{
				{Name: "MAIN.sess_conn", Value: 100},
			},
			wantErr: true,
		},
		{
			name:    "empty stats",
			stats:   []Stat{},
			wantErr: true,
		},
		{
			name: "extra stats ignored",
			stats: []Stat{
				{Name: "MAIN.cache_hit", Value: 9999},
				{Name: "MAIN.sess_conn", Value: 50},
				{Name: "MAIN.sess_closed", Value: 30},
				{Name: "MAIN.sess_dropped", Value: 10},
				{Name: "MAIN.cache_miss", Value: 1234},
			},
			want: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := activeSessions(tt.stats)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("activeSessions = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseOutputLargeValues(t *testing.T) {
	input := []byte(`{
		"version": 1,
		"timestamp": "2024-01-15T10:30:00",
		"counters": {
			"MAIN.client_req": {
				"description": "Good client requests received",
				"flag": "c",
				"format": "i",
				"value": 18446744073709551615
			}
		}
	}`)

	stats, err := parseOutput(input)
	if err != nil {
		t.Fatalf("parseOutput: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("got %d stats, want 1", len(stats))
	}
	if stats[0].Value <= 0 {
		t.Errorf("expected large positive value, got %v", stats[0].Value)
	}
}
