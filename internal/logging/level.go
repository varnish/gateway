// Package logging provides small helpers shared across operator and chaperone
// for configuring slog from environment variables.
package logging

import (
	"log/slog"
	"strings"
)

// ParseLevel maps a LOG_LEVEL string to a slog.Level. Recognized values are
// "debug", "info", "warn"/"warning", and "error" (case-insensitive). An empty
// string is treated as a valid request for the Info default. The boolean is
// false only when the input is non-empty and not one of the recognized values,
// so callers can surface a warning while still proceeding with Info.
func ParseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return slog.LevelInfo, true
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}
