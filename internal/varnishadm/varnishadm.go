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
	Port           uint16
	Secret         string
	logger         *slog.Logger
	reqCh          chan varnishRequest
	banner         string // Stores the Varnish CLI banner received on connection
	bannerReceived bool   // Tracks if banner has been read for this connection
	environment    string // Stores the environment line (e.g., "Darwin,24.6.0,arm64,-jnone,-smse4,-sdefault,-hcritbit")
	version        string // Stores the Varnish version (e.g., "varnish-7.7.3")
	connected      chan struct{}
	connectedOnce  sync.Once
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
	command      string
	responseChan chan VarnishResponse
}

// Timeout constants for different operations
const (
	defaultCmdTimeout = 30 * time.Second // Overall command timeout
	readWriteTimeout  = 10 * time.Second // Individual socket I/O operations
	authTimeout       = 5 * time.Second  // Authentication operations
)

func New(port uint16, secret string, logger *slog.Logger) *Server {
	return &Server{
		Port:      port,
		Secret:    secret,
		logger:    logger,
		reqCh:     make(chan varnishRequest, 1),
		connected: make(chan struct{}),
	}
}

// Connected returns a channel that is closed when varnishd has connected and authenticated.
// This can be used to wait for the admin connection to be ready before sending commands.
func (v *Server) Connected() <-chan struct{} {
	return v.connected
}

// Run runs the server and waits for connections from varnishd - blocks
func (v *Server) Run(ctx context.Context) error {
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

	v.bannerReceived = false // Reset banner state for new connection
	v.banner = ""

	// Read the initial banner that Varnish sends on connection (includes authentication)
	if err := v.readBanner(tcpConn); err != nil {
		v.logger.Error("Failed to read Varnish banner or authenticate", "error", err, "local_addr", tcpConn.LocalAddr(), "remote_addr", tcpConn.RemoteAddr())
		return fmt.Errorf("banner/authentication failed: %w", err)
	}

	v.logger.Info("Varnish connected and authenticated", "version", v.version, "remote_addr", tcpConn.RemoteAddr())

	// Signal that connection is established
	v.connectedOnce.Do(func() {
		close(v.connected)
	})

	for {
		select {
		case <-ctx.Done():
			return nil
		case req := <-v.reqCh:
			resp, err := v.run(tcpConn, req.command)
			if err != nil {
				return fmt.Errorf("readFromConnection: %w", err)
			}
			if resp.statusCode != ClisOk {
				v.logger.Warn("command failed", "command", req.command, "status", resp.statusCode, "payload", resp.payload)
			}
			req.responseChan <- resp
		}
	}
}

// readBanner reads the authentication challenge and performs authentication
// Authentication is always required since we control Varnish startup with -S secret
func (v *Server) readBanner(c *net.TCPConn) error {

	payload, statusCode, err := v.readFromConnection(c, authTimeout)
	if err != nil {
		v.logger.Error("Authentication challenge read failed", "error", err, "timeout_used", authTimeout)
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

	// Store the full banner and parse environment/version from auth response payload
	v.banner = authResponse.payload
	v.bannerReceived = true

	// Parse environment and version from banner
	env, version := parseBanner(authResponse.payload)
	v.environment = env
	v.version = version

	return nil
}

// GetBanner returns the stored Varnish CLI banner
func (v *Server) GetBanner() string {
	return v.banner
}

// GetEnvironment returns the parsed environment information
func (v *Server) GetEnvironment() string {
	return v.environment
}

// GetVersion returns the parsed Varnish version
func (v *Server) GetVersion() string {
	return v.version
}

// parseBanner extracts environment and version information from Varnish CLI banner
func parseBanner(banner string) (environment, version string) {
	// Extract environment line (e.g., "Linux,6.8.0-79-generic,x86_64,-jlinux,-smse4,-hcritbit")
	envRegex := regexp.MustCompile(`(?m)^([A-Za-z0-9_]+(?:,[^,\r\n]+)+)\s*$`)
	if envMatch := envRegex.FindStringSubmatch(banner); len(envMatch) > 1 {
		environment = envMatch[1]
	}

	// Extract version line (e.g., "varnish-plus-6.0.15r1 revision d0b65fce8c712013f9bd614bacca1e67a45799e8")
	versionRegex := regexp.MustCompile(`(varnish-[^\r\n]+)`)
	if versionMatch := versionRegex.FindStringSubmatch(banner); len(versionMatch) > 1 {
		version = versionMatch[1]
	}

	return
}

// Exec executes a given command and returns the output as a varnishresponse
func (v *Server) Exec(cmd string) (VarnishResponse, error) {

	respCh := make(chan VarnishResponse)
	v.reqCh <- varnishRequest{
		command:      cmd,
		responseChan: respCh,
	}
	select {
	case resp := <-respCh:
		if resp.statusCode != ClisOk {
			v.logger.Error("Varnishadm command failed", "command", cmd, "status", resp.statusCode, "response", resp.payload)
			return VarnishResponse{}, fmt.Errorf("command failed: %s", resp.payload)
		}
		v.logger.Debug("Varnishadm command succeeded", "command", cmd, "status", resp.statusCode, "payload", resp.payload, "response", resp.payload)
		return resp, nil
	case <-time.After(defaultCmdTimeout):
		v.logger.Error("Varnishadm command timed out", "command", cmd, "timeout", defaultCmdTimeout)
		return VarnishResponse{}, errors.New("command timed out")
	}
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
	deadline := time.Now().Add(readWriteTimeout)
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
		return out, errors.New("Nothing was written on the varnish connection")
	}

	// Read response with timeout
	out.payload, out.statusCode, err = v.readFromConnection(c, readWriteTimeout)
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
