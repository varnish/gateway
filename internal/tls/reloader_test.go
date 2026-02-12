package tls

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	// Write initial PEM file
	pemPath := filepath.Join(dir, "test.pem")
	if err := os.WriteFile(pemPath, []byte("initial"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := varnishadm.NewMock(0, "", slog.Default())

	// Simulate initial cert loaded via LoadAll
	mock.SetTLSState([]varnishadm.TLSCertEntry{
		{CertificateID: "test", Frontend: "main", State: "active", Hostname: "test"},
	})

	r := New(mock, dir, slog.Default())
	r.debounceDelay = 50 * time.Millisecond

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

	// Should see: tls.cert.list, tls.cert.discard test, tls.cert.load test <path>, tls.cert.commit
	history := mock.GetCallHistory()
	var foundList, foundDiscard, foundLoad, foundCommit bool
	for _, cmd := range history {
		if cmd == "tls.cert.list" {
			foundList = true
		}
		if cmd == "tls.cert.discard test" {
			foundDiscard = true
		}
		if strings.HasPrefix(cmd, "tls.cert.load test ") {
			foundLoad = true
		}
		if cmd == "tls.cert.commit" {
			foundCommit = true
		}
	}
	if !foundList {
		t.Errorf("Run() did not call tls.cert.list; history: %v", history)
	}
	if !foundDiscard {
		t.Errorf("Run() did not call tls.cert.discard for existing cert; history: %v", history)
	}
	if !foundLoad {
		t.Errorf("Run() did not call tls.cert.load for cert; history: %v", history)
	}
	if !foundCommit {
		t.Errorf("Run() did not call tls.cert.commit; history: %v", history)
	}

	cancel()
}

func TestRun_NewCertDiscovered(t *testing.T) {
	dir := t.TempDir()
	// Write initial PEM file
	if err := os.WriteFile(filepath.Join(dir, "existing.pem"), []byte("cert1"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := varnishadm.NewMock(0, "", slog.Default())

	// Simulate one cert already loaded
	mock.SetTLSState([]varnishadm.TLSCertEntry{
		{CertificateID: "existing", Frontend: "main", State: "active", Hostname: "existing"},
	})

	r := New(mock, dir, slog.Default())
	r.debounceDelay = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- r.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)

	// Add a NEW .pem file (simulating a new certificateRef)
	mock.ClearCallHistory()
	newPemPath := filepath.Join(dir, "new-cert.pem")
	if err := os.WriteFile(newPemPath, []byte("new cert data"), 0644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)

	// Should see the full cycle: list, discard existing, load existing + new-cert, commit
	history := mock.GetCallHistory()
	var loadedCerts []string
	for _, cmd := range history {
		if strings.HasPrefix(cmd, "tls.cert.load ") {
			// Extract cert name: "tls.cert.load <name> <path>"
			parts := strings.Fields(cmd)
			if len(parts) >= 3 {
				loadedCerts = append(loadedCerts, parts[1])
			}
		}
	}

	// Both existing and new-cert should be loaded
	wantCerts := map[string]bool{"existing": false, "new-cert": false}
	for _, name := range loadedCerts {
		if _, ok := wantCerts[name]; ok {
			wantCerts[name] = true
		}
	}
	for name, found := range wantCerts {
		if !found {
			t.Errorf("Run() did not load cert %q after new cert added; loaded: %v, history: %v", name, loadedCerts, history)
		}
	}

	// After commit, both certs should be in the committed state
	state := mock.GetTLSState()
	if len(state) != 2 {
		t.Errorf("Expected 2 committed certs, got %d", len(state))
	}

	cancel()
}

func TestRun_CertRemovedTriggersReload(t *testing.T) {
	dir := t.TempDir()
	// Write two initial PEM files
	if err := os.WriteFile(filepath.Join(dir, "keep.pem"), []byte("cert1"), 0644); err != nil {
		t.Fatal(err)
	}
	removePath := filepath.Join(dir, "remove.pem")
	if err := os.WriteFile(removePath, []byte("cert2"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := varnishadm.NewMock(0, "", slog.Default())

	// Simulate both certs loaded
	mock.SetTLSState([]varnishadm.TLSCertEntry{
		{CertificateID: "keep", Frontend: "main", State: "active", Hostname: "keep"},
		{CertificateID: "remove", Frontend: "main", State: "active", Hostname: "remove"},
	})

	r := New(mock, dir, slog.Default())
	r.debounceDelay = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- r.Run(ctx)
	}()

	time.Sleep(100 * time.Millisecond)

	// Remove one PEM file
	mock.ClearCallHistory()
	if err := os.Remove(removePath); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)

	// After the cycle, only "keep" should be loaded
	history := mock.GetCallHistory()
	var foundCommit bool
	var loadedCerts []string
	for _, cmd := range history {
		if strings.HasPrefix(cmd, "tls.cert.load ") {
			parts := strings.Fields(cmd)
			if len(parts) >= 3 {
				loadedCerts = append(loadedCerts, parts[1])
			}
		}
		if cmd == "tls.cert.commit" {
			foundCommit = true
		}
	}

	if !foundCommit {
		t.Errorf("Run() did not call tls.cert.commit after cert removal; history: %v", history)
	}

	// Only "keep" should have been loaded
	if len(loadedCerts) != 1 || loadedCerts[0] != "keep" {
		t.Errorf("Expected only 'keep' cert to be loaded, got: %v; history: %v", loadedCerts, history)
	}

	// Committed state should only have "keep"
	state := mock.GetTLSState()
	if len(state) != 1 {
		t.Errorf("Expected 1 committed cert after removal, got %d", len(state))
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
		if cmd == "tls.cert.list" || strings.HasPrefix(cmd, "tls.cert.load") || cmd == "tls.cert.commit" {
			t.Errorf("Run() should not trigger reload for non-PEM file changes; history: %v", history)
			break
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

func TestReloadAllCerts_NoCertsToNoCerts(t *testing.T) {
	dir := t.TempDir()
	mock := varnishadm.NewMock(0, "", slog.Default())
	r := New(mock, dir, slog.Default())

	if err := r.reloadAllCerts(); err != nil {
		t.Fatalf("reloadAllCerts() unexpected error: %v", err)
	}

	// No certs loaded, no files on disk - should not commit
	history := mock.GetCallHistory()
	for _, cmd := range history {
		if cmd == "tls.cert.commit" {
			t.Error("reloadAllCerts() should not commit when nothing to do")
		}
	}
}

func TestReloadAllCerts_AddNewCerts(t *testing.T) {
	dir := t.TempDir()
	// No certs currently loaded, but files on disk
	if err := os.WriteFile(filepath.Join(dir, "new.pem"), []byte("cert"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := varnishadm.NewMock(0, "", slog.Default())
	r := New(mock, dir, slog.Default())

	if err := r.reloadAllCerts(); err != nil {
		t.Fatalf("reloadAllCerts() unexpected error: %v", err)
	}

	history := mock.GetCallHistory()
	var foundLoad, foundCommit bool
	for _, cmd := range history {
		if strings.HasPrefix(cmd, "tls.cert.load new ") {
			foundLoad = true
		}
		if cmd == "tls.cert.commit" {
			foundCommit = true
		}
	}
	if !foundLoad {
		t.Errorf("reloadAllCerts() did not load new cert; history: %v", history)
	}
	if !foundCommit {
		t.Errorf("reloadAllCerts() did not commit; history: %v", history)
	}
}

func TestReloadAllCerts_RemoveAllCerts(t *testing.T) {
	dir := t.TempDir()
	// Cert loaded but no files on disk

	mock := varnishadm.NewMock(0, "", slog.Default())
	mock.SetTLSState([]varnishadm.TLSCertEntry{
		{CertificateID: "old", Frontend: "main", State: "active", Hostname: "old"},
	})

	r := New(mock, dir, slog.Default())

	if err := r.reloadAllCerts(); err != nil {
		t.Fatalf("reloadAllCerts() unexpected error: %v", err)
	}

	// Should discard "old" and commit
	history := mock.GetCallHistory()
	var foundDiscard, foundCommit bool
	for _, cmd := range history {
		if cmd == "tls.cert.discard old" {
			foundDiscard = true
		}
		if cmd == "tls.cert.commit" {
			foundCommit = true
		}
	}
	if !foundDiscard {
		t.Errorf("reloadAllCerts() did not discard old cert; history: %v", history)
	}
	if !foundCommit {
		t.Errorf("reloadAllCerts() did not commit; history: %v", history)
	}

	// No certs should remain
	state := mock.GetTLSState()
	if len(state) != 0 {
		t.Errorf("Expected 0 committed certs, got %d", len(state))
	}
}
