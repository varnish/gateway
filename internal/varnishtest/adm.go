package varnishtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/varnish/gateway/internal/varnishadm"
)

// CLI implements varnishadm.VarnishadmInterface on top of the varnishadm
// command line tool, so code written against the chaperone's reverse-mode
// server can drive this test instance. VCLInline writes the VCL to a file and
// uses vcl.load (the CLI tool cannot send heredocs). Commands the tests have
// no use for (TLS, Stop, panic handling) return an error.
//
// Only tests use this package; nothing in production code may import it.
type CLI struct {
	v      *Varnishd
	seq    atomic.Uint64
	closed chan struct{}
}

var _ varnishadm.VarnishadmInterface = (*CLI)(nil)

// CLI returns a varnishadm.VarnishadmInterface for this instance.
func (v *Varnishd) CLI() *CLI {
	c := &CLI{v: v, closed: make(chan struct{})}
	close(c.closed)
	return c
}

var errCodeRe = regexp.MustCompile(`Command failed with error code (\d+)`)

// exec runs varnishadm and maps its exit to a CLI status: exit 0 is 200, a
// failed command reports "Command failed with error code N" on stderr. Any
// other failure (cannot run varnishadm, cannot reach varnishd) is an error,
// like a transport error from the real server.
func (c *CLI) exec(args ...string) (varnishadm.VarnishResponse, error) {
	cmd := exec.Command("varnishadm", append([]string{"-n", c.v.workDir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return varnishadm.NewVarnishResponse(varnishadm.ClisOk, stdout.String()), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if m := errCodeRe.FindStringSubmatch(stderr.String() + stdout.String()); m != nil {
			code, _ := strconv.Atoi(m[1])
			payload := strings.TrimSpace(errCodeRe.ReplaceAllString(stdout.String(), ""))
			return varnishadm.NewVarnishResponse(code, payload), nil
		}
	}
	return varnishadm.VarnishResponse{}, fmt.Errorf("varnishadm(%s): %w: %s%s",
		strings.Join(args, " "), err, stdout.String(), stderr.String())
}

func errUnsupported(name string) error {
	return fmt.Errorf("varnishtest.CLI: %s is not supported", name)
}

// Run blocks until ctx is cancelled; there is no server to run.
func (c *CLI) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// Connected returns a closed channel: varnishd is already up.
func (c *CLI) Connected() <-chan struct{} { return c.closed }

// Exec runs a single-line command, split on whitespace.
func (c *CLI) Exec(cmd string) (varnishadm.VarnishResponse, error) {
	if strings.ContainsAny(cmd, "\n") {
		return varnishadm.VarnishResponse{}, errUnsupported("multi-line Exec")
	}
	return c.exec(strings.Fields(cmd)...)
}

func (c *CLI) Ping() (varnishadm.VarnishResponse, error)   { return c.exec("ping") }
func (c *CLI) Status() (varnishadm.VarnishResponse, error) { return c.exec("status") }
func (c *CLI) Start() (varnishadm.VarnishResponse, error)  { return c.exec("start") }

func (c *CLI) Stop() (varnishadm.VarnishResponse, error) {
	return varnishadm.VarnishResponse{}, errUnsupported("Stop")
}

func (c *CLI) PanicShow() (varnishadm.VarnishResponse, error) {
	return varnishadm.VarnishResponse{}, errUnsupported("PanicShow")
}

func (c *CLI) PanicClear() (varnishadm.VarnishResponse, error) {
	return varnishadm.VarnishResponse{}, errUnsupported("PanicClear")
}

func (c *CLI) VCLLoad(name, path string) (varnishadm.VarnishResponse, error) {
	return c.exec("vcl.load", name, path)
}

// VCLInline writes vcl to a file under the instance's VCL directory and loads
// it with vcl.load.
func (c *CLI) VCLInline(name, vcl string) (varnishadm.VarnishResponse, error) {
	path := filepath.Join(c.v.vclDir, fmt.Sprintf("inline-%d-%s.vcl", c.seq.Add(1), name))
	if err := os.WriteFile(path, []byte(vcl), 0o644); err != nil {
		return varnishadm.VarnishResponse{}, fmt.Errorf("os.WriteFile(%s): %w", path, err)
	}
	return c.exec("vcl.load", name, path)
}

func (c *CLI) VCLUse(name string) (varnishadm.VarnishResponse, error) {
	return c.exec("vcl.use", name)
}

func (c *CLI) VCLLabel(label, name string) (varnishadm.VarnishResponse, error) {
	return c.exec("vcl.label", label, name)
}

func (c *CLI) VCLDiscard(name string) (varnishadm.VarnishResponse, error) {
	return c.exec("vcl.discard", name)
}

func (c *CLI) VCLList() (varnishadm.VarnishResponse, error) { return c.exec("vcl.list") }

func (c *CLI) VCLListJSON() ([]varnishadm.VCLEntry, error) {
	resp, err := c.exec("vcl.list", "-j")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() != varnishadm.ClisOk {
		return nil, fmt.Errorf("vcl.list -j failed with status %d: %s", resp.StatusCode(), resp.Payload())
	}
	return varnishadm.ParseVCLListJSON(resp.Payload())
}

func (c *CLI) ParamShow(name string) (varnishadm.VarnishResponse, error) {
	if name == "" {
		return c.exec("param.show")
	}
	return c.exec("param.show", name)
}

// ParamSet returns an error on a non-200 reply, like varnishadm.Server.
func (c *CLI) ParamSet(name, value string) (varnishadm.VarnishResponse, error) {
	resp, err := c.exec("param.set", name, value)
	if err != nil {
		return resp, err
	}
	if resp.StatusCode() != varnishadm.ClisOk {
		return resp, fmt.Errorf("param.set %s failed with status %d: %s", name, resp.StatusCode(), resp.Payload())
	}
	return resp, nil
}

func (c *CLI) TLSCertList() (varnishadm.VarnishResponse, error) {
	return varnishadm.VarnishResponse{}, errUnsupported("TLSCertList")
}

func (c *CLI) TLSCertListStructured() (*varnishadm.TLSCertListResult, error) {
	return nil, errUnsupported("TLSCertListStructured")
}

func (c *CLI) TLSCertLoad(string, string, string) (varnishadm.VarnishResponse, error) {
	return varnishadm.VarnishResponse{}, errUnsupported("TLSCertLoad")
}

func (c *CLI) TLSCertDiscard(string) (varnishadm.VarnishResponse, error) {
	return varnishadm.VarnishResponse{}, errUnsupported("TLSCertDiscard")
}

func (c *CLI) TLSCertCommit() (varnishadm.VarnishResponse, error) {
	return varnishadm.VarnishResponse{}, errUnsupported("TLSCertCommit")
}

func (c *CLI) TLSCertRollback() (varnishadm.VarnishResponse, error) {
	return varnishadm.VarnishResponse{}, errUnsupported("TLSCertRollback")
}

func (c *CLI) TLSCertReload() (varnishadm.VarnishResponse, error) {
	return varnishadm.VarnishResponse{}, errUnsupported("TLSCertReload")
}
