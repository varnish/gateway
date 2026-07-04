package varnishadm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is the data structure used to communicate with varnishadm.
// It listens for incoming connections from varnishd started with the -M option.
type Server struct {
	Port          uint16
	Secret        string
	logger        *slog.Logger
	reqCh         chan varnishRequest
	connected     chan struct{}
	connectedOnce sync.Once
	done          chan struct{} // closed when Run returns; signals server shutdown
	doneOnce      sync.Once
	cmdTimeout    time.Duration // Overall command timeout (default: 30s)
	rwTimeout     time.Duration // Individual socket I/O operations (default: 10s)
	authTimeout   time.Duration // Authentication operations (default: 5s)

	// stateMu guards all per-connection state below. session is created fresh
	// each time a new varnishd connects and authenticates, and closed when
	// that connection ends so anything blocked on it wakes up promptly. banner
	// and friends are rewritten on each reconnect and must be read under the
	// same lock to avoid torn observations.
	stateMu        sync.Mutex
	session        chan struct{}
	banner         string // Varnish CLI banner from the latest authenticated connection
	bannerReceived bool
	environment    string // e.g. "Darwin,24.6.0,arm64,-jnone,-smse4,-sdefault,-hcritbit"
	version        string // e.g. "varnish-7.7.3"
}

// ServerOption is a functional option for configuring a Server.
type ServerOption func(*Server)

// WithTimeouts returns a ServerOption that sets the command, read/write, and
// authentication timeouts. Zero values are replaced with defaults.
func WithTimeouts(cmd, rw, auth time.Duration) ServerOption {
	return func(s *Server) {
		if cmd > 0 {
			s.cmdTimeout = cmd
		}
		if rw > 0 {
			s.rwTimeout = rw
		}
		if auth > 0 {
			s.authTimeout = auth
		}
	}
}

// VarnishResponse is a type the maps the response
// after issuing a command against VarnishAdm.
type VarnishResponse struct {
	statusCode int
	payload    string
}

// NewVarnishResponse creates a new VarnishResponse (useful for testing)
func NewVarnishResponse(statusCode int, payload string) VarnishResponse {
	return VarnishResponse{
		statusCode: statusCode,
		payload:    payload,
	}
}

// StatusCode returns the status code of the response
func (vr VarnishResponse) StatusCode() int {
	return vr.statusCode
}

// Payload returns the payload of the response
func (vr VarnishResponse) Payload() string {
	return vr.payload
}

// CheckOK returns an error if the response status is not ClisOk.
// The format string and args are used to build the error prefix;
// the response payload is appended automatically.
func (vr VarnishResponse) CheckOK(format string, args ...any) error {
	if vr.statusCode != ClisOk {
		return fmt.Errorf(format+": %s", append(args, vr.payload)...)
	}
	return nil
}

type varnishRequest struct {
	command    string
	responseCh chan varnishResult
}

// varnishResult is delivered to a pending Exec caller. Exactly one of resp/err
// is meaningful: a non-nil err means the underlying CLI connection failed or
// was lost while the command was in flight.
type varnishResult struct {
	resp VarnishResponse
	err  error
}

// Timeout constants for different operations
const (
	defaultCmdTimeout   = 30 * time.Second // Overall command timeout
	readWriteTimeout    = 10 * time.Second // Individual socket I/O operations
	authTimeoutDuration = 5 * time.Second  // Authentication operations
	maxResponseBodyLen  = 10 * 1024 * 1024 // 10 MiB sanity limit for response body
)

func New(port uint16, secret string, logger *slog.Logger, opts ...ServerOption) *Server {
	s := &Server{
		Port:        port,
		Secret:      secret,
		logger:      logger,
		reqCh:       make(chan varnishRequest, 1),
		connected:   make(chan struct{}),
		done:        make(chan struct{}),
		cmdTimeout:  defaultCmdTimeout,
		rwTimeout:   readWriteTimeout,
		authTimeout: authTimeoutDuration,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Connected returns a channel that is closed when varnishd has connected and authenticated.
// This can be used to wait for the admin connection to be ready before sending commands.
func (v *Server) Connected() <-chan struct{} {
	return v.connected
}

// Run runs the server and waits for connections from varnishd - blocks
func (v *Server) Run(ctx context.Context) error {
	// Signal shutdown to any pending Exec callers on return.
	defer v.doneOnce.Do(func() { close(v.done) })

	v.logger.Debug("Starting varnishadm server on localhost", "port", v.Port)
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", v.Port))
	if err != nil {
		return fmt.Errorf("net.Listen(port: %d): %w", v.Port, err)
	}
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	defer l.Close()
	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				v.logger.Debug("context canceled, stopping varnishadm server")
				return nil
			default:
				return fmt.Errorf("listener.Accept(): %w", err)
			}
		}

		var remoteAddr string
		if conn != nil && conn.RemoteAddr() != nil {
			remoteAddr = conn.RemoteAddr().String()
		}
		v.logger.Debug("VarnishAdm connection established from varnishd", "remote_addr", remoteAddr)

		if err := v.handleConnection(ctx, conn); err != nil {
			v.logger.Error("Error handling connection", "error", err)
		}

		v.logger.Debug("VarnishAdm connection closed", "remote_addr", remoteAddr)
	}
}

func (v *Server) handleConnection(ctx context.Context, conn net.Conn) error {
	// make sure it is a TCP connection:
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return fmt.Errorf("conn is not a *net.TCPConn, it's a %T", conn)
	}
	defer tcpConn.Close()

	// Reset banner state under lock so concurrent GetBanner/GetVersion calls
	// can't observe a torn mid-reconnect string.
	v.stateMu.Lock()
	v.bannerReceived = false
	v.banner = ""
	v.environment = ""
	v.version = ""
	v.stateMu.Unlock()

	// Read the initial banner that Varnish sends on connection (includes authentication)
	if err := v.readBanner(tcpConn); err != nil {
		v.logger.Error("Failed to read Varnish banner or authenticate", "error", err, "local_addr", tcpConn.LocalAddr(), "remote_addr", tcpConn.RemoteAddr())
		return fmt.Errorf("banner/authentication failed: %w", err)
	}

	v.logger.Info("Varnish connected and authenticated", "version", v.GetVersion(), "remote_addr", tcpConn.RemoteAddr())

	// Install a fresh session channel for this connection.
	sessionCh := make(chan struct{})
	v.stateMu.Lock()
	v.session = sessionCh
	v.stateMu.Unlock()

	// On exit: close the session BEFORE draining reqCh so newly-arriving
	// Execs bail at queue time and don't race their requests in. Then drain
	// anything already queued and deliver a definitive error rather than
	// leaving an orphaned request in reqCh for the next varnishd to execute.
	//
	// v.session is intentionally NOT reset to nil. Leaving it pointing at the
	// now-closed channel means an Exec call entering during the dead-window
	// (between this handleConnection and the next) captures the closed
	// channel and observes the drop immediately, instead of capturing nil
	// and stalling until cmdTimeout. The next handleConnection will overwrite
	// v.session with its own fresh channel.
	defer func() {
		v.stateMu.Lock()
		close(sessionCh)
		v.stateMu.Unlock()

		reason := errors.New("varnishadm connection lost")
		if ctx.Err() != nil {
			reason = errors.New("varnishadm server stopping")
		}
		v.drainPending(reason)
	}()

	// Signal that the admin connection has been established at least once.
	// Connected() is "ever-connected" semantics by design — callers like
	// chaperone use it as a one-shot startup barrier. Per-drop notification
	// flows through the session channel above instead.
	v.connectedOnce.Do(func() {
		close(v.connected)
	})

	for {
		select {
		case <-ctx.Done():
			return nil
		case req := <-v.reqCh:
			if err := v.processRequest(tcpConn, req); err != nil {
				return err
			}
		}
	}
}

// processRequest runs one CLI command and delivers exactly one result to the
// caller's responseCh. On run() error, it returns the error so the caller
// (handleConnection) can tear down the connection. A panic anywhere in here
// is recovered and surfaced to the caller as a result, then re-raised so the
// connection is also torn down.
func (v *Server) processRequest(tcpConn *net.TCPConn, req varnishRequest) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			v.sendResult(req.responseCh, varnishResult{err: fmt.Errorf("varnishadm internal panic: %v", r)})
			returnErr = fmt.Errorf("panic in processRequest: %v", r)
		}
	}()

	resp, err := v.run(tcpConn, req.command)
	if err != nil {
		v.sendResult(req.responseCh, varnishResult{err: fmt.Errorf("varnishadm communication failed: %w", err)})
		return fmt.Errorf("run(%q): %w", req.command, err)
	}
	v.sendResult(req.responseCh, varnishResult{resp: resp})
	return nil
}

// sendResult delivers a result to a caller's responseCh non-blockingly. The
// channel is documented to be buffered (size 1) by Exec, so the send is
// expected to succeed; the default arm is a safety net for any future caller
// that forgets to buffer.
func (v *Server) sendResult(ch chan varnishResult, r varnishResult) {
	select {
	case ch <- r:
	default:
		v.logger.Warn("varnishadm response channel full or unbuffered; dropping result", "err", r.err)
	}
}

// drainPending non-blockingly drains everything currently queued in reqCh and
// fails each request with the given reason. Called from handleConnection's
// defer after the session is closed, so newly-arriving Execs see the closed
// session and bail before queuing — anything we drain here was queued before
// the session closed.
func (v *Server) drainPending(reason error) {
	for {
		select {
		case req := <-v.reqCh:
			v.sendResult(req.responseCh, varnishResult{err: reason})
		default:
			return
		}
	}
}

// readBanner reads the authentication challenge and performs authentication
// Authentication is always required since we control Varnish startup with -S secret
func (v *Server) readBanner(c *net.TCPConn) error {

	payload, statusCode, err := v.readFromConnection(c, v.authTimeout)
	if err != nil {
		v.logger.Error("Authentication challenge read failed", "error", err, "timeout_used", v.authTimeout)
		return fmt.Errorf("failed to read authentication challenge: %w", err)
	}

	// Expect authentication challenge (status 107)
	if statusCode != ClisAuth {
		return fmt.Errorf("expected authentication challenge (status %d), got status %d: %s", ClisAuth, statusCode, strings.Trim(payload, "\n"))
	}

	// Extract challenge from payload
	splitPayload := strings.Split(payload, NewLine)
	if len(splitPayload) == 0 {
		return errors.New("empty payload, could not extract auth challenge")
	}

	challenge := splitPayload[0]

	// Compute authentication response: SHA256(challenge + "\n" + secret + challenge + "\n")
	var challengeBuffer bytes.Buffer
	challengeBuffer.WriteString(challenge)
	challengeBuffer.WriteString(NewLine)
	challengeBuffer.WriteString(v.Secret)
	challengeBuffer.WriteString(challenge)
	challengeBuffer.WriteString(NewLine)

	authenticator := sha256.Sum256(challengeBuffer.Bytes())
	challengeBuffer.Reset()
	challengeBuffer.Write(authenticator[:])

	// Send auth command
	authCmd := "auth " + hex.EncodeToString(challengeBuffer.Bytes())
	authResponse, err := v.run(c, authCmd)
	challengeBuffer.Reset()

	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}
	if authResponse.statusCode != ClisOk {
		return fmt.Errorf("authentication rejected by Varnish (status %d): %s", authResponse.statusCode, authResponse.payload)
	}

	// Store the full banner and parse environment/version from the auth
	// response payload. All four fields move under stateMu so concurrent
	// GetBanner/GetEnvironment/GetVersion calls observe a consistent snapshot.
	env, version := parseBanner(authResponse.payload)
	v.stateMu.Lock()
	v.banner = authResponse.payload
	v.bannerReceived = true
	v.environment = env
	v.version = version
	v.stateMu.Unlock()

	return nil
}

// GetBanner returns the stored Varnish CLI banner
func (v *Server) GetBanner() string {
	v.stateMu.Lock()
	defer v.stateMu.Unlock()
	return v.banner
}

// GetEnvironment returns the parsed environment information
func (v *Server) GetEnvironment() string {
	v.stateMu.Lock()
	defer v.stateMu.Unlock()
	return v.environment
}

// GetVersion returns the parsed Varnish version
func (v *Server) GetVersion() string {
	v.stateMu.Lock()
	defer v.stateMu.Unlock()
	return v.version
}

var (
	reBannerEnv     = regexp.MustCompile(`(?m)^([A-Za-z0-9_]+(?:,[^,\r\n]+)+)\s*$`)
	reBannerVersion = regexp.MustCompile(`(varnish-[^\r\n]+)`)
)

// parseBanner extracts environment and version information from Varnish CLI banner
func parseBanner(banner string) (environment, version string) {
	// Extract environment line (e.g., "Linux,6.8.0-79-generic,x86_64,-jlinux,-smse4,-hcritbit")
	if envMatch := reBannerEnv.FindStringSubmatch(banner); len(envMatch) > 1 {
		environment = envMatch[1]
	}

	// Extract version line (e.g., "varnish-plus-6.0.15r1 revision d0b65fce8c712013f9bd614bacca1e67a45799e8")
	if versionMatch := reBannerVersion.FindStringSubmatch(banner); len(versionMatch) > 1 {
		version = versionMatch[1]
	}

	return
}

// Exec executes a given command and returns the output as a varnishresponse.
//
// The total deadline (cmdTimeout) covers both queueing and execution; on
// connection drop or server shutdown, the call returns promptly with an
// appropriate error rather than waiting for the timeout.
func (v *Server) Exec(cmd string) (VarnishResponse, error) {
	// Capture the live session (if any) so we can detect a connection drop
	// for the duration of this call. A nil session here means no varnishd is
	// currently connected; the request still goes onto reqCh and will be
	// served by the next handleConnection, or rejected at shutdown via done.
	v.stateMu.Lock()
	session := v.session
	v.stateMu.Unlock()

	// Buffered so handleConnection can always send without blocking even if
	// this caller has already given up via timeout, session-drop, or shutdown.
	respCh := make(chan varnishResult, 1)
	req := varnishRequest{command: cmd, responseCh: respCh}

	// Single timer shared across the queue and execute phases — total Exec
	// latency is bounded by one cmdTimeout, not two.
	timer := time.NewTimer(v.cmdTimeout)
	defer timer.Stop()

	// Step 1: get the request onto the queue.
	select {
	case v.reqCh <- req:
	case <-session:
		return VarnishResponse{}, errors.New("varnishadm connection lost before command was queued")
	case <-v.done:
		return VarnishResponse{}, errors.New("varnishadm server stopped before command was queued")
	case <-timer.C:
		v.logger.Error("Varnishadm command timed out while queuing", "command", cmd, "timeout", v.cmdTimeout)
		return VarnishResponse{}, errors.New("command timed out")
	}

	// Step 2: wait for the response.
	//
	// When the session or done signal fires, do one final non-blocking
	// respCh check before reporting "connection lost" / "server stopped".
	// Rationale: handleConnection sends the result on respCh BEFORE its
	// defer closes the session, so by the Go memory model the value is
	// visible by the time we observe the close. Without this, a successful
	// command racing a disconnect can be reported as a failure to the caller.
	select {
	case result := <-respCh:
		return v.handleResult(cmd, result)
	case <-session:
		if result, ok := tryRead(respCh); ok {
			return v.handleResult(cmd, result)
		}
		return VarnishResponse{}, errors.New("varnishadm connection lost while command was in flight")
	case <-v.done:
		if result, ok := tryRead(respCh); ok {
			return v.handleResult(cmd, result)
		}
		return VarnishResponse{}, errors.New("varnishadm server stopped while command was in flight")
	case <-timer.C:
		v.logger.Error("Varnishadm command timed out", "command", cmd, "timeout", v.cmdTimeout)
		return VarnishResponse{}, errors.New("command timed out")
	}
}

// tryRead does a non-blocking receive on ch. Returns (value, true) if a value
// was available, (zero, false) otherwise.
func tryRead(ch <-chan varnishResult) (varnishResult, bool) {
	select {
	case r := <-ch:
		return r, true
	default:
		return varnishResult{}, false
	}
}

// handleResult interprets a varnishResult delivered to an Exec caller.
func (v *Server) handleResult(cmd string, result varnishResult) (VarnishResponse, error) {
	if result.err != nil {
		v.logger.Error("Varnishadm command failed", "command", cmd, "error", result.err)
		return VarnishResponse{}, result.err
	}
	if result.resp.statusCode != ClisOk {
		v.logger.Warn("Varnishadm command non-OK", "command", cmd, "status", result.resp.statusCode, "response", result.resp.payload)
		return VarnishResponse{}, fmt.Errorf("command failed: %s", result.resp.payload)
	}
	v.logger.Debug("Varnishadm command succeeded", "command", cmd, "status", result.resp.statusCode, "payload", result.resp.payload)
	return result.resp, nil
}

// run is the internal function to execute and read a command towards varnishadm
func (v *Server) run(c *net.TCPConn, cmd string) (out VarnishResponse, err error) {
	var writeBuffer bytes.Buffer

	if len(cmd) == 0 {
		out.statusCode = ClisSyntax
		return out, errors.New("empty command given")
	}

	writeBuffer.WriteString(cmd)
	writeBuffer.WriteString(NewLine)

	// Set deadline for write operation
	deadline := time.Now().Add(v.rwTimeout)
	if err := c.SetDeadline(deadline); err != nil {
		out.statusCode = ClisComms
		return out, fmt.Errorf("failed to set write deadline: %w", err)
	}

	writtenBytes, err := c.Write(writeBuffer.Bytes())
	writeBuffer.Reset()

	if err != nil {
		out.statusCode = ClisComms
		return out, err
	}

	if writtenBytes == 0 {
		out.statusCode = ClisComms
		return out, errors.New("nothing was written on the varnish connection")
	}

	// Read response with timeout
	out.payload, out.statusCode, err = v.readFromConnection(c, v.rwTimeout)
	return out, err
}

// readFromConnection reads a response from the Varnish CLI protocol
// The protocol format is:
// - 13 byte header: "SSS LLLLLLLL\n" where SSS is status code, LLLLLLLL is body length
// - Body of exactly LLLLLLLL bytes
// - Final newline
func (v *Server) readFromConnection(conn *net.TCPConn, timeout time.Duration) (string, int, error) {

	deadline := time.Now().Add(timeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return "", 0, fmt.Errorf("conn.SetDeadline(header): %w", err)
	}
	reader := bufio.NewReader(conn)

	// Read the 13-byte header (including newline)
	header := make([]byte, 13)
	n, err := io.ReadFull(reader, header)
	if err != nil {
		return "", 0, fmt.Errorf("io.ReadFull(header): %w", err)
	}
	if n != 13 {
		return "", 0, fmt.Errorf("incomplete header: got %d bytes, expected 13", n)
	}

	// Validate header format
	if header[3] != ' ' {
		return "", 0, fmt.Errorf("invalid header format: missing space at position 3")
	}
	if header[12] != '\n' {
		return "", 0, fmt.Errorf("invalid header format: missing newline at position 12")
	}

	// Parse status code (first 3 bytes)
	statusStr := strings.TrimSpace(string(header[0:3]))
	status, err := strconv.Atoi(statusStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid status code '%s': %w", statusStr, err)
	}

	// Parse body length (bytes 4-11)
	lengthStr := strings.TrimSpace(string(header[4:12]))
	bodyLen, err := strconv.Atoi(lengthStr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid body length '%s': %w", lengthStr, err)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return "", 0, fmt.Errorf("conn.SetDeadline(body): %w", err)
	}
	if bodyLen > maxResponseBodyLen {
		return "", 0, fmt.Errorf("response body length %d exceeds maximum %d", bodyLen, maxResponseBodyLen)
	}
	// Read the body + trailing newline
	bodyWithNewline := make([]byte, bodyLen+1)
	deadline = time.Now().Add(timeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return "", 0, fmt.Errorf("conn.SetDeadline(body): %w", err)
	}
	n, err = io.ReadFull(reader, bodyWithNewline)
	if err != nil {
		return "", 0, fmt.Errorf("failed to read body: %w", err)
	}
	if n != bodyLen+1 {
		return "", 0, fmt.Errorf("incomplete body: got %d bytes, expected %d", n, bodyLen+1)
	}

	// Validate trailing newline
	if bodyWithNewline[bodyLen] != '\n' {
		return "", 0, fmt.Errorf("missing trailing newline after body")
	}

	// Remove the trailing newline from body
	body := string(bodyWithNewline[:bodyLen])

	return body, status, nil
}
