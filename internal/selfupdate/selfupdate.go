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
	"strconv"
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

// parseSemver splits a "X.Y.Z" version string into (major, minor, patch).
// Returns (0,0,0) if parsing fails. Strips a leading "v" if present.
func parseSemver(v string) (int, int, int) {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return 0, 0, 0
	}
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	// Strip anything after a hyphen (e.g., "1.2.3-rc1" → "3")
	patchStr := strings.SplitN(parts[2], "-", 2)[0]
	patch, _ := strconv.Atoi(patchStr)
	return major, minor, patch
}

// isDowngrade returns true if newVersion is strictly less than currentVersion.
func isDowngrade(currentVersion, newVersion string) bool {
	cMaj, cMin, cPatch := parseSemver(currentVersion)
	nMaj, nMin, nPatch := parseSemver(newVersion)
	if nMaj != cMaj {
		return nMaj < cMaj
	}
	if nMin != cMin {
		return nMin < cMin
	}
	return nPatch < cPatch
}

// Apply downloads the new agent binary, verifies its signature and checksum,
// and replaces the current binary. Returns nil on success — the caller should
// exit to let systemd restart with the new binary.
//
// The configDir (e.g. /etc/clank-agent) is used for staging the new binary
// before overwriting the target. This avoids creating new files in the install
// directory (/usr/local/bin) which may not be writable by the agent user.
//
// If signature is non-empty and a signing public key is embedded, the archive's
// ECDSA P-256 signature is verified before proceeding. This prevents supply-chain
// attacks where a compromised control plane serves a malicious binary with
// a matching checksum.
func Apply(downloadURL, expectedSHA256, signature, currentVersion, newVersion, configDir string) error {
	if currentVersion == newVersion {
		log.Printf("[update] Already running version %s, skipping", currentVersion)
		return nil
	}

	// Reject downgrades — defense-in-depth against compromised control plane
	// or accidental VERSION file rollback.
	if isDowngrade(currentVersion, newVersion) {
		log.Printf("[update] Rejecting downgrade from %s to %s", currentVersion, newVersion)
		return &PhaseError{
			Phase: "version",
			Err:   fmt.Errorf("downgrade rejected: %s → %s (current > requested)", currentVersion, newVersion),
		}
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

	// 6. Replace the binary by overwriting in-place.
	//
	// We cannot use the classic stage-then-rename approach because the agent
	// runs as the "clank" user which owns the binary file but does NOT have
	// write permission on the install directory (/usr/local/bin/ is root:root).
	// Creating new files (.new) or renaming within that directory requires
	// directory write permission.
	//
	// Instead, we overwrite the existing binary's contents directly. The agent
	// user can do this because it owns the file. This is non-atomic (a crash
	// mid-write could corrupt the binary), but the backup in configDir and
	// systemd restart provide recovery.
	log.Printf("[update] Overwriting binary at %s", execPath)
	if err := overwriteFile(newBinaryPath, execPath); err != nil {
		return &PhaseError{Phase: "replace", Err: fmt.Errorf("overwriting binary: %w", err)}
	}

	log.Printf("[update] Binary replaced at %s", execPath)
	return nil
}

// BackupAndApply creates a backup of the current binary before applying
// the update. If Apply fails, the backup is automatically restored.
//
// The backup is stored in configDir (e.g. /etc/clank-agent/) which the
// agent user owns, avoiding permission issues with /usr/local/bin/.
func BackupAndApply(downloadURL, expectedSHA256, signature, currentVersion, newVersion, configDir string) error {
	if currentVersion == newVersion {
		log.Printf("[update] Already running version %s, skipping", currentVersion)
		return nil
	}

	// Reject downgrades before even creating a backup
	if isDowngrade(currentVersion, newVersion) {
		log.Printf("[update] Rejecting downgrade from %s to %s", currentVersion, newVersion)
		return &PhaseError{
			Phase: "version",
			Err:   fmt.Errorf("downgrade rejected: %s → %s (current > requested)", currentVersion, newVersion),
		}
	}

	execPath, err := os.Executable()
	if err != nil {
		return &PhaseError{Phase: "backup", Err: fmt.Errorf("resolving executable path: %w", err)}
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return &PhaseError{Phase: "backup", Err: fmt.Errorf("resolving symlinks: %w", err)}
	}

	// Create backup in the config directory (owned by clank user)
	backupPath := filepath.Join(configDir, "clank-agent.prev")
	log.Printf("[update] Backing up current binary to %s", backupPath)
	if err := copyFile(execPath, backupPath); err != nil {
		return &PhaseError{Phase: "backup", Err: fmt.Errorf("creating backup: %w", err)}
	}
	if err := os.Chmod(backupPath, 0755); err != nil {
		os.Remove(backupPath)
		return &PhaseError{Phase: "backup", Err: fmt.Errorf("setting backup permissions: %w", err)}
	}

	// Apply the update
	if err := Apply(downloadURL, expectedSHA256, signature, currentVersion, newVersion, configDir); err != nil {
		// Restore backup on failure by overwriting the binary with the backup
		log.Printf("[update] Apply failed, restoring backup: %v", err)
		if restoreErr := overwriteFile(backupPath, execPath); restoreErr != nil {
			log.Printf("[update] WARNING: failed to restore backup: %v", restoreErr)
		}
		return err
	}

	return nil
}

// Rollback restores the previous binary from the backup in configDir.
// Returns an error if no backup exists.
func Rollback(configDir string) error {
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("resolving symlinks: %w", err)
	}

	backupPath := filepath.Join(configDir, "clank-agent.prev")
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		return fmt.Errorf("no backup found at %s", backupPath)
	}

	log.Printf("[update] Rolling back to previous binary from %s", backupPath)
	if err := overwriteFile(backupPath, execPath); err != nil {
		return fmt.Errorf("restoring backup: %w", err)
	}

	log.Printf("[update] Rollback complete")
	return nil
}

// CleanupBackup removes the .prev backup after a successful update.
func CleanupBackup(configDir string) {
	backupPath := filepath.Join(configDir, "clank-agent.prev")
	os.Remove(backupPath)
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

// copyFile creates dst (or truncates if it exists) and copies src into it.
// Used for creating files in directories the agent owns (e.g. configDir).
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

// overwriteFile replaces the contents of an existing file without creating
// a new directory entry. This works even when the agent user owns the file
// but doesn't have write permission on the parent directory.
func overwriteFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
