package vrun

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHelperProcess is the mock varnishd entry point. When invoked as a subprocess
// with GO_WANT_HELPER_PROCESS=1, it mimics varnishd behavior based on FAKE_VARNISHD_MODE.
// During normal test runs it returns immediately (no-op).
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	mode := os.Getenv("FAKE_VARNISHD_MODE")

	switch mode {
	case "ready", "graceful_shutdown":
		// Print readiness line, wait for SIGTERM, exit 0
		fmt.Fprintln(os.Stdout, "Info: Child (99999) said Child starts")
		waitForSignal()
		os.Exit(0)

	case "crash_before_ready":
		// Exit immediately without printing readiness
		os.Exit(1)

	case "crash_after_ready":
		// Print readiness then crash
		fmt.Fprintln(os.Stdout, "Info: Child (99999) said Child starts")
		os.Exit(1)

	case "slow_startup":
		// Never print readiness, just wait for signal
		waitForSignal()
		os.Exit(0)

	case "stderr_output":
		// Write to stderr, then print readiness on stdout, wait for signal
		fmt.Fprintln(os.Stderr, "Debug: Loading configuration")
		fmt.Fprintln(os.Stderr, "Info: VCL compiled OK")
		fmt.Fprintln(os.Stdout, "Info: Child (99999) said Child starts")
		waitForSignal()
		os.Exit(0)

	default:
		fmt.Fprintf(os.Stderr, "unknown FAKE_VARNISHD_MODE: %q\n", mode)
		os.Exit(2)
	}
}

// waitForSignal blocks until SIGTERM or SIGINT is received.
func waitForSignal() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh
}

// fakeVarnishCmd returns the command and args to invoke the test binary as a fake varnishd.
// The caller must set GO_WANT_HELPER_PROCESS=1 and FAKE_VARNISHD_MODE via t.Setenv().
func fakeVarnishCmd() (cmd string, args []string) {
	return os.Args[0], []string{"-test.run=^TestHelperProcess$"}
}

// isContextError returns true if the error is caused by context cancellation or deadline.
// exec.CommandContext.Wait() returns the context's error when the context is done
// before the process exits on its own, even if the process then exits cleanly.
func isContextError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "context canceled") || strings.Contains(msg, "context deadline exceeded")
}

func TestMockStartAndReady(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("FAKE_VARNISHD_MODE", "ready")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	workDir := t.TempDir()
	mgr := New(workDir, logger, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd, args := fakeVarnishCmd()
	ready, err := mgr.Start(ctx, cmd, args)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Ready channel should close promptly
	select {
	case <-ready:
		// success
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for ready channel")
	}

	// Cancel context to trigger SIGTERM → clean exit.
	// exec.CommandContext.Wait() returns the context error when context is
	// canceled before the process exits on its own, so we expect that.
	cancel()

	err = mgr.Wait()
	if err != nil && !isContextError(err) {
		t.Fatalf("Wait returned unexpected error: %v", err)
	}
}

func TestMockCrashBeforeReady(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("FAKE_VARNISHD_MODE", "crash_before_ready")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	workDir := t.TempDir()
	mgr := New(workDir, logger, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd, args := fakeVarnishCmd()
	ready, err := mgr.Start(ctx, cmd, args)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait should return an error (exit code 1)
	err = mgr.Wait()
	if err == nil {
		t.Fatal("Wait should have returned an error for crashed process")
	}

	// Ready channel should never have closed
	select {
	case <-ready:
		t.Fatal("ready channel should not be closed when process crashes before ready")
	default:
		// expected
	}
}

func TestMockCrashAfterReady(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("FAKE_VARNISHD_MODE", "crash_after_ready")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	workDir := t.TempDir()
	mgr := New(workDir, logger, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd, args := fakeVarnishCmd()
	ready, err := mgr.Start(ctx, cmd, args)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Ready channel should close (process prints readiness before crashing)
	select {
	case <-ready:
		// success
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for ready channel")
	}

	// Wait should return an error (exit code 1)
	err = mgr.Wait()
	if err == nil {
		t.Fatal("Wait should have returned an error for crashed process")
	}
}

func TestMockGracefulShutdown(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("FAKE_VARNISHD_MODE", "graceful_shutdown")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	workDir := t.TempDir()
	mgr := New(workDir, logger, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd, args := fakeVarnishCmd()
	ready, err := mgr.Start(ctx, cmd, args)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	select {
	case <-ready:
		// success
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for ready channel")
	}

	// Cancel context → SIGTERM → process exits cleanly.
	// Wait returns the context error (expected with CommandContext).
	cancel()

	err = mgr.Wait()
	if err != nil && !isContextError(err) {
		t.Fatalf("Wait returned unexpected error after graceful shutdown: %v", err)
	}
}

func TestMockSlowStartup(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("FAKE_VARNISHD_MODE", "slow_startup")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	workDir := t.TempDir()
	mgr := New(workDir, logger, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd, args := fakeVarnishCmd()
	ready, err := mgr.Start(ctx, cmd, args)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Ready channel should NOT close within a short window
	select {
	case <-ready:
		t.Fatal("ready channel should not close for slow_startup mode")
	case <-time.After(200 * time.Millisecond):
		// expected: process hasn't printed readiness
	}

	// Kill the process via context cancel
	cancel()

	err = mgr.Wait()
	if err != nil && !isContextError(err) {
		t.Logf("Wait returned (expected): %v", err)
	}
}

func TestMockStderrOutput(t *testing.T) {
	t.Setenv("GO_WANT_HELPER_PROCESS", "1")
	t.Setenv("FAKE_VARNISHD_MODE", "stderr_output")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	workDir := t.TempDir()
	mgr := New(workDir, logger, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd, args := fakeVarnishCmd()
	ready, err := mgr.Start(ctx, cmd, args)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Ready channel should close despite stderr output happening first
	select {
	case <-ready:
		// success
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for ready channel")
	}

	cancel()

	err = mgr.Wait()
	if err != nil && !isContextError(err) {
		t.Fatalf("Wait returned unexpected error: %v", err)
	}
}
