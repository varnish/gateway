package tls

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/varnish/gateway/internal/varnishadm"
)

func TestLoadAll_NoPEMFiles(t *testing.T) {
	dir := t.TempDir()
	mock := varnishadm.NewMock(0, "", slog.Default())
	r := New(mock, dir, slog.Default())

	if err := r.LoadAll(); err != nil {
		t.Fatalf("LoadAll() unexpected error: %v", err)
	}

	// Should not have called tls.cert.load or tls.cert.commit
	history := mock.GetCallHistory()
	for _, cmd := range history {
		if cmd == "tls.cert.commit" {
			t.Error("LoadAll() should not call tls.cert.commit when no PEM files found")
		}
	}
}

func TestLoadAll_SingleCert(t *testing.T) {
	dir := t.TempDir()
	// Write a fake PEM file
	pemData := []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----\n")
	if err := os.WriteFile(filepath.Join(dir, "my-cert.pem"), pemData, 0644); err != nil {
		t.Fatal(err)
	}

	mock := varnishadm.NewMock(0, "", slog.Default())
	r := New(mock, dir, slog.Default())

	if err := r.LoadAll(); err != nil {
		t.Fatalf("LoadAll() unexpected error: %v", err)
	}

	// Verify tls.cert.load was called
	history := mock.GetCallHistory()
	var foundLoad, foundCommit bool
	for _, cmd := range history {
		if cmd == "tls.cert.load my-cert "+filepath.Join(dir, "my-cert.pem") {
			foundLoad = true
		}
		if cmd == "tls.cert.commit" {
			foundCommit = true
		}
	}
	if !foundLoad {
		t.Errorf("LoadAll() did not call tls.cert.load for my-cert; history: %v", history)
	}
	if !foundCommit {
		t.Errorf("LoadAll() did not call tls.cert.commit; history: %v", history)
	}
}

func TestLoadAll_MultipleCerts(t *testing.T) {
	dir := t.TempDir()
	pemData := []byte("cert+key PEM data")

	for _, name := range []string{"alpha.pem", "beta.pem", "gamma.pem"} {
		if err := os.WriteFile(filepath.Join(dir, name), pemData, 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Also write a non-pem file that should be ignored
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("ignored"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := varnishadm.NewMock(0, "", slog.Default())
	r := New(mock, dir, slog.Default())

	if err := r.LoadAll(); err != nil {
		t.Fatalf("LoadAll() unexpected error: %v", err)
	}

	// Should have loaded exactly 3 certs + 1 commit
	history := mock.GetCallHistory()
	loadCount := 0
	commitCount := 0
	for _, cmd := range history {
		if len(cmd) > 14 && cmd[:14] == "tls.cert.load " {
			loadCount++
		}
		if cmd == "tls.cert.commit" {
			commitCount++
		}
	}
	if loadCount != 3 {
		t.Errorf("LoadAll() loaded %d certs, want 3; history: %v", loadCount, history)
	}
	if commitCount != 1 {
		t.Errorf("LoadAll() called tls.cert.commit %d times, want 1", commitCount)
	}
}

func TestLoadAll_ErrorOnLoad(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.pem"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := varnishadm.NewMock(0, "", slog.Default())
	// Set error response for the load command
	mock.SetResponse("tls.cert.load bad "+filepath.Join(dir, "bad.pem"), varnishadm.VarnishResponse{})
	// Override the Exec to return an error status - use a custom response
	mock.SetResponse("tls.cert.load bad "+filepath.Join(dir, "bad.pem"),
		varnishadm.NewVarnishResponse(300, "Certificate file not found"))

	r := New(mock, dir, slog.Default())

	err := r.LoadAll()
	if err == nil {
		t.Fatal("LoadAll() expected error, got nil")
	}
}

func TestLoadAll_DirectoryNotExist(t *testing.T) {
	mock := varnishadm.NewMock(0, "", slog.Default())
	r := New(mock, "/nonexistent/path", slog.Default())

	err := r.LoadAll()
	if err == nil {
		t.Fatal("LoadAll() expected error for nonexistent directory, got nil")
	}
}

func TestRun_FileChangeTriggersReload(t *testing.T) {
	dir := t.TempDir()
	// Write initial PEM file so watcher has something to watch
	pemPath := filepath.Join(dir, "test.pem")
	if err := os.WriteFile(pemPath, []byte("initial"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := varnishadm.NewMock(0, "", slog.Default())
	r := New(mock, dir, slog.Default())
	r.debounceDelay = 50 * time.Millisecond // Speed up for tests

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- r.Run(ctx)
	}()

	// Give the watcher time to start
	time.Sleep(100 * time.Millisecond)

	// Modify the PEM file
	mock.ClearCallHistory()
	if err := os.WriteFile(pemPath, []byte("updated cert data"), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for debounce + processing
	time.Sleep(300 * time.Millisecond)

	history := mock.GetCallHistory()
	var foundReload bool
	for _, cmd := range history {
		if cmd == "tls.cert.reload" {
			foundReload = true
		}
	}
	if !foundReload {
		t.Errorf("Run() did not call tls.cert.reload after file change; history: %v", history)
	}

	cancel()
}

func TestRun_NonPEMFileIgnored(t *testing.T) {
	dir := t.TempDir()

	mock := varnishadm.NewMock(0, "", slog.Default())
	r := New(mock, dir, slog.Default())
	r.debounceDelay = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- r.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)

	mock.ClearCallHistory()
	// Write a non-PEM file
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)

	history := mock.GetCallHistory()
	for _, cmd := range history {
		if cmd == "tls.cert.reload" {
			t.Error("Run() should not reload for non-PEM file changes")
		}
	}

	cancel()
}

func TestRun_ContextCancelStops(t *testing.T) {
	dir := t.TempDir()

	mock := varnishadm.NewMock(0, "", slog.Default())
	r := New(mock, dir, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- r.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Errorf("Run() returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not stop after context cancel")
	}
}
