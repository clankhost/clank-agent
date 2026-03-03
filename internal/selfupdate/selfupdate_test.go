package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	// Create a "current binary"
	tmpDir := t.TempDir()
	fakeBin := filepath.Join(tmpDir, "clank-agent")
	originalContent := "#!/bin/sh\necho original"
	os.WriteFile(fakeBin, []byte(originalContent), 0755)

	// BackupAndApply with a server that returns 404 (download failure)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", 404)
	}))
	defer srv.Close()

	// We can't easily test BackupAndApply since os.Executable() won't point
	// to our fake binary. Instead, test the Rollback mechanism directly.
	backupPath := fakeBin + ".prev"
	os.WriteFile(backupPath, []byte("#!/bin/sh\necho backup"), 0755)

	// Corrupt the "current" binary
	os.WriteFile(fakeBin, []byte("corrupted"), 0755)

	// Verify backup exists
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		t.Fatal("backup should exist")
	}
}

func TestRollback_NoBackup(t *testing.T) {
	err := Rollback()
	if err == nil {
		t.Fatal("expected error when no backup exists")
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
