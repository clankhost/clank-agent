package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// createFakeArchive creates a tar.gz with a fake clank-agent binary.
func createFakeArchive(t *testing.T, binaryContent string) (archivePath string, sha string) {
	t.Helper()
	tmpDir := t.TempDir()
	archivePath = filepath.Join(tmpDir, "clank-agent_linux_amd64.tar.gz")

	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	data := []byte(binaryContent)
	tw.WriteHeader(&tar.Header{
		Name: "clank-agent",
		Mode: 0755,
		Size: int64(len(data)),
	})
	tw.Write(data)
	tw.Close()
	gw.Close()
	f.Close()

	// Compute SHA256
	raw, _ := os.ReadFile(archivePath)
	h := sha256.Sum256(raw)
	sha = hex.EncodeToString(h[:])

	return archivePath, sha
}

// serveArchive starts a test HTTP server serving the archive.
func serveArchive(t *testing.T, archivePath string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, archivePath)
	}))
}

func TestApply_SameVersion(t *testing.T) {
	err := Apply("http://example.com/archive.tar.gz", "", "", "1.0.0", "1.0.0")
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestApply_ChecksumMismatch(t *testing.T) {
	archivePath, _ := createFakeArchive(t, "#!/bin/sh\necho new")
	srv := serveArchive(t, archivePath)
	defer srv.Close()

	// Create a "current binary" for Apply to replace
	tmpDir := t.TempDir()
	fakeBin := filepath.Join(tmpDir, "clank-agent")
	os.WriteFile(fakeBin, []byte("#!/bin/sh\necho old"), 0755)

	// Use a bad checksum
	err := Apply(srv.URL, "0000000000000000000000000000000000000000000000000000000000000000", "", "1.0.0", "2.0.0")
	if err == nil {
		t.Fatal("expected error for checksum mismatch")
	}

	phase := ErrorPhase(err)
	if phase != "checksum" {
		t.Fatalf("expected phase 'checksum', got %q", phase)
	}
}

func TestApply_DownloadFailure(t *testing.T) {
	// Server returns 404
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	err := Apply(srv.URL+"/missing.tar.gz", "", "", "1.0.0", "2.0.0")
	if err == nil {
		t.Fatal("expected error for download failure")
	}

	phase := ErrorPhase(err)
	if phase != "download" {
		t.Fatalf("expected phase 'download', got %q", phase)
	}

	if !IsRetryable(err) {
		t.Fatal("download errors should be retryable")
	}
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		phase     string
		retryable bool
	}{
		{"download", true},
		{"checksum", false},
		{"extract", false},
		{"replace", false},
		{"backup", false},
		{"", false},
	}

	for _, tt := range tests {
		err := &PhaseError{Phase: tt.phase, Err: fmt.Errorf("test error")}
		if got := IsRetryable(err); got != tt.retryable {
			t.Errorf("IsRetryable(phase=%q) = %v, want %v", tt.phase, got, tt.retryable)
		}
	}
}

func TestErrorPhase_NonPhaseError(t *testing.T) {
	err := fmt.Errorf("plain error")
	if phase := ErrorPhase(err); phase != "" {
		t.Fatalf("expected empty phase for non-PhaseError, got %q", phase)
	}
}

func TestBackupAndApply_RestoresOnFailure(t *testing.T) {
	tmpDir := t.TempDir()
	fakeBin := filepath.Join(tmpDir, "clank-agent")
	originalContent := "#!/bin/sh\necho original"
	if err := os.WriteFile(fakeBin, []byte(originalContent), 0755); err != nil {
		t.Fatal(err)
	}

	origResolver := execPathResolver
	execPathResolver = func() (string, error) { return fakeBin, nil }
	t.Cleanup(func() { execPathResolver = origResolver })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	err := BackupAndApply(srv.URL+"/missing.tar.gz", "", "", "1.0.0", "2.0.0")
	if err == nil {
		t.Fatal("expected BackupAndApply to fail on 404")
	}
	if ErrorPhase(err) != "download" {
		t.Fatalf("expected phase 'download', got %q", ErrorPhase(err))
	}

	got, readErr := os.ReadFile(fakeBin)
	if readErr != nil {
		t.Fatalf("reading restored binary: %v", readErr)
	}
	if string(got) != originalContent {
		t.Fatalf("restored binary = %q, want %q", string(got), originalContent)
	}
}

func TestRollback_RestoresPreviousBinary(t *testing.T) {
	tmpDir := t.TempDir()
	fakeBin := filepath.Join(tmpDir, "clank-agent")
	backupPath := filepath.Join(tmpDir, "clank-agent.prev")

	if err := os.WriteFile(fakeBin, []byte("broken-new-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath, []byte("good-old-binary"), 0755); err != nil {
		t.Fatal(err)
	}

	origResolver := execPathResolver
	execPathResolver = func() (string, error) { return fakeBin, nil }
	t.Cleanup(func() { execPathResolver = origResolver })

	if err := Rollback(); err != nil {
		t.Fatalf("Rollback() failed: %v", err)
	}

	got, err := os.ReadFile(fakeBin)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "good-old-binary" {
		t.Fatalf("binary after rollback = %q, want %q", string(got), "good-old-binary")
	}
}

func TestApply_EnforcesHTTPS(t *testing.T) {
	err := Apply("http://example.com/archive.tar.gz", "", "", "1.0.0", "2.0.0")
	if err == nil {
		t.Fatal("expected error for non-loopback HTTP URL")
	}
	if !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("expected HTTPS error, got %v", err)
	}
}

func TestApplyContext_HonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()

	err := ApplyContext(ctx, srv.URL+"/slow.tar.gz", "", "", "1.0.0", "2.0.0")
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
	if ErrorPhase(err) != "download" {
		t.Fatalf("expected phase 'download', got %q", ErrorPhase(err))
	}
}

func TestRollback_NoBackup(t *testing.T) {
	tmpDir := t.TempDir()
	fakeBin := filepath.Join(tmpDir, "clank-agent")
	if err := os.WriteFile(fakeBin, []byte("bin"), 0755); err != nil {
		t.Fatal(err)
	}
	origResolver := execPathResolver
	execPathResolver = func() (string, error) { return fakeBin, nil }
	t.Cleanup(func() { execPathResolver = origResolver })

	err := Rollback()
	if err == nil {
		t.Fatal("expected error when no backup exists")
	}
}

func TestBinDir_ReturnsNonEmpty(t *testing.T) {
	dir := BinDir()
	if dir == "" {
		t.Fatal("BinDir() returned empty string")
	}
}

func TestState_SaveLoadClear(t *testing.T) {
	dir := t.TempDir()

	// Initially nil
	if s := LoadState(dir); s != nil {
		t.Fatal("expected nil state initially")
	}

	// Save
	state := &UpdateState{
		Status:      "pending",
		FromVersion: "1.0.0",
		ToVersion:   "2.0.0",
		Attempts:    1,
	}
	if err := SaveState(dir, state); err != nil {
		t.Fatal(err)
	}

	// Load
	loaded := LoadState(dir)
	if loaded == nil {
		t.Fatal("expected non-nil state after save")
	}
	if loaded.Status != "pending" || loaded.FromVersion != "1.0.0" || loaded.ToVersion != "2.0.0" || loaded.Attempts != 1 {
		t.Fatalf("loaded state mismatch: %+v", loaded)
	}

	// Clear
	ClearState(dir)
	if s := LoadState(dir); s != nil {
		t.Fatal("expected nil state after clear")
	}
}
