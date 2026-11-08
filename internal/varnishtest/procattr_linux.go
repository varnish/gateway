//go:build linux

package varnishtest

import "syscall"

// sysProcAttr puts varnishd in its own process group so cleanup can reach the
// manager's child too, and asks the kernel to kill the manager if the test
// binary dies without running its cleanups (an interrupted or timed-out
// `go test`). The manager's own child exits when the manager goes away.
//
// Pdeathsig is Linux-only, which is why this sits behind a build tag.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
