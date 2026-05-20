package logging

import (
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in        string
		wantLevel slog.Level
		wantOK    bool
	}{
		{"", slog.LevelInfo, true},
		{"debug", slog.LevelDebug, true},
		{"DEBUG", slog.LevelDebug, true},
		{"  Debug  ", slog.LevelDebug, true},
		{"info", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"trace", slog.LevelInfo, false},
		{"nonsense", slog.LevelInfo, false},
	}
	for _, c := range cases {
		gotLevel, gotOK := ParseLevel(c.in)
		if gotLevel != c.wantLevel || gotOK != c.wantOK {
			t.Errorf("ParseLevel(%q) = (%v, %v); want (%v, %v)",
				c.in, gotLevel, gotOK, c.wantLevel, c.wantOK)
		}
	}
}
