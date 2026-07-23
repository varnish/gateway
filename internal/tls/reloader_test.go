package tls

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/varnish/gateway/internal/varnishadm"
)

// TestShouldReload is the deterministic, platform-independent guard for the
// H-9 fix: fsnotify events on the Kubernetes atomic-writer "..data" symlink
// must trigger a reload (previously only .pem events did, so Secret rotations
// were silently missed). It also confirms direct .pem writes still reload.
func TestShouldReload(t *testing.T) {
	const base = "/etc/varnish/tls"
	tests := []struct {
		name  string
		event fsnotify.Event
		want  bool
	}{
		{
			name:  "k8s atomic-writer swap (rename-over surfaces as Create on ..data)",
			event: fsnotify.Event{Name: base + "/..data", Op: fsnotify.Create},
			want:  true,
		},
		{
			name:  "k8s ..data swap seen as Rename",
			event: fsnotify.Event{Name: base + "/..data", Op: fsnotify.Rename},
			want:  true,
		},
		{
			name:  "direct .pem write still reloads",
			event: fsnotify.Event{Name: base + "/my-cert.pem", Op: fsnotify.Write},
			want:  true,
		},
		{
			name:  "new .pem created still reloads",
			event: fsnotify.Event{Name: base + "/new-cert.pem", Op: fsnotify.Create},
			want:  true,
		},
		{
			name:  ".pem removed still reloads",
			event: fsnotify.Event{Name: base + "/gone.pem", Op: fsnotify.Remove},
			want:  true,
		},
		{
			name:  "atomic-writer temp symlink does not reload",
			event: fsnotify.Event{Name: base + "/..data_tmp", Op: fsnotify.Create},
			want:  false,
		},
		{
			name:  "timestamped data dir does not reload on its own",
			event: fsnotify.Event{Name: base + "/..2026_07_23_10_00_00.111111", Op: fsnotify.Create},
			want:  false,
		},
		{
			name:  "unrelated file does not reload",
			event: fsnotify.Event{Name: base + "/notes.txt", Op: fsnotify.Write},
			want:  false,
		},
		{
			name:  "chmod-only on .pem does not reload",
			event: fsnotify.Event{Name: base + "/my-cert.pem", Op: fsnotify.Chmod},
			want:  false,
		},
		{
			name:  "chmod-only on ..data does not reload",
			event: fsnotify.Event{Name: base + "/..data", Op: fsnotify.Chmod},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldReload(tt.event); got != tt.want {
				t.Errorf("shouldReload(%q, %s) = %v, want %v", tt.event.Name, tt.event.Op, got, tt.want)
			}
		})
	}
}

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

	// Verify rollback was called after the load error
	history := mock.GetCallHistory()
	var foundRollback bool
	for _, cmd := range history {
		if cmd == "tls.cert.rollback" {
			foundRollback = true
		}
	}
	if !foundRollback {
		t.Errorf("LoadAll() did not call tls.cert.rollback after load error; history: %v", history)
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

// writeK8sSecretVolume lays out a directory the way Kubernetes' atomic-writer
// populates a mounted Secret volume: cert data lives in a timestamped
// "..<timestamp>" directory, "..data" is a symlink to it, and each visible
// entry (tls.crt, tls.key, *.pem) is a symlink into "..data".
// It returns the name of the timestamped data directory it created.
func writeK8sSecretVolume(t *testing.T, dir, timestampDir string, files map[string][]byte) string {
	t.Helper()

	dataDir := filepath.Join(dir, timestampDir)
	if err := os.Mkdir(dataDir, 0755); err != nil {
		t.Fatalf("os.Mkdir(%s): %v", dataDir, err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dataDir, name), content, 0644); err != nil {
			t.Fatalf("os.WriteFile(%s): %v", name, err)
		}
	}

	// ..data -> ..<timestamp>
	dataLink := filepath.Join(dir, "..data")
	if err := os.Symlink(timestampDir, dataLink); err != nil {
		t.Fatalf("os.Symlink(..data): %v", err)
	}

	// Visible entries are symlinks into ..data.
	for name := range files {
		visible := filepath.Join(dir, name)
		if err := os.Symlink(filepath.Join("..data", name), visible); err != nil {
			t.Fatalf("os.Symlink(%s): %v", name, err)
		}
	}

	return timestampDir
}

// rotateK8sSecretVolume simulates cert-manager renewing the Secret: the
// atomic-writer writes a fresh "..<timestamp>" directory and atomically swaps
// the "..data" symlink to point at it (symlink to a temp name + rename over
// "..data"). Crucially, the visible *.pem symlinks are NOT recreated — matching
// real Kubernetes behavior — so only "..data" changes on disk.
func rotateK8sSecretVolume(t *testing.T, dir, newTimestampDir string, files map[string][]byte) {
	t.Helper()

	newDataDir := filepath.Join(dir, newTimestampDir)
	if err := os.Mkdir(newDataDir, 0755); err != nil {
		t.Fatalf("os.Mkdir(%s): %v", newDataDir, err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(newDataDir, name), content, 0644); err != nil {
			t.Fatalf("os.WriteFile(%s): %v", name, err)
		}
	}

	// Atomic swap: create a temporary symlink, then rename it over ..data.
	tmpLink := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(newTimestampDir, tmpLink); err != nil {
		t.Fatalf("os.Symlink(..data_tmp): %v", err)
	}
	if err := os.Rename(tmpLink, filepath.Join(dir, "..data")); err != nil {
		t.Fatalf("os.Rename(..data): %v", err)
	}
}

// TestRun_K8sSecretRotationTriggersReload verifies the reloader reacts to a
// Kubernetes Secret volume rotation. Real Secret volumes never fire a *.pem
// fsnotify event on renewal (the visible .pem entries are stable symlinks);
// only the "..data" symlink is swapped. The reloader must watch "..data" or it
// serves the stale cert until pod restart.
func TestRun_K8sSecretRotationTriggersReload(t *testing.T) {
	// This is an OS-level integration test of the fsnotify watch path. On
	// Linux (inotify) the atomic-writer's rename-over-"..data" surfaces as an
	// IN_MOVED_TO -> Create event named ".../..data", which the filter catches.
	// macOS (kqueue) detects directory changes by name-diffing and cannot
	// observe an in-place symlink swap (the "..data" name is unchanged across
	// the rename), so the event never fires there regardless of the filter.
	// The deterministic guard for the fix is TestShouldReload; this test
	// exercises the real watcher on the platform that behaves like production.
	if runtime.GOOS != "linux" {
		t.Skipf("kqueue on %s cannot observe an in-place '..data' symlink swap; see TestShouldReload", runtime.GOOS)
	}

	dir := t.TempDir()

	certFiles := map[string][]byte{
		"tls.crt":     []byte("-----BEGIN CERTIFICATE-----\ninitial\n-----END CERTIFICATE-----\n"),
		"tls.key":     []byte("-----BEGIN PRIVATE KEY-----\ninitial\n-----END PRIVATE KEY-----\n"),
		"my-cert.pem": []byte("initial cert+key PEM"),
	}
	writeK8sSecretVolume(t, dir, "..2026_07_23_10_00_00.111111", certFiles)

	mock := varnishadm.NewMock(0, "", slog.Default())
	// The my-cert.pem symlink is already loaded (as if via LoadAll at startup).
	mock.SetTLSState([]varnishadm.TLSCertEntry{
		{CertificateID: "my-cert", Frontend: "main", State: "active", Hostname: "my-cert"},
	})

	r := New(mock, dir, slog.Default())
	r.debounceDelay = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- r.Run(ctx)
	}()

	// Give the watcher time to start.
	time.Sleep(100 * time.Millisecond)

	// Rotate: only "..data" changes on disk, no .pem event fires.
	mock.ClearCallHistory()
	rotateK8sSecretVolume(t, dir, "..2026_07_23_11_00_00.222222", map[string][]byte{
		"tls.crt":     []byte("-----BEGIN CERTIFICATE-----\nrenewed\n-----END CERTIFICATE-----\n"),
		"tls.key":     []byte("-----BEGIN PRIVATE KEY-----\nrenewed\n-----END PRIVATE KEY-----\n"),
		"my-cert.pem": []byte("renewed cert+key PEM"),
	})

	// Wait for debounce + processing.
	time.Sleep(300 * time.Millisecond)

	// The rotation must have triggered a full reload cycle.
	history := mock.GetCallHistory()
	var foundList, foundDiscard, foundLoad, foundCommit bool
	for _, cmd := range history {
		if cmd == "tls.cert.list" {
			foundList = true
		}
		if cmd == "tls.cert.discard my-cert" {
			foundDiscard = true
		}
		if strings.HasPrefix(cmd, "tls.cert.load my-cert ") {
			foundLoad = true
		}
		if cmd == "tls.cert.commit" {
			foundCommit = true
		}
	}
	if !foundList {
		t.Errorf("Run() did not call tls.cert.list after Secret rotation; history: %v", history)
	}
	if !foundDiscard {
		t.Errorf("Run() did not discard existing cert after Secret rotation; history: %v", history)
	}
	if !foundLoad {
		t.Errorf("Run() did not reload cert after Secret rotation; history: %v", history)
	}
	if !foundCommit {
		t.Errorf("Run() did not commit after Secret rotation; history: %v", history)
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
