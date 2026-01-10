package main

import (
	_ "embed"
	"log/slog"
	"os"
	"strings"
)

//go:embed .version
var version string

const useJSONLogging = false // set to true for production/k8s

func configureLogger() {
	var handler slog.Handler
	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}
	if useJSONLogging {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(handler))
}

func main() {
	configureLogger()
	if err := run(); err != nil {
		slog.Error("sidecar failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	slog.Info("sidecar starting", "version", strings.TrimSpace(version))

	// TODO: implement sidecar logic
	// - watch for file changes (backends.conf, services.json)
	// - reload VCL on changes

	return nil
}
