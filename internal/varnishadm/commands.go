package varnishadm

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Standard Varnish commands

// Ping sends a ping command to varnishadm
func (v *Server) Ping() (VarnishResponse, error) {
	return v.Exec("ping")
}

// Status returns the status of the Varnish child process
func (v *Server) Status() (VarnishResponse, error) {
	return v.Exec("status")
}

// Start starts the Varnish child process
func (v *Server) Start() (VarnishResponse, error) {
	return v.Exec("start")
}

// Stop stops the Varnish child process
func (v *Server) Stop() (VarnishResponse, error) {
	return v.Exec("stop")
}

// PanicShow shows the panic message if available
func (v *Server) PanicShow() (VarnishResponse, error) {
	return v.Exec("panic.show")
}

// PanicClear clears the panic message
func (v *Server) PanicClear() (VarnishResponse, error) {
	return v.Exec("panic.clear")
}

// VCL commands

// VCLLoad loads a VCL configuration from a file
func (v *Server) VCLLoad(name, path string) (VarnishResponse, error) {
	cmd := fmt.Sprintf("vcl.load %s %s", name, path)
	return v.Exec(cmd)
}

// VCLInline loads VCL configuration inline (without a file).
//
// The VCL is delivered via a Varnish CLI heredoc. Using a fixed delimiter
// (e.g. "EOF") is unsafe: user VCL is merged verbatim, so a line in the VCL
// that is exactly the delimiter would close the heredoc early and let the
// following lines execute as CLI commands (stop, param.set cc_command, ...).
// We generate an unguessable per-call delimiter from crypto/rand so no
// attacker-supplied VCL can predict and inject it, and additionally reject
// (rather than silently mis-parse) the astronomically unlikely case where the
// VCL still contains a line equal to the chosen delimiter.
func (v *Server) VCLInline(name, vcl string) (VarnishResponse, error) {
	delimiter, err := heredocDelimiter()
	if err != nil {
		return VarnishResponse{}, fmt.Errorf("heredocDelimiter(): %w", err)
	}
	// Belt-and-suspenders: guarantee the delimiter cannot appear as a standalone
	// line in the payload. With 128 bits of randomness this should never fire.
	if containsLine(vcl, delimiter) {
		return VarnishResponse{}, fmt.Errorf("VCLInline: generated heredoc delimiter %q collides with a line in the VCL payload", delimiter)
	}
	cmd := fmt.Sprintf("vcl.inline %s << %s\n%s\n%s", name, delimiter, vcl, delimiter)
	return v.Exec(cmd)
}

// heredocDelimiter returns an unguessable heredoc delimiter word of the form
// "EOF_<32 hex chars>" backed by 128 bits of cryptographic randomness. The
// Varnish CLI accepts any single word as the heredoc terminator.
func heredocDelimiter() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rand.Read: %w", err)
	}
	return "EOF_" + hex.EncodeToString(b[:]), nil
}

// containsLine reports whether s contains a line exactly equal to line.
// Both "\n" and "\r\n" line endings are considered so a trailing CR cannot
// smuggle a delimiter past the check.
func containsLine(s, line string) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimRight(l, "\r") == line {
			return true
		}
	}
	return false
}

// VCLUse switches to using the specified VCL configuration
func (v *Server) VCLUse(name string) (VarnishResponse, error) {
	cmd := fmt.Sprintf("vcl.use %s", name)
	return v.Exec(cmd)
}

// VCLLabel assigns a label to a VCL
func (v *Server) VCLLabel(label, name string) (VarnishResponse, error) {
	cmd := fmt.Sprintf("vcl.label %s %s", label, name)
	return v.Exec(cmd)
}

// VCLDiscard discards a VCL configuration
func (v *Server) VCLDiscard(name string) (VarnishResponse, error) {
	cmd := fmt.Sprintf("vcl.discard %s", name)
	return v.Exec(cmd)
}

// VCLList lists all VCL configurations
func (v *Server) VCLList() (VarnishResponse, error) {
	return v.Exec("vcl.list")
}

// VCLListStructured lists all VCL configurations and returns parsed results
func (v *Server) VCLListStructured() (*VCLListResult, error) {
	resp, err := v.Exec("vcl.list")
	if err != nil {
		return nil, err
	}

	if resp.statusCode != ClisOk {
		return nil, fmt.Errorf("vcl.list command failed with status %d: %s", resp.statusCode, resp.payload)
	}

	return parseVCLList(resp.payload)
}

// Parameter commands

// ParamShow shows the value of a parameter
func (v *Server) ParamShow(name string) (VarnishResponse, error) {
	if name == "" {
		return v.Exec("param.show")
	}
	cmd := fmt.Sprintf("param.show %s", name)
	return v.Exec(cmd)
}

// ParamSet sets the value of a parameter
func (v *Server) ParamSet(name, value string) (VarnishResponse, error) {
	cmd := fmt.Sprintf("param.set %s %s", name, value)
	resp, err := v.Exec(cmd)
	if err != nil {
		return resp, err
	}
	if resp.statusCode != ClisOk {
		return resp, fmt.Errorf("param.set %s failed with status %d: %s", name, resp.statusCode, resp.payload)
	}
	return resp, nil
}

// ParamValue defines acceptable parameter value types
type ParamValue interface {
	int | bool | float64 | string | time.Duration | Size
}

// ParamSetter is a minimal interface for types that can set parameters
type ParamSetter interface {
	ParamSet(name, value string) (VarnishResponse, error)
}

// ParamSetTyped sets a parameter with type-safe value conversion.
// Note: This is a package function (not a method) because Go doesn't allow type parameters on methods.
func ParamSetTyped[T ParamValue](v ParamSetter, name string, value T) (VarnishResponse, error) {
	var strValue string

	switch val := any(value).(type) {
	case int:
		strValue = strconv.Itoa(val)
	case bool:
		if val {
			strValue = "on"
		} else {
			strValue = "off"
		}
	case float64:
		strValue = strconv.FormatFloat(val, 'f', -1, 64)
	case string:
		strValue = val
	case time.Duration:
		strValue = fmt.Sprintf("%.0fs", val.Seconds())
	case Size:
		strValue = val.String()
	}

	return v.ParamSet(name, strValue)
}

// Varnish Enterprise TLS commands

// TLSCertList lists all TLS certificates
func (v *Server) TLSCertList() (VarnishResponse, error) {
	return v.Exec("tls.cert.list")
}

// TLSCertListStructured lists all TLS certificates and returns parsed results
func (v *Server) TLSCertListStructured() (*TLSCertListResult, error) {
	resp, err := v.Exec("tls.cert.list")
	if err != nil {
		return nil, err
	}

	if resp.statusCode != ClisOk {
		return nil, fmt.Errorf("tls.cert.list command failed with status %d: %s", resp.statusCode, resp.payload)
	}

	return parseTLSCertList(resp.payload)
}

// TLSCertLoad loads a TLS certificate and key file in the mock: either combined or using a separate private key file
func (v *Server) TLSCertLoad(name, certFile string, privateKeyFile string) (VarnishResponse, error) {
	var cmd string
	if privateKeyFile == "" {
		cmd = fmt.Sprintf("tls.cert.load %s %s", name, certFile)
	} else {
		cmd = fmt.Sprintf("tls.cert.load %s %s -k %s", name, certFile, privateKeyFile)
	}
	return v.Exec(cmd)
}

// TLSCertDiscard discards a TLS certificate by ID
func (v *Server) TLSCertDiscard(id string) (VarnishResponse, error) {
	cmd := fmt.Sprintf("tls.cert.discard %s", id)
	return v.Exec(cmd)
}

// TLSCertCommit commits the loaded TLS certificates
func (v *Server) TLSCertCommit() (VarnishResponse, error) {
	return v.Exec("tls.cert.commit")
}

// TLSCertRollback rolls back the TLS certificate changes
func (v *Server) TLSCertRollback() (VarnishResponse, error) {
	return v.Exec("tls.cert.rollback")
}

// TLSCertReload reloads all TLS certificates
func (v *Server) TLSCertReload() (VarnishResponse, error) {
	return v.Exec("tls.cert.reload")
}

// Backend commands

// BackendList lists all backends
// If detailed is true, adds -p flag for detailed output
// If json is true, adds -j flag for JSON output
func (v *Server) BackendList(detailed, json bool) (VarnishResponse, error) {
	cmd := "backend.list"
	if json {
		cmd += " -j"
	} else if detailed {
		cmd += " -p"
	}
	return v.Exec(cmd)
}
