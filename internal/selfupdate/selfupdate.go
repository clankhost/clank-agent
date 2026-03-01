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

// Apply downloads the new agent binary, verifies its checksum, and replaces
// the current binary atomically. Returns nil on success — the caller should
// exit to let systemd restart with the new binary.
func Apply(downloadURL, expectedSHA256, currentVersion, newVersion string) error {
	if currentVersion == newVersion {
		log.Printf("[update] Already running version %s, skipping", currentVersion)
		return nil
	}

	log.Printf("[update] Updating from %s to %s", currentVersion, newVersion)

	// 1. Determine current binary path
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("resolving symlinks: %w", err)
	}

	// 2. Download archive to temp dir
	log.Printf("[update] Downloading %s", downloadURL)
	tmpDir, err := os.MkdirTemp("", "clank-update-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	archivePath := filepath.Join(tmpDir, "archive.tar.gz")
	if err := downloadFile(downloadURL, archivePath); err != nil {
		return fmt.Errorf("downloading archive: %w", err)
	}

	// 3. Verify SHA-256 checksum
	if expectedSHA256 != "" {
		actual, err := fileSHA256(archivePath)
		if err != nil {
			return fmt.Errorf("computing checksum: %w", err)
		}
		if !strings.EqualFold(actual, expectedSHA256) {
			return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedSHA256, actual)
		}
		log.Printf("[update] Checksum verified")
	} else {
		log.Printf("[update] WARNING: no checksum provided, skipping verification")
	}

	// 4. Extract binary from tar.gz
	newBinaryPath := filepath.Join(tmpDir, "clank-agent")
	if err := extractBinary(archivePath, newBinaryPath); err != nil {
		return fmt.Errorf("extracting binary: %w", err)
	}

	// 5. Make executable
	if err := os.Chmod(newBinaryPath, 0755); err != nil {
		return fmt.Errorf("setting permissions: %w", err)
	}

	// 6. Atomic replace: stage next to the target (same filesystem) then rename.
	// We stage to {execPath}.new rather than using tmpDir because systemd's
	// PrivateTmp=true mounts /tmp on a separate filesystem, and os.Rename
	// requires source and dest on the same filesystem.
	stagePath := execPath + ".new"
	if err := copyFile(newBinaryPath, stagePath); err != nil {
		return fmt.Errorf("staging binary: %w", err)
	}
	if err := os.Chmod(stagePath, 0755); err != nil {
		os.Remove(stagePath)
		return fmt.Errorf("setting stage permissions: %w", err)
	}
	if err := os.Rename(stagePath, execPath); err != nil {
		os.Remove(stagePath)
		return fmt.Errorf("replacing binary: %w", err)
	}

	log.Printf("[update] Binary replaced at %s", execPath)
	return nil
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
