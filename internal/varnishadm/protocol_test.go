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
	"testing"
	"time"
)

// formatCLIResponse formats a Varnish CLI protocol response.
// Header: "SSS LLLLLLLL\n" (3-char status, space, 8-char body length, newline)
// Body: exactly len(body) bytes followed by a trailing newline.
func formatCLIResponse(status int, body string) []byte {
	header := fmt.Sprintf("%03d %8d\n", status, len(body))
	return []byte(header + body + "\n")
}

// makeTCPPair creates a connected pair of TCP connections for testing.
// serverSide simulates the accepted connection (what readFromConnection uses).
// varnishdSide simulates varnishd writing/reading protocol bytes.
func makeTCPPair(t *testing.T) (serverSide *net.TCPConn, varnishdSide *net.TCPConn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	var accepted net.Conn
	done := make(chan struct{})
	go func() {
		accepted, _ = listener.Accept()
		close(done)
	}()

	dialed, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	<-done

	return accepted.(*net.TCPConn), dialed.(*net.TCPConn)
}

func testServer() *Server {
	return &Server{
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		cmdTimeout:  defaultCmdTimeout,
		rwTimeout:   readWriteTimeout,
		authTimeout: authTimeoutDuration,
	}
}

// freePort finds a free TCP port and returns it.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func TestNewVarnishResponse(t *testing.T) {
	resp := NewVarnishResponse(ClisOk, "test payload")
	if resp.StatusCode() != ClisOk {
		t.Errorf("StatusCode() = %d, want %d", resp.StatusCode(), ClisOk)
	}
	if resp.Payload() != "test payload" {
		t.Errorf("Payload() = %q, want %q", resp.Payload(), "test payload")
	}
}

func TestCheckOK(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		resp := NewVarnishResponse(ClisOk, "OK")
		if err := resp.CheckOK("command %s", "ping"); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("failure", func(t *testing.T) {
		resp := NewVarnishResponse(ClisCant, "cannot do that")
		err := resp.CheckOK("command %s", "stop")
		if err == nil {
			t.Fatal("expected error")
		}
		expected := "command stop: cannot do that"
		if err.Error() != expected {
			t.Errorf("error = %q, want %q", err.Error(), expected)
		}
	})

	t.Run("syntax error", func(t *testing.T) {
		resp := NewVarnishResponse(ClisSyntax, "bad syntax")
		err := resp.CheckOK("exec")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestReadFromConnection(t *testing.T) {
	s := testServer()

	t.Run("valid response", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()
		defer varnishdSide.Close()

		body := "PONG 1234567890 1.0"
		go func() {
			_, _ = varnishdSide.Write(formatCLIResponse(200, body))
		}()

		payload, status, err := s.readFromConnection(serverSide, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if status != 200 {
			t.Errorf("status = %d, want 200", status)
		}
		if payload != body {
			t.Errorf("payload = %q, want %q", payload, body)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()
		defer varnishdSide.Close()

		go func() {
			_, _ = varnishdSide.Write(formatCLIResponse(200, ""))
		}()

		payload, status, err := s.readFromConnection(serverSide, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if status != 200 {
			t.Errorf("status = %d, want 200", status)
		}
		if payload != "" {
			t.Errorf("payload = %q, want empty", payload)
		}
	})

	t.Run("auth challenge status 107", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()
		defer varnishdSide.Close()

		challenge := "abcdef1234567890\n\nAuthentication required."
		go func() {
			_, _ = varnishdSide.Write(formatCLIResponse(107, challenge))
		}()

		payload, status, err := s.readFromConnection(serverSide, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if status != 107 {
			t.Errorf("status = %d, want 107", status)
		}
		if payload != challenge {
			t.Errorf("payload = %q, want %q", payload, challenge)
		}
	})

	t.Run("short header EOF", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()

		go func() {
			_, _ = varnishdSide.Write([]byte("200 "))
			varnishdSide.Close()
		}()

		_, _, err := s.readFromConnection(serverSide, 5*time.Second)
		if err == nil {
			t.Fatal("expected error for short header")
		}
	})

	t.Run("invalid space at position 3", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()
		defer varnishdSide.Close()

		go func() {
			// "200X" instead of "200 " at bytes 0-3
			_, _ = varnishdSide.Write([]byte("200X       4\nPONG\n"))
		}()

		_, _, err := s.readFromConnection(serverSide, 5*time.Second)
		if err == nil {
			t.Fatal("expected error for invalid space position")
		}
		if !strings.Contains(err.Error(), "missing space at position 3") {
			t.Errorf("error = %q, want 'missing space' message", err.Error())
		}
	})

	t.Run("invalid newline at position 12", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()
		defer varnishdSide.Close()

		go func() {
			// Byte 12 is 'X' instead of '\n'
			_, _ = varnishdSide.Write([]byte("200        4XPONG\n"))
		}()

		_, _, err := s.readFromConnection(serverSide, 5*time.Second)
		if err == nil {
			t.Fatal("expected error for invalid newline position")
		}
		if !strings.Contains(err.Error(), "missing newline at position 12") {
			t.Errorf("error = %q, want 'missing newline' message", err.Error())
		}
	})

	t.Run("non-numeric status code", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()
		defer varnishdSide.Close()

		go func() {
			_, _ = varnishdSide.Write([]byte("abc        4\nPONG\n"))
		}()

		_, _, err := s.readFromConnection(serverSide, 5*time.Second)
		if err == nil {
			t.Fatal("expected error for non-numeric status")
		}
		if !strings.Contains(err.Error(), "invalid status code") {
			t.Errorf("error = %q, want 'invalid status code' message", err.Error())
		}
	})

	t.Run("non-numeric body length", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()
		defer varnishdSide.Close()

		go func() {
			_, _ = varnishdSide.Write([]byte("200 bad_len_\nPONG\n"))
		}()

		_, _, err := s.readFromConnection(serverSide, 5*time.Second)
		if err == nil {
			t.Fatal("expected error for non-numeric body length")
		}
		if !strings.Contains(err.Error(), "invalid body length") {
			t.Errorf("error = %q, want 'invalid body length' message", err.Error())
		}
	})

	t.Run("body length exceeds maximum", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()
		defer varnishdSide.Close()

		go func() {
			header := fmt.Sprintf("200 %8d\n", 11*1024*1024) // 11 MiB > 10 MiB limit
			_, _ = varnishdSide.Write([]byte(header))
		}()

		_, _, err := s.readFromConnection(serverSide, 5*time.Second)
		if err == nil {
			t.Fatal("expected error for oversized body")
		}
		if !strings.Contains(err.Error(), "exceeds maximum") {
			t.Errorf("error = %q, want 'exceeds maximum' message", err.Error())
		}
	})

	t.Run("truncated body", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()

		go func() {
			header := fmt.Sprintf("200 %8d\n", 100) // promises 100 bytes
			_, _ = varnishdSide.Write([]byte(header))
			_, _ = varnishdSide.Write([]byte("short")) // only 5 bytes
			varnishdSide.Close()
		}()

		_, _, err := s.readFromConnection(serverSide, 5*time.Second)
		if err == nil {
			t.Fatal("expected error for truncated body")
		}
	})

	t.Run("missing trailing newline", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()
		defer varnishdSide.Close()

		go func() {
			// Body = "PONG" (4 bytes), trailing byte = 'X' instead of '\n'
			_, _ = varnishdSide.Write([]byte("200        4\nPONGX"))
		}()

		_, _, err := s.readFromConnection(serverSide, 5*time.Second)
		if err == nil {
			t.Fatal("expected error for missing trailing newline")
		}
		if !strings.Contains(err.Error(), "missing trailing newline") {
			t.Errorf("error = %q, want 'missing trailing newline' message", err.Error())
		}
	})

	t.Run("large valid response 64KB", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()
		defer varnishdSide.Close()

		body := strings.Repeat("x", 64*1024)
		go func() {
			_, _ = varnishdSide.Write(formatCLIResponse(200, body))
		}()

		payload, status, err := s.readFromConnection(serverSide, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if status != 200 {
			t.Errorf("status = %d, want 200", status)
		}
		if len(payload) != 64*1024 {
			t.Errorf("payload len = %d, want %d", len(payload), 64*1024)
		}
	})

	t.Run("various status codes", func(t *testing.T) {
		codes := []int{100, 101, 102, 200, 201, 300, 400, 500}
		for _, code := range codes {
			t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
				serverSide, varnishdSide := makeTCPPair(t)
				defer serverSide.Close()
				defer varnishdSide.Close()

				go func() {
					_, _ = varnishdSide.Write(formatCLIResponse(code, "body"))
				}()

				_, status, err := s.readFromConnection(serverSide, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				if status != code {
					t.Errorf("status = %d, want %d", status, code)
				}
			})
		}
	})
}

func TestRun(t *testing.T) {
	s := testServer()

	t.Run("empty command", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()
		defer varnishdSide.Close()

		resp, err := s.run(serverSide, "")
		if err == nil {
			t.Fatal("expected error for empty command")
		}
		if resp.statusCode != ClisSyntax {
			t.Errorf("status = %d, want %d", resp.statusCode, ClisSyntax)
		}
	})

	t.Run("valid command roundtrip", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()
		defer varnishdSide.Close()

		go func() {
			// Read command from server
			buf := make([]byte, 4096)
			n, err := varnishdSide.Read(buf)
			if err != nil {
				return
			}
			cmd := strings.TrimSpace(string(buf[:n]))
			if cmd != "ping" {
				return
			}
			_, _ = varnishdSide.Write(formatCLIResponse(200, "PONG 1234567890 1.0"))
		}()

		resp, err := s.run(serverSide, "ping")
		if err != nil {
			t.Fatal(err)
		}
		if resp.statusCode != 200 {
			t.Errorf("status = %d, want 200", resp.statusCode)
		}
		if resp.payload != "PONG 1234567890 1.0" {
			t.Errorf("payload = %q, want PONG", resp.payload)
		}
	})

	t.Run("write to closed connection", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		varnishdSide.Close()
		serverSide.Close()

		_, err := s.run(serverSide, "ping")
		if err == nil {
			t.Fatal("expected error writing to closed connection")
		}
	})

	t.Run("error status in response", func(t *testing.T) {
		serverSide, varnishdSide := makeTCPPair(t)
		defer serverSide.Close()
		defer varnishdSide.Close()

		go func() {
			buf := make([]byte, 4096)
			_, _ = varnishdSide.Read(buf)
			_, _ = varnishdSide.Write(formatCLIResponse(300, "Child not running"))
		}()

		resp, err := s.run(serverSide, "stop")
		if err != nil {
			t.Fatal(err)
		}
		if resp.statusCode != 300 {
			t.Errorf("status = %d, want 300", resp.statusCode)
		}
		if resp.payload != "Child not running" {
			t.Errorf("payload = %q", resp.payload)
		}
	})
}

func TestNew(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(6082, "secret123", logger)

	if s.Port != 6082 {
		t.Errorf("Port = %d, want 6082", s.Port)
	}
	if s.Secret != "secret123" {
		t.Errorf("Secret = %q, want %q", s.Secret, "secret123")
	}
	if s.reqCh == nil {
		t.Error("reqCh is nil")
	}
	if s.connected == nil {
		t.Error("connected is nil")
	}
}

func TestConnected(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(6082, "secret", logger)

	ch := s.Connected()
	if ch == nil {
		t.Fatal("Connected() returned nil")
	}

	// Channel should not be closed yet (no connection established)
	select {
	case <-ch:
		t.Error("Connected channel should not be closed before authentication")
	default:
		// expected
	}
}

func TestServerGetters_Fresh(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(6082, "secret", logger)

	if s.GetBanner() != "" {
		t.Errorf("GetBanner() = %q, want empty", s.GetBanner())
	}
	if s.GetEnvironment() != "" {
		t.Errorf("GetEnvironment() = %q, want empty", s.GetEnvironment())
	}
	if s.GetVersion() != "" {
		t.Errorf("GetVersion() = %q, want empty", s.GetVersion())
	}
}

// TestServerFullLifecycle tests the complete server lifecycle:
// listening, accepting a connection from varnishd, authentication,
// command execution, and clean shutdown.
func TestServerFullLifecycle(t *testing.T) {
	port := freePort(t)
	secret := "test-secret-42"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(uint16(port), secret, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Run(ctx)
	}()

	// Give the server time to start listening
	time.Sleep(100 * time.Millisecond)

	// Connect as varnishd
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)

	// Step 1: Send auth challenge (status 107)
	challenge := "abcdef1234567890abcdef1234567890"
	challengePayload := challenge + "\n\nAuthentication required.\nPlease use the 'auth' command."
	_, err = conn.Write(formatCLIResponse(ClisAuth, challengePayload))
	if err != nil {
		t.Fatal(err)
	}

	// Step 2: Read auth command from server
	authLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	authLine = strings.TrimSpace(authLine)

	if !strings.HasPrefix(authLine, "auth ") {
		t.Fatalf("expected 'auth <hex>', got %q", authLine)
	}

	// Verify the SHA256 computation
	hexResponse := strings.TrimPrefix(authLine, "auth ")
	var challengeBuffer bytes.Buffer
	challengeBuffer.WriteString(challenge)
	challengeBuffer.WriteString("\n")
	challengeBuffer.WriteString(secret)
	challengeBuffer.WriteString(challenge)
	challengeBuffer.WriteString("\n")
	expectedHash := sha256.Sum256(challengeBuffer.Bytes())
	expectedHex := hex.EncodeToString(expectedHash[:])

	if hexResponse != expectedHex {
		t.Errorf("auth hash = %q, want %q", hexResponse, expectedHex)
	}

	// Step 3: Send OK response with banner
	banner := "-----------------------------\nVarnish Cache CLI 1.0\n-----------------------------\n" +
		"Linux,6.8.0-79-generic,x86_64,-jlinux,-smse4,-hcritbit\n" +
		"varnish-7.5.0 revision abc123def\n\n" +
		"Type 'help' for command list.\nType 'quit' to close CLI session."
	_, err = conn.Write(formatCLIResponse(ClisOk, banner))
	if err != nil {
		t.Fatal(err)
	}

	// Wait for connected signal
	select {
	case <-server.Connected():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for connected signal")
	}

	// Verify parsed banner data
	if !strings.Contains(server.GetVersion(), "varnish-7.5.0") {
		t.Errorf("GetVersion() = %q, want containing varnish-7.5.0", server.GetVersion())
	}
	if !strings.Contains(server.GetEnvironment(), "Linux") {
		t.Errorf("GetEnvironment() = %q, want containing Linux", server.GetEnvironment())
	}
	if server.GetBanner() == "" {
		t.Error("GetBanner() is empty after authentication")
	}

	// Step 4: Start command responder goroutine
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.TrimSpace(line)
			switch cmd {
			case "ping":
				_, _ = conn.Write(formatCLIResponse(ClisOk, "PONG 1234567890 1.0"))
			case "status":
				_, _ = conn.Write(formatCLIResponse(ClisOk, "Child in state running"))
			default:
				_, _ = conn.Write(formatCLIResponse(ClisOk, "OK"))
			}
		}
	}()

	// Step 5: Execute commands through the server
	resp, err := server.Exec("ping")
	if err != nil {
		t.Fatalf("Exec(ping) error: %v", err)
	}
	if resp.StatusCode() != ClisOk {
		t.Errorf("ping status = %d, want %d", resp.StatusCode(), ClisOk)
	}
	if !strings.Contains(resp.Payload(), "PONG") {
		t.Errorf("ping payload = %q, want containing PONG", resp.Payload())
	}

	resp, err = server.Exec("status")
	if err != nil {
		t.Fatalf("Exec(status) error: %v", err)
	}
	if !strings.Contains(resp.Payload(), "running") {
		t.Errorf("status payload = %q, want containing running", resp.Payload())
	}

	// Step 6: Clean shutdown
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run() returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Run() did not stop after cancel")
	}
}

// TestServerExecNonOKStatus verifies that Exec returns an error when
// varnishd responds with a non-200 status code.
func TestServerExecNonOKStatus(t *testing.T) {
	port := freePort(t)
	secret := "test-secret"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(uint16(port), secret, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		_ = server.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)

	// Auth dance
	challenge := "testchallenge123"
	_, _ = conn.Write(formatCLIResponse(ClisAuth, challenge+"\n"))
	_, _ = reader.ReadString('\n') // read auth command
	_, _ = conn.Write(formatCLIResponse(ClisOk, "varnish-7.5.0\nLinux,x86_64"))

	<-server.Connected()

	// Respond with error status
	go func() {
		_, _ = reader.ReadString('\n') // read command
		_, _ = conn.Write(formatCLIResponse(ClisCant, "Child not running"))
	}()

	_, err = server.Exec("stop")
	if err == nil {
		t.Error("Exec should return error for non-OK status")
	}
	if !strings.Contains(err.Error(), "command failed") {
		t.Errorf("error = %q, want containing 'command failed'", err.Error())
	}

	cancel()
}

// TestServerAuthRejection verifies that the server handles authentication
// rejection gracefully and continues accepting new connections.
func TestServerAuthRejection(t *testing.T) {
	port := freePort(t)
	secret := "my-secret"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(uint16(port), secret, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)

	// First connection: reject auth
	conn1, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	reader1 := bufio.NewReader(conn1)

	_, _ = conn1.Write(formatCLIResponse(ClisAuth, "challenge1\n"))
	_, _ = reader1.ReadString('\n') // read auth command
	_, _ = conn1.Write(formatCLIResponse(ClisAuth, "Authentication failed"))
	conn1.Close()

	// Give server time to handle rejection and loop back to Accept
	time.Sleep(100 * time.Millisecond)

	// Second connection: succeed
	conn2, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	reader2 := bufio.NewReader(conn2)

	_, _ = conn2.Write(formatCLIResponse(ClisAuth, "challenge2\n"))
	_, _ = reader2.ReadString('\n') // read auth command
	_, _ = conn2.Write(formatCLIResponse(ClisOk, "varnish-7.5.0\nLinux,x86_64"))

	select {
	case <-server.Connected():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for connected after second connection")
	}

	cancel()
}

// TestServerBadInitialStatus tests the server receiving an unexpected status
// code on initial connection (not 107 auth challenge).
func TestServerBadInitialStatus(t *testing.T) {
	port := freePort(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(uint16(port), "secret", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		_ = server.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}

	// Send status 200 instead of 107 — server should reject this
	_, _ = conn.Write(formatCLIResponse(ClisOk, "Unexpected OK"))
	conn.Close()

	// Server should have logged the error and looped back to Accept.
	// Connected channel should NOT be closed.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-server.Connected():
		t.Error("Connected channel should not be closed after bad initial status")
	default:
		// expected
	}

	cancel()
}

// TestServerRunListenError tests that Run returns an error when it cannot listen.
func TestServerRunListenError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Port 1 is privileged and should fail to bind for unprivileged users.
	// If running as root, skip.
	server := New(1, "secret", logger)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := server.Run(ctx)
	if err == nil {
		t.Skip("Run succeeded on port 1 (likely running as root)")
	}
}
