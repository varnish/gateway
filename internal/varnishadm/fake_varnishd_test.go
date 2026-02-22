package varnishadm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Fault types ---

// AuthFault describes a fault injected during the authentication phase.
type AuthFault int

const (
	AuthNoFault         AuthFault = iota
	FaultAuthNoChallenge          // close immediately, no challenge sent
	FaultAuthBadStatus            // send status 200 instead of 107
	FaultAuthReject               // complete challenge, reject auth response
	FaultAuthFreeze               // send challenge, then go silent
	FaultAuthPartialHeader        // send partial header bytes, then close
)

// CmdFault describes a fault injected when responding to a command.
type CmdFault int

const (
	CmdNoFault           CmdFault = iota
	FaultCmdFreeze                // read command, never respond
	FaultCmdDisconnect            // read command, close connection
	FaultCmdPartialHeader         // send partial header bytes, close
	FaultCmdPartialBody           // send header with inflated length, send partial body, close
	FaultCmdSlowResponse          // respond after configurable delay
	FaultCmdBadHeader             // send malformed header bytes
)

// CommandAction describes how to respond to a single command.
type CommandAction struct {
	Status int
	Body   string
	Fault  CmdFault
	Delay  time.Duration // used with FaultCmdSlowResponse
}

// --- FakeVarnishd ---

// FakeVarnishd simulates a varnishd process that connects to a varnishadm Server.
// It speaks the CLI protocol and can inject faults for testing.
type FakeVarnishd struct {
	t         *testing.T
	addr      string
	secret    string
	challenge string

	authFault AuthFault

	mu      sync.Mutex
	actions []CommandAction

	conn   net.Conn
	cancel context.CancelFunc
	done   chan struct{}
}

// NewFakeVarnishd creates a new FakeVarnishd that will connect to the given address.
func NewFakeVarnishd(t *testing.T, addr string, secret string) *FakeVarnishd {
	t.Helper()
	return &FakeVarnishd{
		t:         t,
		addr:      addr,
		secret:    secret,
		challenge: "abcdef1234567890abcdef1234567890",
		done:      make(chan struct{}),
	}
}

// SetAuthFault sets the fault to inject during authentication.
func (f *FakeVarnishd) SetAuthFault(fault AuthFault) {
	f.authFault = fault
}

// Enqueue adds a CommandAction to the queue. Actions are consumed in order.
func (f *FakeVarnishd) Enqueue(action CommandAction) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, action)
}

func (f *FakeVarnishd) dequeue() (CommandAction, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.actions) == 0 {
		return CommandAction{}, false
	}
	a := f.actions[0]
	f.actions = f.actions[1:]
	return a, true
}

// Start connects to the Server and begins the protocol exchange in a goroutine.
func (f *FakeVarnishd) Start(ctx context.Context) {
	f.t.Helper()
	childCtx, cancel := context.WithCancel(ctx)
	f.cancel = cancel
	go f.run(childCtx)
}

// Stop tears down the connection and waits for the goroutine to exit.
func (f *FakeVarnishd) Stop() {
	if f.cancel != nil {
		f.cancel()
	}
	if f.conn != nil {
		f.conn.Close()
	}
	<-f.done
}

func (f *FakeVarnishd) run(ctx context.Context) {
	defer close(f.done)

	var err error
	f.conn, err = net.DialTimeout("tcp", f.addr, 2*time.Second)
	if err != nil {
		f.t.Logf("FakeVarnishd: dial failed: %v", err)
		return
	}

	if !f.doAuth(ctx) {
		return
	}

	f.serveCommands(ctx)
}

// doAuth performs the authentication handshake. Returns false if the connection
// should be terminated (either due to a fault or an error).
func (f *FakeVarnishd) doAuth(ctx context.Context) bool {
	switch f.authFault {
	case FaultAuthNoChallenge:
		f.conn.Close()
		return false

	case FaultAuthBadStatus:
		_, _ = f.conn.Write(formatCLIResponse(ClisOk, "unexpected OK"))
		f.conn.Close()
		return false

	case FaultAuthFreeze:
		// Send a valid challenge, then go silent forever
		challengePayload := f.challenge + "\n\nAuthentication required."
		_, _ = f.conn.Write(formatCLIResponse(ClisAuth, challengePayload))
		// Block until context cancelled
		<-ctx.Done()
		return false

	case FaultAuthPartialHeader:
		// Send only 5 bytes of a 13-byte header, then close
		_, _ = f.conn.Write([]byte("107 "))
		f.conn.Close()
		return false

	case FaultAuthReject:
		// Send valid challenge
		challengePayload := f.challenge + "\n\nAuthentication required."
		_, _ = f.conn.Write(formatCLIResponse(ClisAuth, challengePayload))
		// Read the auth command
		reader := bufio.NewReader(f.conn)
		_, _ = reader.ReadString('\n')
		// Reject it
		_, _ = f.conn.Write(formatCLIResponse(ClisAuth, "Authentication failed"))
		f.conn.Close()
		return false
	}

	// Normal auth flow
	challengePayload := f.challenge + "\n\nAuthentication required."
	_, _ = f.conn.Write(formatCLIResponse(ClisAuth, challengePayload))

	reader := bufio.NewReader(f.conn)
	authLine, err := reader.ReadString('\n')
	if err != nil {
		f.t.Logf("FakeVarnishd: failed to read auth cmd: %v", err)
		return false
	}

	authLine = strings.TrimSpace(authLine)
	if !strings.HasPrefix(authLine, "auth ") {
		f.t.Logf("FakeVarnishd: unexpected auth line: %q", authLine)
		return false
	}

	// Verify the hash
	hexResp := strings.TrimPrefix(authLine, "auth ")
	var buf bytes.Buffer
	buf.WriteString(f.challenge)
	buf.WriteString("\n")
	buf.WriteString(f.secret)
	buf.WriteString(f.challenge)
	buf.WriteString("\n")
	expectedHash := sha256.Sum256(buf.Bytes())
	expectedHex := hex.EncodeToString(expectedHash[:])
	if hexResp != expectedHex {
		f.t.Logf("FakeVarnishd: auth hash mismatch: got %q, want %q", hexResp, expectedHex)
		_, _ = f.conn.Write(formatCLIResponse(ClisAuth, "Authentication failed"))
		return false
	}

	// Send OK + banner
	banner := "-----------------------------\nVarnish Cache CLI 1.0\n-----------------------------\n" +
		"Linux,6.8.0-79-generic,x86_64,-jlinux,-smse4,-hcritbit\n" +
		"varnish-7.5.0 revision abc123def\n\n" +
		"Type 'help' for command list.\nType 'quit' to close CLI session."
	_, _ = f.conn.Write(formatCLIResponse(ClisOk, banner))
	return true
}

// serveCommands reads commands and dispatches responses from the action queue.
func (f *FakeVarnishd) serveCommands(ctx context.Context) {
	reader := bufio.NewReader(f.conn)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Set a read deadline so we can check context periodically
		_ = f.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		line, err := reader.ReadString('\n')
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}

		_ = strings.TrimSpace(line) // consume the command

		action, ok := f.dequeue()
		if !ok {
			// Default: respond with OK
			_, _ = f.conn.Write(formatCLIResponse(ClisOk, "OK"))
			continue
		}

		f.respondToCommand(ctx, action)
	}
}

func (f *FakeVarnishd) respondToCommand(ctx context.Context, action CommandAction) {
	switch action.Fault {
	case FaultCmdFreeze:
		// Read the command but never respond — block until context cancelled
		<-ctx.Done()

	case FaultCmdDisconnect:
		f.conn.Close()

	case FaultCmdPartialHeader:
		// Send a few bytes of header, then close
		_, _ = f.conn.Write([]byte("200 "))
		f.conn.Close()

	case FaultCmdPartialBody:
		// Send a header claiming a large body, then only partial data + close
		header := fmt.Sprintf("%03d %8d\n", ClisOk, 1000) // claims 1000 bytes
		_, _ = f.conn.Write([]byte(header))
		_, _ = f.conn.Write([]byte("partial"))
		f.conn.Close()

	case FaultCmdSlowResponse:
		select {
		case <-time.After(action.Delay):
		case <-ctx.Done():
			return
		}
		status := action.Status
		if status == 0 {
			status = ClisOk
		}
		_, _ = f.conn.Write(formatCLIResponse(status, action.Body))

	case FaultCmdBadHeader:
		// Send completely garbled bytes
		_, _ = f.conn.Write([]byte("GARBAGE_BYTES"))
		f.conn.Close()

	default:
		// Normal response
		status := action.Status
		if status == 0 {
			status = ClisOk
		}
		_, _ = f.conn.Write(formatCLIResponse(status, action.Body))
	}
}

// --- Test helper ---

// testTimeouts are short timeouts used by all FakeVarnishd tests.
var testTimeouts = WithTimeouts(500*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	port := freePort(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(uint16(port), "test-secret-42", logger, testTimeouts)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	return s, addr
}

// startServer starts the server in a goroutine and returns after the listener is ready.
func startServer(t *testing.T, ctx context.Context, s *Server) chan error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx)
	}()
	// Give server time to bind
	time.Sleep(50 * time.Millisecond)
	return errCh
}

// --- Tests ---

func TestFake_NormalLifecycle(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := startServer(t, ctx, s)

	fake := NewFakeVarnishd(t, addr, "test-secret-42")
	fake.Enqueue(CommandAction{Status: ClisOk, Body: "PONG 1234567890 1.0"})
	fake.Enqueue(CommandAction{Status: ClisOk, Body: "Child in state running"})
	fake.Start(ctx)
	defer fake.Stop()

	select {
	case <-s.Connected():
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for connected signal")
	}

	resp, err := s.Exec("ping")
	if err != nil {
		t.Fatalf("Exec(ping): %v", err)
	}
	if !strings.Contains(resp.Payload(), "PONG") {
		t.Errorf("payload = %q, want containing PONG", resp.Payload())
	}

	resp, err = s.Exec("status")
	if err != nil {
		t.Fatalf("Exec(status): %v", err)
	}
	if !strings.Contains(resp.Payload(), "running") {
		t.Errorf("payload = %q, want containing running", resp.Payload())
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Errorf("Run() error: %v", err)
	}
}

func TestFake_AuthNoChallenge(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_ = startServer(t, ctx, s)

	fake := NewFakeVarnishd(t, addr, "test-secret-42")
	fake.SetAuthFault(FaultAuthNoChallenge)
	fake.Start(ctx)
	defer fake.Stop()

	// Server should handle the immediate disconnect and loop back to accept.
	// Connected should NOT be signalled.
	time.Sleep(300 * time.Millisecond)
	select {
	case <-s.Connected():
		t.Error("Connected should not fire after auth no-challenge fault")
	default:
	}

	cancel()
}

func TestFake_AuthBadStatus(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_ = startServer(t, ctx, s)

	fake := NewFakeVarnishd(t, addr, "test-secret-42")
	fake.SetAuthFault(FaultAuthBadStatus)
	fake.Start(ctx)
	defer fake.Stop()

	time.Sleep(300 * time.Millisecond)
	select {
	case <-s.Connected():
		t.Error("Connected should not fire after auth bad-status fault")
	default:
	}

	cancel()
}

func TestFake_AuthReject(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_ = startServer(t, ctx, s)

	fake := NewFakeVarnishd(t, addr, "test-secret-42")
	fake.SetAuthFault(FaultAuthReject)
	fake.Start(ctx)
	defer fake.Stop()

	time.Sleep(300 * time.Millisecond)
	select {
	case <-s.Connected():
		t.Error("Connected should not fire after auth rejection")
	default:
	}

	cancel()
}

func TestFake_AuthFreeze(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_ = startServer(t, ctx, s)

	fake := NewFakeVarnishd(t, addr, "test-secret-42")
	fake.SetAuthFault(FaultAuthFreeze)
	fake.Start(ctx)
	defer fake.Stop()

	// Server should time out on the silent varnishd (authTimeout = 200ms)
	// and loop back to accept.
	time.Sleep(500 * time.Millisecond)
	select {
	case <-s.Connected():
		t.Error("Connected should not fire after auth freeze")
	default:
	}

	cancel()
}

func TestFake_AuthPartialHeader(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_ = startServer(t, ctx, s)

	fake := NewFakeVarnishd(t, addr, "test-secret-42")
	fake.SetAuthFault(FaultAuthPartialHeader)
	fake.Start(ctx)
	defer fake.Stop()

	time.Sleep(300 * time.Millisecond)
	select {
	case <-s.Connected():
		t.Error("Connected should not fire after auth partial header")
	default:
	}

	cancel()
}

func TestFake_CmdFreeze(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = startServer(t, ctx, s)

	fake := NewFakeVarnishd(t, addr, "test-secret-42")
	fake.Enqueue(CommandAction{Fault: FaultCmdFreeze})
	fake.Start(ctx)
	defer fake.Stop()

	<-s.Connected()

	// Exec should time out (cmdTimeout = 500ms).
	// This documents weakness #1: handleConnection returns on run() error
	// without responding to the pending Exec caller.
	start := time.Now()
	_, err := s.Exec("ping")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want containing 'timed out'", err.Error())
	}
	// Should take approximately cmdTimeout (500ms), not the full default 30s
	if elapsed > 2*time.Second {
		t.Errorf("Exec took %v, expected ~500ms (cmdTimeout)", elapsed)
	}

	cancel()
}

func TestFake_CmdDisconnect(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = startServer(t, ctx, s)

	fake := NewFakeVarnishd(t, addr, "test-secret-42")
	fake.Enqueue(CommandAction{Fault: FaultCmdDisconnect})
	fake.Start(ctx)
	defer fake.Stop()

	<-s.Connected()

	_, err := s.Exec("ping")
	if err == nil {
		t.Fatal("expected error after disconnect")
	}

	cancel()
}

func TestFake_CmdPartialHeader(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = startServer(t, ctx, s)

	fake := NewFakeVarnishd(t, addr, "test-secret-42")
	fake.Enqueue(CommandAction{Fault: FaultCmdPartialHeader})
	fake.Start(ctx)
	defer fake.Stop()

	<-s.Connected()

	_, err := s.Exec("ping")
	if err == nil {
		t.Fatal("expected error for partial header")
	}

	cancel()
}

func TestFake_CmdPartialBody(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = startServer(t, ctx, s)

	fake := NewFakeVarnishd(t, addr, "test-secret-42")
	fake.Enqueue(CommandAction{Fault: FaultCmdPartialBody})
	fake.Start(ctx)
	defer fake.Stop()

	<-s.Connected()

	_, err := s.Exec("ping")
	if err == nil {
		t.Fatal("expected error for partial body")
	}

	cancel()
}

func TestFake_CmdSlowResponse(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = startServer(t, ctx, s)

	// Delay is 50ms, well within rwTimeout (200ms)
	fake := NewFakeVarnishd(t, addr, "test-secret-42")
	fake.Enqueue(CommandAction{
		Fault:  FaultCmdSlowResponse,
		Delay:  50 * time.Millisecond,
		Status: ClisOk,
		Body:   "PONG 1234567890 1.0",
	})
	fake.Start(ctx)
	defer fake.Stop()

	<-s.Connected()

	resp, err := s.Exec("ping")
	if err != nil {
		t.Fatalf("Exec(ping): %v", err)
	}
	if !strings.Contains(resp.Payload(), "PONG") {
		t.Errorf("payload = %q, want containing PONG", resp.Payload())
	}

	cancel()
}

func TestFake_CmdSlowExceedsTimeout(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = startServer(t, ctx, s)

	// Delay is 500ms, exceeds rwTimeout (200ms)
	fake := NewFakeVarnishd(t, addr, "test-secret-42")
	fake.Enqueue(CommandAction{
		Fault:  FaultCmdSlowResponse,
		Delay:  500 * time.Millisecond,
		Status: ClisOk,
		Body:   "PONG",
	})
	fake.Start(ctx)
	defer fake.Stop()

	<-s.Connected()

	_, err := s.Exec("ping")
	if err == nil {
		t.Fatal("expected timeout error")
	}

	cancel()
}

func TestFake_CmdBadHeader(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = startServer(t, ctx, s)

	fake := NewFakeVarnishd(t, addr, "test-secret-42")
	fake.Enqueue(CommandAction{Fault: FaultCmdBadHeader})
	fake.Start(ctx)
	defer fake.Stop()

	<-s.Connected()

	_, err := s.Exec("ping")
	if err == nil {
		t.Fatal("expected error for bad header")
	}

	cancel()
}

func TestFake_MultipleCommands(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = startServer(t, ctx, s)

	fake := NewFakeVarnishd(t, addr, "test-secret-42")
	fake.Enqueue(CommandAction{Status: ClisOk, Body: "PONG 1"})
	fake.Enqueue(CommandAction{Status: ClisOk, Body: "PONG 2"})
	fake.Enqueue(CommandAction{Status: ClisOk, Body: "PONG 3"})
	fake.Start(ctx)
	defer fake.Stop()

	<-s.Connected()

	for i := 1; i <= 3; i++ {
		resp, err := s.Exec("ping")
		if err != nil {
			t.Fatalf("Exec #%d: %v", i, err)
		}
		expected := fmt.Sprintf("PONG %d", i)
		if resp.Payload() != expected {
			t.Errorf("Exec #%d payload = %q, want %q", i, resp.Payload(), expected)
		}
	}

	cancel()
}

func TestFake_ReconnectAfterDrop(t *testing.T) {
	s, addr := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = startServer(t, ctx, s)

	// First connection: will disconnect on first command
	fake1 := NewFakeVarnishd(t, addr, "test-secret-42")
	fake1.Enqueue(CommandAction{Fault: FaultCmdDisconnect})
	fake1.Start(ctx)

	<-s.Connected()

	// This Exec will fail because fake1 disconnects
	_, err := s.Exec("ping")
	if err == nil {
		t.Fatal("expected error after disconnect")
	}
	fake1.Stop()

	// Give server time to loop back to Accept
	time.Sleep(100 * time.Millisecond)

	// Second connection: normal operation
	fake2 := NewFakeVarnishd(t, addr, "test-secret-42")
	fake2.Enqueue(CommandAction{Status: ClisOk, Body: "PONG after reconnect"})
	fake2.Start(ctx)
	defer fake2.Stop()

	// Wait for the new connection to authenticate.
	// Note: Connected() uses sync.Once so it's already closed from the first
	// connection — we can't re-wait on it. This documents weakness #3.
	// Instead, just give authentication time to complete.
	time.Sleep(300 * time.Millisecond)

	resp, err := s.Exec("ping")
	if err != nil {
		t.Fatalf("Exec after reconnect: %v", err)
	}
	if !strings.Contains(resp.Payload(), "reconnect") {
		t.Errorf("payload = %q, want containing 'reconnect'", resp.Payload())
	}

	cancel()
}
