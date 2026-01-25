package varnishadm

import (
	"fmt"
	"strconv"
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
