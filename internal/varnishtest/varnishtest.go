// Package varnishtest runs a real varnishd manager process for Go tests.
//
// Start skips the calling test when varnishd or varnishadm are not on PATH,
// when varnishd is older than 7.0, or when the ghost VMOD has not been built
// (ghost/target/release/libvmod_ghost.so). CI's Go job has neither, so tests
// built on this package skip there and run on a developer machine.
//
// The manager is started with no VCL (an empty -f argument) and no child, so
// VCLs can be loaded, labelled and switched through the CLI before Start is
// called. This is the only way to compile a VCL that references labels:
// varnishd -C cannot resolve them.
package varnishtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Varnishd is a running varnishd manager process owned by a test.
type Varnishd struct {
	t       *testing.T
	workDir string // -n directory
	vclDir  string // VCL files written by LoadVCL
	logPath string
	cmd     *exec.Cmd
	done    chan struct{} // closed when cmd.Wait returns
}

const (
	minMajorVersion = 7
	pingTimeout     = 5 * time.Second
	stopTimeout     = 5 * time.Second
)

// Start launches a varnishd manager for the test and registers a cleanup that
// stops it (and its child) and waits for it to exit. It calls t.Skip when the
// environment cannot run varnishd with the ghost VMOD.
func Start(t *testing.T) *Varnishd {
	t.Helper()

	varnishdBin, err := exec.LookPath("varnishd")
	if err != nil {
		t.Skip("varnishd not on PATH; skipping test that needs a real varnishd")
	}
	if _, err := exec.LookPath("varnishadm"); err != nil {
		t.Skip("varnishadm not on PATH; skipping test that needs a real varnishd")
	}
	major, err := varnishdMajorVersion(varnishdBin)
	if err != nil {
		t.Skipf("cannot determine varnishd version: %v", err)
	}
	if major < minMajorVersion {
		t.Skipf("varnishd major version %d < %d; skipping", major, minMajorVersion)
	}
	vmodDir, err := GhostVMODDir()
	if err != nil {
		t.Skipf("ghost VMOD not available: %v", err)
	}

	workDir := t.TempDir()
	scratch := t.TempDir()
	v := &Varnishd{
		t:       t,
		workDir: filepath.Join(workDir, "n"),
		vclDir:  filepath.Join(scratch, "vcl"),
		logPath: filepath.Join(scratch, "varnishd.log"),
		done:    make(chan struct{}),
	}
	if err := os.MkdirAll(v.vclDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll(%s): %v", v.vclDir, err)
	}
	secret := filepath.Join(scratch, "secret")
	if err := os.WriteFile(secret, []byte("varnishtest-secret\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%s): %v", secret, err)
	}
	logFile, err := os.Create(v.logPath)
	if err != nil {
		t.Fatalf("os.Create(%s): %v", v.logPath, err)
	}

	v.cmd = exec.Command(varnishdBin,
		"-n", v.workDir,
		"-a", "127.0.0.1:0",
		"-T", "127.0.0.1:0",
		"-S", secret,
		"-s", "malloc,8m",
		"-f", "",
		"-F",
		// Regex-heavy ghost code paths need more than the default stack in
		// debug builds; harmless for release builds.
		"-p", "thread_pool_stack=160k",
	)
	v.cmd.Stdout = logFile
	v.cmd.Stderr = logFile
	// Own process group so cleanup can reach the child process too.
	v.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := v.cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("exec.Start(%s): %v", varnishdBin, err)
	}
	go func() {
		_ = v.cmd.Wait()
		logFile.Close()
		close(v.done)
	}()
	t.Cleanup(v.stop)

	if err := v.waitForPing(); err != nil {
		t.Fatalf("varnishd did not answer ping: %v\nvarnishd log:\n%s", err, v.Log())
	}

	// Prepend the ghost VMOD dir to the build default so `import ghost;` and
	// `import std;` both resolve.
	defaultPath, err := v.paramValue("vmod_path")
	if err != nil {
		t.Fatalf("read vmod_path: %v", err)
	}
	if _, err := v.Adm("param.set", "vmod_path", vmodDir+":"+defaultPath); err != nil {
		t.Fatalf("set vmod_path: %v", err)
	}
	return v
}

// GhostVMODDir returns <repo root>/ghost/target/release if it contains
// libvmod_ghost.so.
func GhostVMODDir() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "ghost", "target", "release")
	so := filepath.Join(dir, "libvmod_ghost.so")
	if _, err := os.Stat(so); err != nil {
		return "", fmt.Errorf("%s not found; run `cargo build --release` in ghost/", so)
	}
	return dir, nil
}

// repoRoot walks up from this source file to the directory holding go.mod.
func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", filepath.Dir(file))
		}
		dir = parent
	}
}

var versionRe = regexp.MustCompile(`varnish-(?:plus-)?(\d+)\.(\d+)`)

// varnishdMajorVersion parses `varnishd -V` (printed on stderr).
func varnishdMajorVersion(bin string) (int, error) {
	out, err := exec.Command(bin, "-V").CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("exec(%s -V): %w", bin, err)
	}
	m := versionRe.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("unrecognised `varnishd -V` output: %q", strings.TrimSpace(string(out)))
	}
	return strconv.Atoi(string(m[1]))
}

func (v *Varnishd) waitForPing() error {
	deadline := time.Now().Add(pingTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-v.done:
			return errors.New("varnishd exited")
		default:
		}
		_, err := v.Adm("ping")
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %s: %w", pingTimeout, lastErr)
}

// stop SIGKILLs the manager's process group (manager and child), waits for the
// manager, then polls until the group is empty so nothing outlives the test
// and the temp dirs can be removed. A graceful SIGTERM was measured to take
// the full 5 s timeout with a running child; the workdir is disposable, so
// there is nothing to shut down cleanly.
func (v *Varnishd) stop() {
	if v.cmd == nil || v.cmd.Process == nil {
		return
	}
	pgid := v.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	select {
	case <-v.done:
	case <-time.After(stopTimeout):
		v.t.Errorf("varnishd manager (pid %d) did not exit after SIGKILL", pgid)
		return
	}
	// The child was reparented when the manager died; wait for it to go.
	deadline := time.Now().Add(stopTimeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		time.Sleep(20 * time.Millisecond)
	}
	v.t.Errorf("varnishd process group %d still alive after SIGKILL", pgid)
}

// Log returns what varnishd has written to stdout/stderr so far.
func (v *Varnishd) Log() string {
	b, err := os.ReadFile(v.logPath)
	if err != nil {
		return fmt.Sprintf("(cannot read log: %v)", err)
	}
	return string(b)
}

// WorkDir returns the -n directory.
func (v *Varnishd) WorkDir() string { return v.workDir }

// Adm runs the varnishadm CLI against this instance. A non-200 CLI status is
// returned as an error that includes the command output.
func (v *Varnishd) Adm(args ...string) (string, error) {
	cmdArgs := append([]string{"-n", v.workDir}, args...)
	cmd := exec.Command("varnishadm", cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("varnishadm(%s): %w: %s%s",
			strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String(), nil
}

// LoadVCL writes src to a file and loads it under name with vcl.load.
func (v *Varnishd) LoadVCL(name, src string) error {
	path := filepath.Join(v.vclDir, name+".vcl")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		return fmt.Errorf("os.WriteFile(%s): %w", path, err)
	}
	out, err := v.Adm("vcl.load", name, path)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "VCL compiled") {
		return fmt.Errorf("varnishadm(vcl.load %s): unexpected output: %s", name, out)
	}
	return nil
}

// Label points label at the loaded VCL name (vcl.label).
func (v *Varnishd) Label(label, name string) error {
	_, err := v.Adm("vcl.label", label, name)
	return err
}

// Use makes name the active VCL (vcl.use).
func (v *Varnishd) Use(name string) error {
	_, err := v.Adm("vcl.use", name)
	return err
}

// Start starts the child process. An active VCL must have been selected.
func (v *Varnishd) Start() error {
	_, err := v.Adm("start")
	return err
}

// ListenAddr returns host:port of the first -a listener. The child must be
// running.
func (v *Varnishd) ListenAddr() string {
	v.t.Helper()
	out, err := v.Adm("debug.listen_address")
	if err != nil {
		v.t.Fatalf("debug.listen_address: %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 {
			return f[1] + ":" + f[2]
		}
	}
	v.t.Fatalf("debug.listen_address: unexpected output %q", out)
	return ""
}

// paramValue returns a parameter's current value via `param.show -j`.
func (v *Varnishd) paramValue(name string) (string, error) {
	out, err := v.Adm("param.show", "-j", name)
	if err != nil {
		return "", err
	}
	// [version, [cmd...], timestamp, {param}, ...]
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		return "", fmt.Errorf("json.Unmarshal(param.show -j %s): %w", name, err)
	}
	if len(arr) < 4 {
		return "", fmt.Errorf("param.show -j %s: short reply %q", name, out)
	}
	var p struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(arr[3], &p); err != nil {
		return "", fmt.Errorf("json.Unmarshal(param.show -j %s entry): %w", name, err)
	}
	if p.Value == "" {
		return "", fmt.Errorf("param.show -j %s: empty value", name)
	}
	return p.Value, nil
}
