package vrun

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLogWriter_SplitLine verifies that the readiness string split across two
// Write calls is correctly reassembled and the ready channel is closed.
func TestLogWriter_SplitLine(t *testing.T) {
	ready := make(chan struct{})
	var readyOnce sync.Once
	lw := newLogWriter(slog.Default(), "test", ready, &readyOnce)
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
	var readyOnce sync.Once
	lw := newLogWriter(slog.Default(), "test", ready, &readyOnce)
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
	var readyOnce sync.Once
	lw := newLogWriter(slog.Default(), "test", ready, &readyOnce)
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
	var readyOnce sync.Once
	lw := newLogWriter(slog.Default(), "test", ready, &readyOnce)
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

// TestLogWriter_SharedReadyOnce verifies that two logWriters sharing the same
// ready channel and the same *sync.Once do not panic when the readiness line
// appears on both streams (M-17). Without a shared Once, each writer's own
// internal Once only dedupes closes within its own stream, and the second
// close(ready) from the "other" stream panics.
func TestLogWriter_SharedReadyOnce(t *testing.T) {
	ready := make(chan struct{})
	var readyOnce sync.Once

	stdout := newLogWriter(slog.Default(), "stdout", ready, &readyOnce)
	stderr := newLogWriter(slog.Default(), "stderr", ready, &readyOnce)
	defer stdout.Close()
	defer stderr.Close()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic from double-close of ready channel: %v", r)
		}
	}()

	// Simulate the readiness line showing up on BOTH stdout and stderr, as can
	// legitimately happen with varnishd/the mgt process duplicating output.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = stdout.Write([]byte("Info: Child (1111) said Child starts\n"))
	}()
	go func() {
		defer wg.Done()
		_, _ = stderr.Write([]byte("Info: Child (1111) said Child starts\n"))
	}()
	wg.Wait()

	select {
	case <-ready:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("ready channel was not closed")
	}

	// Give both scanner goroutines a moment to process; if the second close
	// were going to panic, it would have happened by now (and the deferred
	// recover above would have caught it).
	time.Sleep(100 * time.Millisecond)
}

// TestLogWriter_OverlongLineDrains verifies that a line exceeding the scanner's
// max token size does not deadlock the pipe (M-18). After a scan error, the
// goroutine must keep draining the pipe to EOF so that writes on the other
// end (varnishd) never block. We verify this by writing far more data than
// the OS pipe buffer can hold without a reader; if draining stopped, this
// write would block forever and the test would time out.
func TestLogWriter_OverlongLineDrains(t *testing.T) {
	ready := make(chan struct{})
	var readyOnce sync.Once
	lw := newLogWriter(slog.Default(), "test", ready, &readyOnce)
	defer lw.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)

		// A single "line" (no newline) larger than maxLogLineSize forces
		// bufio.Scanner to fail with ErrTooLong.
		overlong := bytes.Repeat([]byte("x"), maxLogLineSize+1)
		if _, err := lw.Write(overlong); err != nil && err != io.ErrClosedPipe {
			t.Errorf("Write(overlong): %v", err)
			return
		}

		// Keep writing well beyond typical OS pipe buffer sizes (usually
		// 64KB-1MB). If the scanner goroutine stopped draining after the
		// ErrTooLong, one of these writes would block forever and the test
		// would hang until the outer timeout fires.
		chunk := bytes.Repeat([]byte("y"), 256*1024)
		for i := 0; i < 32; i++ {
			if _, err := lw.Write(chunk); err != nil {
				t.Errorf("Write(chunk %d): %v", i, err)
				return
			}
		}
	}()

	select {
	case <-done:
		// success - all writes completed without blocking
	case <-time.After(5 * time.Second):
		t.Fatal("writes blocked after overlong line; pipe drain goroutine likely exited without draining")
	}
}
