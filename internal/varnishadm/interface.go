package varnishadm

import "context"

// VarnishadmInterface defines the interface for varnishadm implementations
type VarnishadmInterface interface {
	// Run starts the varnishadm server and blocks until context is cancelled
	Run(ctx context.Context) error
	// Connected returns a channel that is closed when varnishd has connected and authenticated
	Connected() <-chan struct{}
	// Exec executes a command and returns the response
	Exec(cmd string) (VarnishResponse, error)

	// Standard commands
	Ping() (VarnishResponse, error)
	Status() (VarnishResponse, error)
	Start() (VarnishResponse, error)
	Stop() (VarnishResponse, error)
	PanicShow() (VarnishResponse, error)
	PanicClear() (VarnishResponse, error)

	// VCL commands
	VCLLoad(name, path string) (VarnishResponse, error)
	VCLInline(name, vcl string) (VarnishResponse, error)
	VCLUse(name string) (VarnishResponse, error)
	VCLLabel(label, name string) (VarnishResponse, error)
	VCLDiscard(name string) (VarnishResponse, error)
	VCLList() (VarnishResponse, error)
	VCLListStructured() (*VCLListResult, error)

	// Parameter commands
	ParamShow(name string) (VarnishResponse, error)
	ParamSet(name, value string) (VarnishResponse, error)

	// Varnish Enterprise TLS commands
	TLSCertList() (VarnishResponse, error)
	TLSCertListStructured() (*TLSCertListResult, error)
	TLSCertLoad(name, certFile string, privateKeyFile string) (VarnishResponse, error)
	TLSCertDiscard(id string) (VarnishResponse, error)
	TLSCertCommit() (VarnishResponse, error)
	TLSCertRollback() (VarnishResponse, error)
	TLSCertReload() (VarnishResponse, error)
}
