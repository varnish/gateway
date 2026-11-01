package varnishadm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
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

	// VCL state: loaded VCLs and labels in load order, plus the active one.
	// vcl.inline/vcl.load/vcl.label/vcl.use/vcl.discard update it and
	// `vcl.list -j` renders it, unless SetVCLListJSON scripted a fixed list.
	vcls            []*mockVCL
	activeVCL       string
	vclListOverride []VCLEntry
	strictVCL       bool
	held            map[string]bool   // see HoldVCL
	compileErrors   map[string]string // VCL body substring -> CLI error payload

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
		compileErrors:     make(map[string]string),
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

	m.responses["tls.cert.reload"] = VarnishResponse{
		statusCode: ClisOk,
		payload:    "TLS certificates reloaded",
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

	if resp, handled := m.handleVCLCommand(cmd); handled {
		return resp, nil
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

// VCLInline loads VCL configuration inline (without a file) in the mock
func (m *MockVarnishadm) VCLInline(name, vcl string) (VarnishResponse, error) {
	cmd := fmt.Sprintf("vcl.inline %s << EOF\n%s\nEOF", name, vcl)
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

// VCLListJSON runs `vcl.list -j` against the mock and parses the reply.
func (m *MockVarnishadm) VCLListJSON() ([]VCLEntry, error) {
	resp, err := m.Exec("vcl.list -j")
	if err != nil {
		return nil, err
	}
	if resp.statusCode != ClisOk {
		return nil, fmt.Errorf("vcl.list -j failed with status %d: %s", resp.statusCode, resp.payload)
	}
	return ParseVCLListJSON(resp.payload)
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

// mockVCL is one loaded VCL or label in the mock's VCL state.
type mockVCL struct {
	name        string
	body        string // VCL source (vcl.inline only)
	labelTarget string // non-empty for labels
	discarded   bool   // discarded while held busy (see HoldVCL)
}

// HoldVCL simulates in-flight requests on a VCL: a vcl.discard of it is
// accepted but the VCL stays listed with status "discarded" (as varnishd
// does while idle keep-alive sessions still reference it) until ReleaseVCL.
func (m *MockVarnishadm) HoldVCL(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held == nil {
		m.held = make(map[string]bool)
	}
	m.held[name] = true
}

// ReleaseVCL ends HoldVCL; a VCL discarded meanwhile goes away.
func (m *MockVarnishadm) ReleaseVCL(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.held, name)
	if v := m.findVCL(name); v != nil && v.discarded {
		m.removeVCL(name)
	}
}

var reReturnVCL = regexp.MustCompile(`return\s*\(\s*vcl\s*\(\s*([^)\s]+)\s*\)\s*\)`)

// SetVCLListJSON scripts the entries `vcl.list -j` returns, overriding the
// tracked VCL state. Pass nil to go back to the tracked state.
func (m *MockVarnishadm) SetVCLListJSON(entries []VCLEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vclListOverride = entries
}

// SetStrictVCL makes the VCL commands refuse what varnishd refuses: labelling
// or using an unknown VCL, loading a duplicate name or a VCL that returns to a
// missing label, discarding the active VCL, a labelled VCL, or a label another
// VCL returns to. Without it those commands succeed and only update state.
func (m *MockVarnishadm) SetStrictVCL(strict bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.strictVCL = strict
}

// SetVCLCompileError makes vcl.inline fail with a VCC-style error whenever the
// VCL body contains substr. An empty payload removes the rule.
func (m *MockVarnishadm) SetVCLCompileError(substr, payload string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if payload == "" {
		delete(m.compileErrors, substr)
		return
	}
	m.compileErrors[substr] = payload
}

// SetVCLState replaces the tracked VCL state: vcls maps VCL names to bodies,
// labels maps label names to targets, active names the active VCL.
func (m *MockVarnishadm) SetVCLState(vcls map[string]string, labels map[string]string, active string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vcls = nil
	names := make([]string, 0, len(vcls))
	for n := range vcls {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		m.vcls = append(m.vcls, &mockVCL{name: n, body: vcls[n]})
	}
	names = names[:0]
	for n := range labels {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		m.vcls = append(m.vcls, &mockVCL{name: n, labelTarget: labels[n]})
	}
	m.activeVCL = active
}

// VCLState returns the tracked labels (label -> target), the loaded non-label
// VCL names in load order, and the active VCL.
func (m *MockVarnishadm) VCLState() (labels map[string]string, vcls []string, active string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	labels = make(map[string]string)
	for _, v := range m.vcls {
		if v.labelTarget != "" {
			labels[v.name] = v.labelTarget
		} else {
			vcls = append(vcls, v.name)
		}
	}
	return labels, vcls, m.activeVCL
}

func (m *MockVarnishadm) findVCL(name string) *mockVCL {
	for _, v := range m.vcls {
		if v.name == name {
			return v
		}
	}
	return nil
}

func (m *MockVarnishadm) removeVCL(name string) {
	for i, v := range m.vcls {
		if v.name == name {
			m.vcls = append(m.vcls[:i], m.vcls[i+1:]...)
			return
		}
	}
}

// labelRefs counts the loaded VCLs that return(vcl) to label.
func (m *MockVarnishadm) labelRefs(label string) int {
	n := 0
	for _, v := range m.vcls {
		if v.labelTarget != "" || v.discarded {
			continue
		}
		for _, match := range reReturnVCL.FindAllStringSubmatch(v.body, -1) {
			if match[1] == label {
				n++
				break
			}
		}
	}
	return n
}

func (m *MockVarnishadm) labelsOf(name string) []string {
	var out []string
	for _, v := range m.vcls {
		if v.labelTarget == name {
			out = append(out, v.name)
		}
	}
	return out
}

func vclResp(status int, format string, args ...any) VarnishResponse {
	return VarnishResponse{statusCode: status, payload: fmt.Sprintf(format, args...)}
}

// handleVCLCommand applies the vcl.* commands to the tracked state. It must
// be called with m.mu held. handled is false for commands it does not own.
func (m *MockVarnishadm) handleVCLCommand(cmd string) (resp VarnishResponse, handled bool) {
	firstLine, body, _ := strings.Cut(cmd, "\n")
	fields := strings.Fields(firstLine)
	if len(fields) == 0 {
		return VarnishResponse{}, false
	}
	switch fields[0] {
	case "vcl.list":
		if len(fields) == 2 && fields[1] == "-j" {
			return m.renderVCLListJSON(), true
		}
		return VarnishResponse{}, false
	case "vcl.inline", "vcl.load":
		if len(fields) < 2 {
			return vclResp(ClisToofew, "Too few parameters"), true
		}
		name := fields[1]
		if fields[0] == "vcl.inline" {
			for substr, payload := range m.compileErrors {
				if strings.Contains(body, substr) {
					return vclResp(ClisParam, "%s", payload), true
				}
			}
		} else {
			body = ""
		}
		if m.findVCL(name) != nil {
			return vclResp(ClisParam, "Already a VCL named %s", name), true
		}
		if m.strictVCL {
			for _, match := range reReturnVCL.FindAllStringSubmatch(body, -1) {
				if l := m.findVCL(match[1]); l == nil || l.labelTarget == "" {
					return vclResp(ClisParam, "Message from VCC-compiler:\nSymbol not found: '%s'\nVCL compilation failed", match[1]), true
				}
			}
		}
		m.vcls = append(m.vcls, &mockVCL{name: name, body: body})
		return vclResp(ClisOk, "VCL compiled."), true
	case "vcl.label":
		if len(fields) < 3 {
			return vclResp(ClisToofew, "Too few parameters"), true
		}
		label, target := fields[1], fields[2]
		if m.strictVCL {
			t := m.findVCL(target)
			if t == nil || t.discarded {
				return vclResp(ClisParam, "No VCL name matches %s", target), true
			}
			if t.labelTarget != "" {
				return vclResp(ClisParam, "VCL labels cannot point to labels"), true
			}
		}
		if l := m.findVCL(label); l != nil {
			if l.labelTarget == "" {
				return vclResp(ClisParam, "%s is not a label", label), true
			}
			l.labelTarget = target
		} else {
			m.vcls = append(m.vcls, &mockVCL{name: label, labelTarget: target})
		}
		return vclResp(ClisOk, ""), true
	case "vcl.use":
		if len(fields) < 2 {
			return vclResp(ClisToofew, "Too few parameters"), true
		}
		if v := m.findVCL(fields[1]); m.strictVCL && (v == nil || v.labelTarget != "" || v.discarded) {
			return vclResp(ClisParam, "No VCL name matches %s", fields[1]), true
		}
		m.activeVCL = fields[1]
		return vclResp(ClisOk, "VCL '%s' now active", fields[1]), true
	case "vcl.discard":
		if len(fields) < 2 {
			return vclResp(ClisToofew, "Too few parameters"), true
		}
		name := fields[1]
		if m.strictVCL {
			v := m.findVCL(name)
			if v == nil || v.discarded {
				return vclResp(ClisParam, "No VCL name matches %s", name), true
			}
			if name == m.activeVCL {
				return vclResp(ClisCant, "Cannot discard active VCL program"), true
			}
			if v.labelTarget != "" && m.labelRefs(name) > 0 {
				return vclResp(ClisCant, "Cannot discard VCL label %s, other VCLs depend on it", name), true
			}
			if labels := m.labelsOf(name); v.labelTarget == "" && len(labels) > 0 {
				return vclResp(ClisCant, "Cannot discard labeled VCL program %s:\n\t%s", name, strings.Join(labels, "\n\t")), true
			}
		}
		if v := m.findVCL(name); v != nil && m.held[name] {
			v.discarded = true
		} else {
			m.removeVCL(name)
		}
		return vclResp(ClisOk, ""), true
	}
	return VarnishResponse{}, false
}

// renderVCLListJSON renders the tracked state (or the scripted override) in
// the `vcl.list -j` envelope. It must be called with m.mu held.
func (m *MockVarnishadm) renderVCLListJSON() VarnishResponse {
	entries := m.vclListOverride
	if entries == nil {
		for _, v := range m.vcls {
			e := VCLEntry{Name: v.name, Status: "available", State: "auto", Temperature: "warm"}
			if v.name == m.activeVCL {
				e.Status = "active"
			}
			if v.discarded {
				e.Status, e.State, e.Temperature, e.Busy = "discarded", "cold", "busy", 1
			}
			if v.labelTarget != "" {
				e.State = "label"
				e.LabelTarget = v.labelTarget
				e.LabelRefs = m.labelRefs(v.name)
			} else {
				e.Labels = len(m.labelsOf(v.name))
			}
			entries = append(entries, e)
		}
	}
	parts := []any{2, []string{"vcl.list", "-j"}, 1700000000.0}
	for _, e := range entries {
		obj := map[string]any{
			"status":      e.Status,
			"state":       e.State,
			"temperature": e.Temperature,
			"busy":        e.Busy,
			"name":        e.Name,
		}
		if e.Labels > 0 {
			obj["labels"] = e.Labels
		}
		if e.State == "label" {
			label := map[string]any{"name": e.LabelTarget}
			if e.LabelRefs > 0 {
				label["refs"] = e.LabelRefs
			}
			obj["label"] = label
		}
		parts = append(parts, obj)
	}
	b, err := json.Marshal(parts)
	if err != nil {
		return vclResp(ClisCant, "json.Marshal: %v", err)
	}
	return vclResp(ClisOk, "%s", b)
}
