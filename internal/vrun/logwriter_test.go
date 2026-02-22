package vrun

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestLogWriter_SplitLine verifies that the readiness string split across two
// Write calls is correctly reassembled and the ready channel is closed.
func TestLogWriter_SplitLine(t *testing.T) {
	ready := make(chan struct{})
	lw := newLogWriter(slog.Default(), "test", ready)
	defer lw.Close()

	// Write the readiness line in two fragments
	if _, err := lw.Write([]byte("Info: Child (1234")); err != nil {
		t.Fatalf("Write fragment 1: %v", err)
	}
	if _, err := lw.Write([]byte(") said Child starts\n")); err != nil {
		t.Fatalf("Write fragment 2: %v", err)
	}

	select {
	case <-ready:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("ready channel was not closed after split readiness line")
	}
}

// TestLogWriter_CompleteLine verifies that writing the full readiness line in one
// call closes the ready channel.
func TestLogWriter_CompleteLine(t *testing.T) {
	ready := make(chan struct{})
	lw := newLogWriter(slog.Default(), "test", ready)
	defer lw.Close()

	if _, err := lw.Write([]byte("Info: Child (5678) said Child starts\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	select {
	case <-ready:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("ready channel was not closed after complete readiness line")
	}
}

// TestLogWriter_LogLevels verifies that various varnishd log prefixes are parsed
// without errors. We don't assert specific slog levels but ensure no panics or
// hangs occur.
func TestLogWriter_LogLevels(t *testing.T) {
	ready := make(chan struct{})
	lw := newLogWriter(slog.Default(), "test", ready)
	defer lw.Close()

	lines := []string{
		"Debug: some debug info\n",
		"Info: some info message\n",
		"Warning: something warned\n",
		"Warn: alternative warning\n",
		"Error: something failed\n",
		"Child launched OK\n",
		"some other output\n",
	}

	for _, line := range lines {
		if _, err := lw.Write([]byte(line)); err != nil {
			t.Fatalf("Write(%q): %v", line, err)
		}
	}

	// Give the scanner goroutine time to process
	time.Sleep(100 * time.Millisecond)

	// Ready should not have been signaled (no "Child starts" line)
	select {
	case <-ready:
		t.Fatal("ready channel should not be closed without readiness line")
	default:
		// expected
	}
}

// TestLogWriter_OversizedLine verifies that lines exceeding the default 64KB
// bufio.Scanner limit are handled correctly with the increased buffer.
func TestLogWriter_OversizedLine(t *testing.T) {
	ready := make(chan struct{})
	lw := newLogWriter(slog.Default(), "test", ready)
	defer lw.Close()

	// Create a line larger than the default 64KB scanner limit
	bigLine := "Info: " + strings.Repeat("x", 128*1024) + "\n"
	if _, err := lw.Write([]byte(bigLine)); err != nil {
		t.Fatalf("Write oversized line: %v", err)
	}

	// Write a normal line after to verify the scanner is still working
	if _, err := lw.Write([]byte("Info: Child (9999) said Child starts\n")); err != nil {
		t.Fatalf("Write readiness line: %v", err)
	}

	select {
	case <-ready:
		// success - scanner survived the oversized line
	case <-time.After(2 * time.Second):
		t.Fatal("scanner goroutine died after oversized line; ready channel never closed")
	}
}
