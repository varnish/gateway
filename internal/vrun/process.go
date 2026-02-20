package vrun

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Manager manages the varnishd process lifecycle
type Manager struct {
	workDir    string
	varnishDir string
	secret     string
	logger     *slog.Logger
	cmd        *exec.Cmd
	stdoutLog  *logWriter
	stderrLog  *logWriter
}

// New creates a new Varnish manager
// If customVarnishDir is empty, defaults to workDir/varnish
func New(workDir string, logger *slog.Logger, customVarnishDir string) *Manager {
	return &Manager{
		workDir:    workDir,
		varnishDir: customVarnishDir,
		logger:     logger,
	}
}

// PrepareWorkspace sets up the varnish directory and secret file
func (m *Manager) PrepareWorkspace() error {
	if m.varnishDir != "" {
		// Create varnish directory with permissions that allow Varnish to read after dropping privileges
		if err := os.MkdirAll(m.varnishDir, 0755); err != nil {
			return fmt.Errorf("failed to create varnish directory %s: %w", m.varnishDir, err)
		}
		if err := os.Chmod(m.varnishDir, 0755); err != nil {
			return fmt.Errorf("failed to set permissions on varnish directory %s: %w", m.varnishDir, err)
		}

		m.logger.Debug("Varnish workspace prepared", "varnish_dir", m.varnishDir)
	} else {
		m.logger.Debug("Using default Varnish working directory (/var/lib/varnish)")
	}

	// Generate secret file for varnishadm authentication
	if err := m.generateSecretFile(); err != nil {
		return fmt.Errorf("failed to generate secret file: %w", err)
	}

	return nil
}

// generateSecretFile creates a cryptographically secure secret for varnishadm authentication
func (m *Manager) generateSecretFile() error {
	// Generate 32 bytes of cryptographically secure random data
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return fmt.Errorf("failed to generate random secret: %w", err)
	}

	// Store the secret as a string for later use
	m.secret = string(secretBytes)

	// Write secret to file with restrictive permissions
	secretPath := filepath.Join(m.workDir, "secret")
	if err := os.WriteFile(secretPath, secretBytes, 0600); err != nil {
		return fmt.Errorf("failed to write secret file: %w", err)
	}

	m.logger.Debug("Generated varnishadm secret file", "path", secretPath)
	return nil
}

// Start starts the varnishd process with the given arguments.
// It returns a channel that closes when varnishd is ready to receive traffic.
// Start is non-blocking; call Wait() to block until the process exits.
func (m *Manager) Start(ctx context.Context, varnishCmd string, args []string) (<-chan struct{}, error) {
	// Find varnishd executable if not specified
	if varnishCmd == "" {
		var err error
		varnishCmd, err = exec.LookPath("varnishd")
		if err != nil {
			return nil, fmt.Errorf("varnishd not found in PATH: %w", err)
		}
	}

	m.logger.Debug("Starting varnishd", "cmd", varnishCmd, "args", args)

	// Create the command, ctx lets us cancel and kill varnishd
	m.cmd = exec.CommandContext(ctx, varnishCmd, args...)
	m.cmd.Cancel = func() error {
		return m.cmd.Process.Signal(syscall.SIGTERM)
	}
	m.cmd.WaitDelay = 10 * time.Second
	if m.varnishDir != "" {
		m.cmd.Dir = m.varnishDir
	}

	// Inherit environment variables so VMOD otel can read OTEL_* configuration
	m.cmd.Env = os.Environ()

	// Create ready channel - closed when varnishd signals readiness
	ready := make(chan struct{})

	// Route varnishd output through our structured logging
	m.stdoutLog = newLogWriter(m.logger, "varnishd", ready)
	m.stderrLog = newLogWriter(m.logger, "varnishd", ready)
	m.cmd.Stdout = m.stdoutLog
	m.cmd.Stderr = m.stderrLog

	m.logger.Debug("Starting Varnish")

	// Start Varnish
	if err := m.cmd.Start(); err != nil {
		return nil, fmt.Errorf("cmd.Start: %w", err)
	}

	return ready, nil
}

// Wait blocks until the varnishd process exits.
// It returns an error if the process exits with a non-zero status.
func (m *Manager) Wait() error {
	if m.cmd == nil {
		return fmt.Errorf("varnishd not started")
	}

	err := m.cmd.Wait()
	// Close logwriters so scanner goroutines exit cleanly
	if m.stdoutLog != nil {
		m.stdoutLog.Close()
	}
	if m.stderrLog != nil {
		m.stderrLog.Close()
	}
	if err != nil {
		return fmt.Errorf("varnish process failed: %w", err)
	}
	m.logger.Info("Varnish process exited successfully")
	return nil
}

// GetSecret returns the varnishadm authentication secret
func (m *Manager) GetSecret() string {
	return m.secret
}

// GetVarnishDir returns the varnish directory path (may be empty)
func (m *Manager) GetVarnishDir() string {
	return m.varnishDir
}

// GetSecretPath returns the path to the secret file
func (m *Manager) GetSecretPath() string {
	return filepath.Join(m.workDir, "secret")
}
