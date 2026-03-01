// Package selfupdate downloads and applies agent binary updates.
package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PhaseError wraps an error with the update phase where it occurred.
type PhaseError struct {
	Phase string // "download", "checksum", "signature", "extract", "replace", "backup"
	Err   error
}

func (e *PhaseError) Error() string {
	return fmt.Sprintf("%s: %v", e.Phase, e.Err)
}

func (e *PhaseError) Unwrap() error {
	return e.Err
}

// ErrorPhase extracts the phase from a PhaseError, or returns "" if not a PhaseError.
func ErrorPhase(err error) string {
	if pe, ok := err.(*PhaseError); ok {
		return pe.Phase
	}
	return ""
}

// IsRetryable returns true if the error occurred in a phase where retrying
// may succeed (e.g., transient download failures).
func IsRetryable(err error) bool {
	phase := ErrorPhase(err)
	return phase == "download"
}

// Apply downloads the new agent binary, verifies its signature and checksum,
// and replaces the current binary atomically. Returns nil on success — the
// caller should exit to let systemd restart with the new binary.
//
// If signature is non-empty and a signing public key is embedded, the archive's
// ECDSA P-256 signature is verified before proceeding. This prevents supply-chain
// attacks where a compromised control plane serves a malicious binary with
// a matching checksum.
func Apply(downloadURL, expectedSHA256, signature, currentVersion, newVersion string) error {
	if currentVersion == newVersion {
		log.Printf("[update] Already running version %s, skipping", currentVersion)
		return nil
	}

	log.Printf("[update] Updating from %s to %s", currentVersion, newVersion)

	// 1. Determine current binary path
	execPath, err := os.Executable()
	if err != nil {
		return &PhaseError{Phase: "replace", Err: fmt.Errorf("resolving executable path: %w", err)}
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return &PhaseError{Phase: "replace", Err: fmt.Errorf("resolving symlinks: %w", err)}
	}

	// 2. Download archive to temp dir
	log.Printf("[update] Downloading %s", downloadURL)
	tmpDir, err := os.MkdirTemp("", "clank-update-*")
	if err != nil {
		return &PhaseError{Phase: "download", Err: fmt.Errorf("creating temp dir: %w", err)}
	}
	defer os.RemoveAll(tmpDir)

	archivePath := filepath.Join(tmpDir, "archive.tar.gz")
	if err := downloadFile(downloadURL, archivePath); err != nil {
		return &PhaseError{Phase: "download", Err: fmt.Errorf("downloading archive: %w", err)}
	}

	// 3. Verify SHA-256 checksum
	if expectedSHA256 != "" {
		actual, err := fileSHA256(archivePath)
		if err != nil {
			return &PhaseError{Phase: "checksum", Err: fmt.Errorf("computing checksum: %w", err)}
		}
		if !strings.EqualFold(actual, expectedSHA256) {
			return &PhaseError{Phase: "checksum", Err: fmt.Errorf("checksum mismatch: expected %s, got %s", expectedSHA256, actual)}
		}
		log.Printf("[update] Checksum verified")
	} else {
		log.Printf("[update] WARNING: no checksum provided, skipping verification")
	}

	// 3b. Verify ECDSA signature (supply-chain protection)
	if signature != "" && len(SigningPublicKey) > 0 {
		// Compute the hash of the archive for signature verification
		archiveHash, err := fileSHA256(archivePath)
		if err != nil {
			return &PhaseError{Phase: "signature", Err: fmt.Errorf("computing hash for signature: %w", err)}
		}
		if err := VerifySignature(archiveHash, signature, SigningPublicKey); err != nil {
			return &PhaseError{Phase: "signature", Err: err}
		}
		log.Printf("[update] Signature verified (key=%s)", signingKeyFingerprint(SigningPublicKey))
	} else if signature == "" && len(SigningPublicKey) > 0 {
		// Public key is embedded but server didn't send a signature.
		// This means the binary wasn't signed during build — reject the update.
		log.Printf("[update] WARNING: no signature provided but signing key is embedded — rejecting unsigned update")
		return &PhaseError{Phase: "signature", Err: fmt.Errorf("unsigned update rejected — signing key is embedded but no signature was provided")}
	} else {
		log.Printf("[update] WARNING: no signing key embedded, skipping signature verification")
	}

	// 4. Extract binary from tar.gz
	newBinaryPath := filepath.Join(tmpDir, "clank-agent")
	if err := extractBinary(archivePath, newBinaryPath); err != nil {
		return &PhaseError{Phase: "extract", Err: fmt.Errorf("extracting binary: %w", err)}
	}

	// 5. Make executable
	if err := os.Chmod(newBinaryPath, 0755); err != nil {
		return &PhaseError{Phase: "extract", Err: fmt.Errorf("setting permissions: %w", err)}
	}

	// 6. Atomic replace: stage next to the target (same filesystem) then rename.
	// We stage to {execPath}.new rather than using tmpDir because systemd's
	// PrivateTmp=true mounts /tmp on a separate filesystem, and os.Rename
	// requires source and dest on the same filesystem.
	stagePath := execPath + ".new"
	if err := copyFile(newBinaryPath, stagePath); err != nil {
		return &PhaseError{Phase: "replace", Err: fmt.Errorf("staging binary: %w", err)}
	}
	if err := os.Chmod(stagePath, 0755); err != nil {
		os.Remove(stagePath)
		return &PhaseError{Phase: "replace", Err: fmt.Errorf("setting stage permissions: %w", err)}
	}
	if err := os.Rename(stagePath, execPath); err != nil {
		os.Remove(stagePath)
		return &PhaseError{Phase: "replace", Err: fmt.Errorf("replacing binary: %w", err)}
	}

	log.Printf("[update] Binary replaced at %s", execPath)
	return nil
}

// BackupAndApply creates a backup of the current binary before applying
// the update. If Apply fails, the backup is automatically restored.
func BackupAndApply(downloadURL, expectedSHA256, signature, currentVersion, newVersion string) error {
	if currentVersion == newVersion {
		log.Printf("[update] Already running version %s, skipping", currentVersion)
		return nil
	}

	execPath, err := os.Executable()
	if err != nil {
		return &PhaseError{Phase: "backup", Err: fmt.Errorf("resolving executable path: %w", err)}
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return &PhaseError{Phase: "backup", Err: fmt.Errorf("resolving symlinks: %w", err)}
	}

	// Create backup
	backupPath := execPath + ".prev"
	log.Printf("[update] Backing up current binary to %s", backupPath)
	if err := copyFile(execPath, backupPath); err != nil {
		return &PhaseError{Phase: "backup", Err: fmt.Errorf("creating backup: %w", err)}
	}
	if err := os.Chmod(backupPath, 0755); err != nil {
		os.Remove(backupPath)
		return &PhaseError{Phase: "backup", Err: fmt.Errorf("setting backup permissions: %w", err)}
	}

	// Apply the update
	if err := Apply(downloadURL, expectedSHA256, signature, currentVersion, newVersion); err != nil {
		// Restore backup on failure
		log.Printf("[update] Apply failed, restoring backup: %v", err)
		if restoreErr := os.Rename(backupPath, execPath); restoreErr != nil {
			log.Printf("[update] WARNING: failed to restore backup: %v", restoreErr)
		}
		return err
	}

	return nil
}

// Rollback restores the previous binary from the .prev backup.
// Returns an error if no backup exists.
func Rollback() error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("resolving symlinks: %w", err)
	}

	backupPath := execPath + ".prev"
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		return fmt.Errorf("no backup found at %s", backupPath)
	}

	log.Printf("[update] Rolling back to previous binary from %s", backupPath)
	if err := os.Rename(backupPath, execPath); err != nil {
		return fmt.Errorf("restoring backup: %w", err)
	}

	log.Printf("[update] Rollback complete")
	return nil
}

// CleanupBackup removes the .prev backup after a successful update.
func CleanupBackup() {
	execPath, err := os.Executable()
	if err != nil {
		return
	}
	execPath, _ = filepath.EvalSymlinks(execPath)
	os.Remove(execPath + ".prev")
}

func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func extractBinary(archivePath, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// Look for the clank-agent binary (may be at root or in a subdirectory,
		// and may have a platform suffix like clank-agent-linux-amd64)
		name := filepath.Base(hdr.Name)
		if strings.HasPrefix(name, "clank-agent") && hdr.Typeflag == tar.TypeReg {
			out, err := os.Create(destPath)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
			return nil
		}
	}
	return fmt.Errorf("clank-agent binary not found in archive")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
