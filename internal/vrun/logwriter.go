package vrun

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// logWriter is an io.Writer adapter that routes varnishd output through structured logging.
//
// It uses an io.Pipe internally so that a persistent bufio.Scanner can reassemble partial
// writes into complete lines. This is important because exec.Cmd may split varnishd output
// across multiple Write calls at arbitrary byte boundaries.
//
// Readiness detection is done by scraping varnishd log output for the "Child starts" message.
// This is the only reliable signal that the Varnish child process is ready to serve traffic.
// varnishadm.Connected() cannot be used as a substitute because the admin connection is
// established by the manager process before the child process has started; at that point
// the child is not yet ready to accept VCL reloads or serve HTTP requests.
type logWriter struct {
	pw *io.PipeWriter
}

// newLogWriter creates a new log writer for varnishd output.
// The ready channel is closed when varnishd signals it's ready to receive traffic.
// The caller must call Close() when done to release the scanner goroutine.
func newLogWriter(logger *slog.Logger, source string, ready chan<- struct{}) *logWriter {
	pr, pw := io.Pipe()

	var readyOnce sync.Once

	go func() {
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 1MB max token size
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			// Determine log level based on line prefix
			var level slog.Level
			switch {
			case line == "Child launched OK":
				// Info from manager process, treat as debug
				level = slog.LevelDebug
			case strings.HasPrefix(line, "Info: Child") && strings.Contains(line, "said Child starts"):
				// Info from child process about starting, treat as debug
				level = slog.LevelDebug

				// Signal readiness and log milestone
				readyOnce.Do(func() {
					close(ready)
				})
				logger.Log(context.Background(), slog.LevelInfo, "Varnish is ready to receive traffic")
			case strings.HasPrefix(line, "Debug:"):
				level = slog.LevelDebug
				line = strings.TrimSpace(strings.TrimPrefix(line, "Debug:"))
			case strings.HasPrefix(line, "Info:"):
				level = slog.LevelInfo
				line = strings.TrimSpace(strings.TrimPrefix(line, "Info:"))
			case strings.HasPrefix(line, "Warning:") || strings.HasPrefix(line, "Warn:"):
				level = slog.LevelWarn
				line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "Warning:"), "Warn:"))
			case strings.HasPrefix(line, "Error:"):
				level = slog.LevelError
				line = strings.TrimSpace(strings.TrimPrefix(line, "Error:"))
			default:
				// Default to info level for other varnishd output
				level = slog.LevelInfo
			}
			// Log with source attribution
			logger.Log(context.Background(), level, line, "source", source)
		}
		if err := scanner.Err(); err != nil {
			logger.Error("log scanner error", "source", source, "error", err)
		}
	}()

	return &logWriter{pw: pw}
}

// Write implements io.Writer interface, delegating to the pipe writer.
func (lw *logWriter) Write(p []byte) (n int, err error) {
	return lw.pw.Write(p)
}

// Close closes the pipe writer, causing the scanner goroutine to exit.
func (lw *logWriter) Close() error {
	return lw.pw.Close()
}
