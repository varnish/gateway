package varnishadm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// MockVarnishadm implements VarnishadmInterface for testing
type MockVarnishadm struct {
	Port    int
	Secret  string
	logger  *slog.Logger
	running bool
	mu      sync.RWMutex

	// connected is closed immediately since mock is always "connected"
	connected chan struct{}

	// responses maps commands to predefined responses
	responses map[string]VarnishResponse

	// callHistory tracks commands that were executed
	callHistory []string

	// simulateDelay adds artificial delay to responses
	simulateDelay time.Duration

	// TLS certificate state tracking
	tlsCertsCommitted map[string]TLSCertEntry // committed certificates (current state)
	tlsCertsStaged    map[string]TLSCertEntry // staged certificates (transaction state)
	tlsTransaction    bool                    // whether a transaction is in progress
}

// NewMock creates a new mock varnishadm instance
func NewMock(port int, secret string, logger *slog.Logger) *MockVarnishadm {
	connected := make(chan struct{})
	close(connected) // Mock is always "connected"

	mock := &MockVarnishadm{
		Port:              port,
		Secret:            secret,
		logger:            logger,
		connected:         connected,
		responses:         make(map[string]VarnishResponse),
		tlsCertsCommitted: make(map[string]TLSCertEntry),
		tlsCertsStaged:    make(map[string]TLSCertEntry),
	}

	// Set up default responses for common commands
	mock.setDefaultResponses()

	return mock
}

// Connected returns a channel that is closed when varnishd has connected.
// For the mock, this is always immediately closed since the mock is always "connected".
func (m *MockVarnishadm) Connected() <-chan struct{} {
	return m.connected
}

// setDefaultResponses sets up common varnish command responses
func (m *MockVarnishadm) setDefaultResponses() {
	m.responses["ping"] = VarnishResponse{
		statusCode: ClisOk,
		payload:    "PONG",
	}

	m.responses["status"] = VarnishResponse{
		statusCode: ClisOk,
		payload:    "Child in state running",
	}

	m.responses["start"] = VarnishResponse{
		statusCode: ClisOk,
		payload:    "Child started",
	}

	m.responses["stop"] = VarnishResponse{
		statusCode: ClisOk,
		payload:    "Child stopped",
	}

	m.responses["banner"] = VarnishResponse{
		statusCode: ClisOk,
		payload:    "varnish-7.5.0 revision b14a3d38eb4d7887bce7fb98ffa6d4bd3b1b2e4e",
	}

	m.responses["vcl.list"] = VarnishResponse{
		statusCode: ClisOk,
		payload: `active      auto/warm          - vcl-api-orig (1 label)
available   auto/warm          - vcl-catz-orig (1 label)
available  label/warm          - label-api -> vcl-api-orig (1 return(vcl))
available  label/warm          - label-catz -> vcl-catz-orig (1 return(vcl))
available   auto/warm          - vcl-root-orig`,
	}

	m.responses["tls.cert.list"] = VarnishResponse{
		statusCode: ClisOk,
		payload: `Frontend State   Hostname         Certificate ID  Expiration date           OCSP stapling
main     active  example.com      cert-001        2024-12-31 23:59:59       enabled
api      active  api.example.com  cert-002        2024-11-30 12:00:00       disabled`,
	}

	m.responses["backend.list"] = VarnishResponse{
		statusCode: ClisOk,
		payload:    "Backend name                   Admin      Probe      Health     Last change\nboot.default                   probe      Healthy    5/5        Wed, 22 Aug 2024 10:30:00 GMT",
	}
}

// Run simulates running the varnishadm server
func (m *MockVarnishadm) Run(ctx context.Context) error {
	m.mu.Lock()
	m.running = true
	m.mu.Unlock()

	m.logger.Info("Starting mock varnishadm server", "port", m.Port)

	<-ctx.Done()

	m.mu.Lock()
	m.running = false
	m.mu.Unlock()

	m.logger.Info("Mock varnishadm server stopped")
	return nil
}

// Exec executes a command and returns a mock response
func (m *MockVarnishadm) Exec(cmd string) (VarnishResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Add to call history
	m.callHistory = append(m.callHistory, cmd)

	// Simulate delay if configured
	if m.simulateDelay > 0 {
		time.Sleep(m.simulateDelay)
	}

	// Check for explicitly set error responses first (for testing error handling)
	// This allows tests to override dynamic handlers by setting error responses
	if resp, exists := m.responses[cmd]; exists && resp.statusCode != ClisOk {
		m.logger.Debug("Mock command executed (error response)", "command", cmd, "status", resp.statusCode)
		return resp, nil
	}

	// Handle TLS certificate commands with state tracking BEFORE static responses
	// This ensures dynamic state is used instead of hardcoded responses
	if strings.HasPrefix(cmd, "tls.cert.load ") {
		return m.handleTLSCertLoad(cmd)
	}

	if strings.HasPrefix(cmd, "tls.cert.discard ") {
		return m.handleTLSCertDiscard(cmd)
	}

	if cmd == "tls.cert.commit" {
		return m.handleTLSCertCommit()
	}

	if cmd == "tls.cert.rollback" {
		return m.handleTLSCertRollback()
	}

	if cmd == "tls.cert.list" {
		return m.handleTLSCertList()
	}

	// Check if we have a specific response for this command (success responses)
	if resp, exists := m.responses[cmd]; exists {
		m.logger.Debug("Mock command executed", "command", cmd, "status", resp.statusCode)
		return resp, nil
	}

	// Handle pattern-based commands
	if strings.HasPrefix(cmd, "vcl.load") {
		return VarnishResponse{
			statusCode: ClisOk,
			payload:    "VCL compiled",
		}, nil
	}

	if strings.HasPrefix(cmd, "vcl.use") {
		return VarnishResponse{
			statusCode: ClisOk,
			payload:    "VCL '" + strings.TrimPrefix(cmd, "vcl.use ") + "' now active",
		}, nil
	}

	if strings.HasPrefix(cmd, "param.set") {
		return VarnishResponse{
			statusCode: ClisOk,
		}, nil
	}

	// Default response for unknown commands
	return VarnishResponse{
		statusCode: ClisUnknown,
		payload:    fmt.Sprintf("Unknown request: %s", cmd),
	}, nil
}

// SetResponse allows tests to configure custom responses for commands
func (m *MockVarnishadm) SetResponse(cmd string, response VarnishResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses[cmd] = response
}

// GetCallHistory returns the history of commands executed
func (m *MockVarnishadm) GetCallHistory() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	history := make([]string, len(m.callHistory))
	copy(history, m.callHistory)
	return history
}

// ClearCallHistory clears the command history
func (m *MockVarnishadm) ClearCallHistory() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callHistory = nil
}

// SetDelay configures artificial delay for responses
func (m *MockVarnishadm) SetDelay(delay time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.simulateDelay = delay
}

// IsRunning returns whether the mock server is running
func (m *MockVarnishadm) IsRunning() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.running
}

// Standard command wrappers

// Ping sends a ping command to the mock
func (m *MockVarnishadm) Ping() (VarnishResponse, error) {
	return m.Exec("ping")
}

// Status returns the status of the mock Varnish child process
func (m *MockVarnishadm) Status() (VarnishResponse, error) {
	return m.Exec("status")
}

// Start starts the mock Varnish child process
func (m *MockVarnishadm) Start() (VarnishResponse, error) {
	return m.Exec("start")
}

// Stop stops the mock Varnish child process
func (m *MockVarnishadm) Stop() (VarnishResponse, error) {
	return m.Exec("stop")
}

// PanicShow shows the mock panic message if available
func (m *MockVarnishadm) PanicShow() (VarnishResponse, error) {
	return m.Exec("panic.show")
}

// PanicClear clears the mock panic message
func (m *MockVarnishadm) PanicClear() (VarnishResponse, error) {
	return m.Exec("panic.clear")
}

// VCL command wrappers

// VCLLoad loads a VCL configuration from a file in the mock
func (m *MockVarnishadm) VCLLoad(name, path string) (VarnishResponse, error) {
	cmd := fmt.Sprintf("vcl.load %s %s", name, path)
	return m.Exec(cmd)
}

// VCLUse switches to using the specified VCL configuration in the mock
func (m *MockVarnishadm) VCLUse(name string) (VarnishResponse, error) {
	cmd := fmt.Sprintf("vcl.use %s", name)
	return m.Exec(cmd)
}

// VCLLabel assigns a label to a VCL
func (m *MockVarnishadm) VCLLabel(label, name string) (VarnishResponse, error) {
	cmd := fmt.Sprintf("vcl.label %s %s", label, name)
	return m.Exec(cmd)
}

// VCLDiscard discards a VCL configuration in the mock
func (m *MockVarnishadm) VCLDiscard(name string) (VarnishResponse, error) {
	cmd := fmt.Sprintf("vcl.discard %s", name)
	return m.Exec(cmd)
}

// VCLList lists all VCL configurations in the mock
func (m *MockVarnishadm) VCLList() (VarnishResponse, error) {
	return m.Exec("vcl.list")
}

// VCLListStructured lists all VCL configurations and returns parsed results in the mock
func (m *MockVarnishadm) VCLListStructured() (*VCLListResult, error) {
	resp, err := m.Exec("vcl.list")
	if err != nil {
		return nil, err
	}

	if resp.statusCode != ClisOk {
		return nil, fmt.Errorf("vcl.list command failed with status %d: %s", resp.statusCode, resp.payload)
	}

	return parseVCLList(resp.payload)
}

// Parameter command wrappers

// ParamShow shows the value of a parameter in the mock
func (m *MockVarnishadm) ParamShow(name string) (VarnishResponse, error) {
	if name == "" {
		return m.Exec("param.show")
	}
	cmd := fmt.Sprintf("param.show %s", name)
	return m.Exec(cmd)
}

// ParamSet sets the value of a parameter in the mock
func (m *MockVarnishadm) ParamSet(name, value string) (VarnishResponse, error) {
	cmd := fmt.Sprintf("param.set %s %s", name, value)
	resp, err := m.Exec(cmd)
	if err != nil {
		return resp, err
	}
	if resp.statusCode != ClisOk {
		return resp, fmt.Errorf("param.set %s failed with status %d: %s", name, resp.statusCode, resp.payload)
	}
	return resp, nil
}

// Varnish Enterprise TLS command wrappers

// TLSCertList lists all TLS certificates in the mock
func (m *MockVarnishadm) TLSCertList() (VarnishResponse, error) {
	return m.Exec("tls.cert.list")
}

// TLSCertListStructured lists all TLS certificates and returns parsed results in the mock
func (m *MockVarnishadm) TLSCertListStructured() (*TLSCertListResult, error) {
	resp, err := m.Exec("tls.cert.list")
	if err != nil {
		return nil, err
	}

	if resp.statusCode != ClisOk {
		return nil, fmt.Errorf("tls.cert.list command failed with status %d: %s", resp.statusCode, resp.payload)
	}

	return parseTLSCertList(resp.payload)
}

// TLSCertLoad loads a TLS certificate and key file in the mock: either combined or using a separate private key file
func (m *MockVarnishadm) TLSCertLoad(name, certFile string, privateKeyFile string) (VarnishResponse, error) {
	var cmd string
	if privateKeyFile == "" {
		cmd = fmt.Sprintf("tls.cert.load %s %s", name, certFile)
	} else {
		cmd = fmt.Sprintf("tls.cert.load %s %s -k %s", name, certFile, privateKeyFile)
	}
	return m.Exec(cmd)
}

// TLSCertDiscard discards a TLS certificate by ID in the mock
func (m *MockVarnishadm) TLSCertDiscard(id string) (VarnishResponse, error) {
	cmd := fmt.Sprintf("tls.cert.discard %s", id)
	return m.Exec(cmd)
}

// TLSCertCommit commits the loaded TLS certificates in the mock
func (m *MockVarnishadm) TLSCertCommit() (VarnishResponse, error) {
	return m.Exec("tls.cert.commit")
}

// TLSCertRollback rolls back the TLS certificate changes in the mock
func (m *MockVarnishadm) TLSCertRollback() (VarnishResponse, error) {
	return m.Exec("tls.cert.rollback")
}

// TLSCertReload reloads all TLS certificates in the mock
func (m *MockVarnishadm) TLSCertReload() (VarnishResponse, error) {
	return m.Exec("tls.cert.reload")
}

// TLS state management handlers (called internally by Exec)
// Note: These methods assume m.mu is already locked by Exec

// handleTLSCertLoad handles tls.cert.load command with state tracking
func (m *MockVarnishadm) handleTLSCertLoad(cmd string) (VarnishResponse, error) {
	// Parse: tls.cert.load <id> <path>
	parts := strings.Fields(cmd)
	if len(parts) < 3 {
		return VarnishResponse{
			statusCode: ClisUnknown,
			payload:    "Usage: tls.cert.load <id> <path>",
		}, nil
	}

	certID := parts[1]

	// Start transaction if not already started
	if !m.tlsTransaction {
		// Copy committed state to staged
		m.tlsCertsStaged = make(map[string]TLSCertEntry)
		for k, v := range m.tlsCertsCommitted {
			m.tlsCertsStaged[k] = v
		}
		m.tlsTransaction = true
	}

	// Check if cert already exists in staged state
	if _, exists := m.tlsCertsStaged[certID]; exists {
		return VarnishResponse{
			statusCode: 300, // Status for "already exists"
			payload:    fmt.Sprintf("Id: \"%s\" already exists", certID),
		}, nil
	}

	// Add to staged state
	m.tlsCertsStaged[certID] = TLSCertEntry{
		CertificateID: certID,
		State:         "active",
		Frontend:      "main",
		Hostname:      certID,
		Expiration:    time.Now().Add(90 * 24 * time.Hour),
		OCSPStapling:  false,
	}

	return VarnishResponse{
		statusCode: ClisOk,
		payload:    fmt.Sprintf("Certificate %s loaded", certID),
	}, nil
}

// handleTLSCertDiscard handles tls.cert.discard command with state tracking
func (m *MockVarnishadm) handleTLSCertDiscard(cmd string) (VarnishResponse, error) {
	// Parse: tls.cert.discard <id>
	parts := strings.Fields(cmd)
	if len(parts) < 2 {
		return VarnishResponse{
			statusCode: ClisUnknown,
			payload:    "Usage: tls.cert.discard <id>",
		}, nil
	}

	certID := parts[1]

	// Start transaction if not already started
	if !m.tlsTransaction {
		// Copy committed state to staged
		m.tlsCertsStaged = make(map[string]TLSCertEntry)
		for k, v := range m.tlsCertsCommitted {
			m.tlsCertsStaged[k] = v
		}
		m.tlsTransaction = true
	}

	// Check if cert exists in staged state
	if _, exists := m.tlsCertsStaged[certID]; !exists {
		return VarnishResponse{
			statusCode: 300,
			payload:    fmt.Sprintf("Certificate \"%s\" not found", certID),
		}, nil
	}

	// Remove from staged state
	delete(m.tlsCertsStaged, certID)

	return VarnishResponse{
		statusCode: ClisOk,
		payload:    fmt.Sprintf("Certificate %s discarded", certID),
	}, nil
}

// handleTLSCertCommit handles tls.cert.commit command with state tracking
func (m *MockVarnishadm) handleTLSCertCommit() (VarnishResponse, error) {
	if !m.tlsTransaction {
		// No transaction in progress, nothing to commit
		return VarnishResponse{
			statusCode: ClisOk,
			payload:    "No changes to commit",
		}, nil
	}

	// Commit staged state
	m.tlsCertsCommitted = make(map[string]TLSCertEntry)
	for k, v := range m.tlsCertsStaged {
		m.tlsCertsCommitted[k] = v
	}

	// Clear transaction
	m.tlsCertsStaged = make(map[string]TLSCertEntry)
	m.tlsTransaction = false

	return VarnishResponse{
		statusCode: ClisOk,
		payload:    "TLS certificates committed",
	}, nil
}

// handleTLSCertRollback handles tls.cert.rollback command with state tracking
func (m *MockVarnishadm) handleTLSCertRollback() (VarnishResponse, error) {
	if !m.tlsTransaction {
		// No transaction in progress, nothing to rollback
		return VarnishResponse{
			statusCode: ClisOk,
			payload:    "No changes to rollback",
		}, nil
	}

	// Discard staged changes
	m.tlsCertsStaged = make(map[string]TLSCertEntry)
	m.tlsTransaction = false

	return VarnishResponse{
		statusCode: ClisOk,
		payload:    "TLS certificate changes rolled back",
	}, nil
}

// handleTLSCertList handles tls.cert.list command and returns current committed state
func (m *MockVarnishadm) handleTLSCertList() (VarnishResponse, error) {
	// Build payload from committed certificates
	var lines []string
	lines = append(lines, "Frontend State   Hostname         Certificate ID  Expiration date           OCSP stapling")

	for _, cert := range m.tlsCertsCommitted {
		// Format: default  active  example.com      cert0           Feb 29 13:38:00 2042 GMT  true
		ocspStatus := "false"
		if cert.OCSPStapling {
			ocspStatus = "true"
		}

		// Use default frontend if not specified
		frontend := cert.Frontend
		if frontend == "" {
			frontend = "default"
		}

		line := fmt.Sprintf("%-8s %-7s %-16s %-15s %-25s %s",
			frontend,
			cert.State,
			cert.Hostname,
			cert.CertificateID,
			cert.Expiration.Format("Jan 02 15:04:05 2006 MST"),
			ocspStatus)
		lines = append(lines, line)
	}

	return VarnishResponse{
		statusCode: ClisOk,
		payload:    strings.Join(lines, "\n"),
	}, nil
}

// SetTLSState sets the initial TLS certificate state for testing
func (m *MockVarnishadm) SetTLSState(certs []TLSCertEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.tlsCertsCommitted = make(map[string]TLSCertEntry)
	for _, cert := range certs {
		m.tlsCertsCommitted[cert.CertificateID] = cert
	}
	m.tlsCertsStaged = make(map[string]TLSCertEntry)
	m.tlsTransaction = false
}

// GetTLSState returns the current committed TLS certificate state
func (m *MockVarnishadm) GetTLSState() []TLSCertEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	certs := make([]TLSCertEntry, 0, len(m.tlsCertsCommitted))
	for _, cert := range m.tlsCertsCommitted {
		certs = append(certs, cert)
	}
	return certs
}

// BackendList executes backend.list command with optional flags
func (m *MockVarnishadm) BackendList(detailed, json bool) (VarnishResponse, error) {
	cmd := "backend.list"
	if json {
		cmd += " -j"
	} else if detailed {
		cmd += " -p"
	}
	return m.Exec(cmd)
}
