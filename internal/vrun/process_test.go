package vrun

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/varnish/gateway/internal/varnishadm"
)

func TestManagerCreation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	workDir := "/tmp/test-varnish"
	varnishDir := filepath.Join(workDir, "varnish")

	mgr := New(workDir, logger, varnishDir)
	if mgr == nil {
		t.Fatal("Manager creation failed")
	}
	if mgr.workDir != workDir {
		t.Errorf("Expected workDir %s, got %s", workDir, mgr.workDir)
	}
	if mgr.varnishDir != filepath.Join(workDir, "varnish") {
		t.Errorf("Expected varnishDir %s, got %s", filepath.Join(workDir, "varnish"), mgr.varnishDir)
	}
}

func TestPrepareWorkspace(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	workDir := t.TempDir()
	varnishDir := filepath.Join(workDir, "varnish")

	mgr := New(workDir, logger, varnishDir)

	err := mgr.PrepareWorkspace()
	if err != nil {
		t.Fatalf("PrepareWorkspace failed: %v", err)
	}

	// Check varnish directory exists
	if _, err := os.Stat(mgr.varnishDir); os.IsNotExist(err) {
		t.Errorf("Varnish directory was not created: %s", mgr.varnishDir)
	}

	// Check secret file exists
	secretPath := filepath.Join(workDir, "secret")
	if _, err := os.Stat(secretPath); os.IsNotExist(err) {
		t.Error("Secret file was not created")
	}

	// Check secret is set
	if mgr.secret == "" {
		t.Error("Secret was not generated")
	}
}

func TestBuildArgs(t *testing.T) {
	cfg := &Config{
		AdminPort:  6082,
		WorkDir:    "/tmp/test",
		VarnishDir: "/tmp/test/varnish",
		Listen:     []string{":8080,http"},
		Storage:    []string{"malloc,256m"},
		Params:     map[string]string{"thread_pool_min": "10"},
	}

	args, err := BuildArgs(cfg)
	if err != nil {
		t.Fatalf("BuildArgs failed: %v", err)
	}

	// Check expected arguments
	expectedArgs := []string{"-n", "/tmp/test/varnish", "-F", "-f", "", "-a", ":8080,http"}
	for _, expected := range expectedArgs {
		found := false
		for _, arg := range args {
			if arg == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected argument %s not found in args: %v", expected, args)
		}
	}

	// Verify storage args
	storageFound := false
	for i, arg := range args {
		if arg == "-s" && i+1 < len(args) && args[i+1] == "malloc,256m" {
			storageFound = true
			break
		}
	}
	if !storageFound {
		t.Error("Storage arguments not found in args")
	}

	// Verify params
	paramFound := false
	for i, arg := range args {
		if arg == "-p" && i+1 < len(args) && args[i+1] == "thread_pool_min=10" {
			paramFound = true
			break
		}
	}
	if !paramFound {
		t.Error("Param arguments not found in args")
	}
}

func TestBuildArgsWithExtraArgs(t *testing.T) {
	cfg := &Config{
		AdminPort:  6082,
		WorkDir:    "/tmp/test",
		VarnishDir: "/tmp/test/varnish",
		ExtraArgs:  []string{"-p", "thread_pools=4", "-p", "workspace_client=256k"},
	}

	args, err := BuildArgs(cfg)
	if err != nil {
		t.Fatalf("BuildArgs failed: %v", err)
	}

	// Verify extra args are appended at the end
	// The last 4 elements should be our extra args
	if len(args) < 4 {
		t.Fatalf("Expected at least 4 args, got %d", len(args))
	}

	tail := args[len(args)-4:]
	expected := []string{"-p", "thread_pools=4", "-p", "workspace_client=256k"}
	for i, exp := range expected {
		if tail[i] != exp {
			t.Errorf("Expected extra arg[%d] = %s, got %s", i, exp, tail[i])
		}
	}
}

func TestBuildArgsProtectedFlagRejection(t *testing.T) {
	protectedTests := []struct {
		name      string
		extraArgs []string
	}{
		{"reject -M", []string{"-M", "localhost:6082"}},
		{"reject -S", []string{"-S", "/path/to/secret"}},
		{"reject -F", []string{"-F"}},
		{"reject -f", []string{"-f", "/path/to/vcl"}},
		{"reject -n", []string{"-n", "/var/lib/varnish"}},
	}

	for _, tt := range protectedTests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				AdminPort: 6082,
				WorkDir:   "/tmp/test",
				ExtraArgs: tt.extraArgs,
			}

			_, err := BuildArgs(cfg)
			if err == nil {
				t.Errorf("Expected error for protected flag, got nil")
			}
		})
	}
}

func TestBuildArgsAllowsNonProtectedFlags(t *testing.T) {
	cfg := &Config{
		AdminPort: 6082,
		WorkDir:   "/tmp/test",
		ExtraArgs: []string{"-a", ":9090,http", "-s", "file,/tmp/storage,1g", "-p", "feature=+http2"},
	}

	args, err := BuildArgs(cfg)
	if err != nil {
		t.Fatalf("BuildArgs should allow non-protected flags, got error: %v", err)
	}

	// Verify extra args are present
	extraArgsStr := strings.Join(args, " ")
	if !strings.Contains(extraArgsStr, "-a :9090,http") {
		t.Errorf("Expected extra -a arg in output")
	}
}

func TestGetParamName(t *testing.T) {
	// Create test structs with yaml tags
	type testStruct struct {
		SimpleParam   string `yaml:"simple_param"`
		WithOmitempty string `yaml:"with_omitempty,omitempty"`
		ThreadPoolMax int    `yaml:"thread_pool_max,omitempty"`
		NoYamlTag     string // Should return empty string
		YamlDash      string `yaml:"-"` // Should return empty string (explicitly ignored)
		HTTPMaxHdr    int    `yaml:"http_max_hdr,omitempty"`
	}

	tests := []struct {
		fieldName string
		expected  string
	}{
		{"SimpleParam", "simple_param"},
		{"WithOmitempty", "with_omitempty"},
		{"ThreadPoolMax", "thread_pool_max"},
		{"NoYamlTag", ""},
		{"YamlDash", ""},
		{"HTTPMaxHdr", "http_max_hdr"},
	}

	structType := reflect.TypeOf(testStruct{})
	for _, tt := range tests {
		field, found := structType.FieldByName(tt.fieldName)
		if !found {
			t.Fatalf("Field %s not found in test struct", tt.fieldName)
		}
		result := GetParamName(field)
		if result != tt.expected {
			t.Errorf("GetParamName(%s) = %s, expected %s", tt.fieldName, result, tt.expected)
		}
	}
}

func TestIntegrationStartVarnish(t *testing.T) {
	// Skip on macOS due to varnishd VSUB_closefrom() compatibility issues
	if os.Getenv("GOOS") == "darwin" || (os.Getenv("GOOS") == "" && runtime.GOOS == "darwin") {
		t.Skip("skipping on macOS: varnishd has VSUB_closefrom() compatibility issues")
	}

	// Skip if varnishd not available
	if _, err := exec.LookPath("varnishd"); err != nil {
		t.Skip("varnishd not found in PATH, skipping integration test")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	workDir := t.TempDir()
	varnishDir := filepath.Join(workDir, "varnish")

	mgr := New(workDir, logger, varnishDir)

	if err := mgr.PrepareWorkspace(); err != nil {
		t.Fatalf("PrepareWorkspace failed: %v", err)
	}

	// Get a free port for admin connection
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to get free port: %v", err)
	}
	adminPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close() // Close so varnishadm server can use it

	// Create varnishadm server to handle admin connections
	admServer := varnishadm.New(uint16(adminPort), mgr.GetSecret(), logger)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start varnishadm server in background (it will accept connection from varnishd)
	admErr := make(chan error, 1)
	go func() {
		admErr <- admServer.Run(ctx)
	}()

	cfg := &Config{
		WorkDir:    workDir,
		AdminPort:  adminPort,
		VarnishDir: varnishDir,
		Listen:     []string{"127.0.0.1:0,http"},
		Storage:    []string{"malloc,32m"},
	}
	args, err := BuildArgs(cfg)
	if err != nil {
		t.Fatalf("BuildArgs failed: %v", err)
	}

	// Start varnishd (non-blocking) - starts without VCL
	ready, err := mgr.Start(ctx, "", args)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for varnishadm connection
	select {
	case <-admServer.Connected():
		t.Log("Varnishadm connected")
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("Timeout waiting for varnishadm connection")
	}

	// Create a minimal VCL file
	vclPath := filepath.Join(workDir, "test.vcl")
	vclContent := `vcl 4.1;
backend default none;
`
	if err := os.WriteFile(vclPath, []byte(vclContent), 0644); err != nil {
		cancel()
		t.Fatalf("Failed to write VCL file: %v", err)
	}

	// Load and use the VCL
	resp, err := admServer.VCLLoad("boot", vclPath)
	if err != nil {
		cancel()
		t.Fatalf("Failed to load VCL: %v", err)
	}
	t.Logf("VCL load response: %s", resp.Payload())

	resp, err = admServer.VCLUse("boot")
	if err != nil {
		cancel()
		t.Fatalf("Failed to use VCL: %v", err)
	}
	t.Logf("VCL use response: %s", resp.Payload())

	// Start the child process
	resp, err = admServer.Start()
	if err != nil {
		cancel()
		t.Fatalf("Failed to start child: %v", err)
	}
	t.Logf("Start response: %s", resp.Payload())

	// Wait for varnishd child to be ready
	select {
	case <-ready:
		t.Log("Varnish child is ready")
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("Timeout waiting for varnish child to be ready")
	}

	// Verify varnishd is responding via admin interface
	resp, err = admServer.Status()
	if err != nil {
		cancel()
		t.Fatalf("Failed to get status: %v", err)
	}
	t.Logf("Varnish status: %s", resp.Payload())

	// Stop varnishd
	cancel()

	// Wait for process to exit
	waitErr := make(chan error, 1)
	go func() {
		waitErr <- mgr.Wait()
	}()

	select {
	case err := <-waitErr:
		// Context cancellation causes the process to be killed, which is expected
		if err != nil {
			t.Logf("Wait returned (expected due to cancellation): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Timed out waiting for varnishd to stop")
	}
}
