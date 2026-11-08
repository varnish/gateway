//go:build !linux

package varnishtest

import "syscall"

// sysProcAttr puts varnishd in its own process group so cleanup can reach the
// manager's child too.
//
// There is no portable equivalent of Linux's Pdeathsig, so on other platforms
// a test binary that dies without running its cleanups leaves the manager
// running. Varnishd's -n work directory is under the test's TempDir, so the
// stray manager is visible and the next run picks a new directory; stop reads
// the process group out of the Cmd and kills it on the normal paths.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
