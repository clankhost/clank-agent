package backup

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/docker"
)

const maxOffsiteErrorBodyBytes = 4096

var errOffsiteDownloadTooLarge = errors.New("offsite backup exceeds maximum allowed size")

// validateHTTPSOffsiteURL enforces that offsite transfer URLs use https://.
func validateHTTPSOffsiteURL(rawURL string) error {
	return validateOffsiteURL(rawURL, false)
}

// validateOffsiteURL validates an offsite presigned URL.
// When allowLoopbackHTTP is true (tests only), http://127.0.0.1, http://localhost,
// and http://[::1] are permitted in addition to https://.
func validateOffsiteURL(rawURL string, allowLoopbackHTTP bool) error {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return fmt.Errorf("offsite URL must not be empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("invalid offsite URL: %w", err)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid offsite URL: missing host")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if allowLoopbackHTTP {
			host := strings.ToLower(u.Hostname())
			if host == "127.0.0.1" || host == "localhost" || host == "::1" {
				return nil
			}
		}
		return fmt.Errorf("offsite URL must use https scheme")
	default:
		return fmt.Errorf("offsite URL must use https scheme, got %q", u.Scheme)
	}
}

// redactURL returns scheme://host/path without query parameters or credentials for safe logging.
func redactURL(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "<invalid-url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// sanitizeDBEnvVars returns a copy of envVars with all internal __CLANK_* keys removed
// so control-plane metadata (such as presigned offsite URLs) can never leak into helper containers.
func sanitizeDBEnvVars(envVars map[string]string) map[string]string {
	if len(envVars) == 0 {
		return nil
	}
	clean := make(map[string]string, len(envVars))
	for k, v := range envVars {
		if strings.HasPrefix(k, "__CLANK_") {
			continue
		}
		clean[k] = v
	}
	return clean
}

// uploadOffsite archives /backups/<backupDir> as a .tar.gz stream and uploads it via HTTP PUT.
func (e *Executor) uploadOffsite(ctx context.Context, cmd *clankv1.BackupCommand, backupDir, uploadURL string) error {
	if err := e.urlValidator(uploadURL); err != nil {
		return fmt.Errorf("invalid upload URL: %w", err)
	}

	uploadCtx, cancel := context.WithTimeout(ctx, offsiteTransferTimeout)
	defer cancel()

	backupRoot := path.Join("/backups", backupDir)
	helperName := fmt.Sprintf("clank-backup-offsite-up-%s", shortID(cmd.GetBackupId(), 8))

	helperID, err := e.runAlpineHelper(uploadCtx, docker.RunOpts{
		Name:       helperName,
		Entrypoint: []string{"sh", "-c"},
		Command:    []string{`test -d "$1"`, "--", backupRoot},
		Volumes: []docker.VolumeMount{
			{Name: backupVolumeName, MountPath: "/backups", ReadOnly: true},
		},
	})
	if err != nil {
		return fmt.Errorf("starting offsite archive helper: %w", err)
	}
	defer func() { _ = e.docker.StopAndRemove(ctx, helperID) }()

	exitCode, err := e.docker.WaitContainer(uploadCtx, helperID)
	if err != nil {
		return fmt.Errorf("waiting for offsite archive helper: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("backup directory %s not found for offsite upload (exit %d)", backupRoot, exitCode)
	}

	tarReader, err := e.docker.CopyFromContainer(uploadCtx, helperID, backupRoot+"/.")
	if err != nil {
		return fmt.Errorf("reading backup archive from container: %w", err)
	}
	defer tarReader.Close()

	// Stream tar -> gzip -> temp file so we do not buffer large backups in RAM
	// and can send an exact Content-Length header required by S3/R2 presigned PUTs.
	tmpFile, err := os.CreateTemp("", "clank-offsite-upload-*.tar.gz")
	if err != nil {
		return fmt.Errorf("creating temp file for offsite upload: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}()

	gzw := gzip.NewWriter(tmpFile)
	if _, err := io.Copy(gzw, tarReader); err != nil {
		_ = gzw.Close()
		return fmt.Errorf("compressing backup archive: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return fmt.Errorf("finalizing gzip archive: %w", err)
	}

	info, err := tmpFile.Stat()
	if err != nil {
		return fmt.Errorf("stat temp upload archive: %w", err)
	}
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seeking temp upload archive: %w", err)
	}

	req, err := http.NewRequestWithContext(uploadCtx, http.MethodPut, uploadURL, tmpFile)
	if err != nil {
		return fmt.Errorf("creating offsite PUT request: %w", err)
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.ContentLength = info.Size()

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("uploading archive to %s: %w", redactURL(uploadURL), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxOffsiteErrorBodyBytes))
		return fmt.Errorf("offsite upload to %s returned HTTP %d: %s", redactURL(uploadURL), resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	return nil
}

type maxBytesReader struct {
	r        io.Reader
	limit    int64
	read     int64
	exceeded bool
}

func (m *maxBytesReader) Read(p []byte) (int, error) {
	if m.exceeded {
		return 0, errOffsiteDownloadTooLarge
	}
	remaining := m.limit - m.read
	if remaining <= 0 {
		// Probe one extra byte to distinguish exact-limit from over-limit.
		var probe [1]byte
		n, err := m.r.Read(probe[:])
		if n > 0 {
			m.exceeded = true
			return 0, errOffsiteDownloadTooLarge
		}
		if err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := m.r.Read(p)
	m.read += int64(n)
	return n, err
}

// downloadOffsite fetches a .tar.gz backup archive via HTTP GET and extracts it into
// /backups/<backupDir> in the clank-backups volume.
func (e *Executor) downloadOffsite(ctx context.Context, cmd *clankv1.BackupCommand, backupDir, downloadURL string) error {
	if err := e.urlValidator(downloadURL); err != nil {
		return fmt.Errorf("invalid download URL: %w", err)
	}

	downloadCtx, cancel := context.WithTimeout(ctx, offsiteTransferTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("creating offsite GET request: %w", err)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading archive from %s: %w", redactURL(downloadURL), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxOffsiteErrorBodyBytes))
		return fmt.Errorf("offsite download from %s returned HTTP %d: %s", redactURL(downloadURL), resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	limit := e.maxDownloadBytes
	if limit <= 0 {
		limit = maxOffsiteDownloadBytes
	}
	if resp.ContentLength > limit {
		return fmt.Errorf("%w (%d bytes > %d byte limit)", errOffsiteDownloadTooLarge, resp.ContentLength, limit)
	}

	limited := &maxBytesReader{r: resp.Body, limit: limit}
	gzReader, err := gzip.NewReader(limited)
	if err != nil {
		return fmt.Errorf("opening gzip stream from %s: %w", redactURL(downloadURL), err)
	}
	defer gzReader.Close()

	backupRoot := path.Join("/backups", backupDir)
	helperName := fmt.Sprintf("clank-restore-dl-%s", shortID(cmd.GetBackupId(), 8))

	helperID, err := e.runAlpineHelper(downloadCtx, docker.RunOpts{
		Name:       helperName,
		Entrypoint: []string{"sh", "-c"},
		Command:    []string{`mkdir -p -- "$1"`, "--", backupRoot},
		Volumes: []docker.VolumeMount{
			{Name: backupVolumeName, MountPath: "/backups"},
		},
	})
	if err != nil {
		return fmt.Errorf("starting offsite download helper: %w", err)
	}
	defer func() { _ = e.docker.StopAndRemove(ctx, helperID) }()

	exitCode, err := e.docker.WaitContainer(downloadCtx, helperID)
	if err != nil {
		return fmt.Errorf("waiting for download helper: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("download helper exited with code %d", exitCode)
	}

	if err := e.docker.CopyToContainer(downloadCtx, helperID, backupRoot, gzReader); err != nil {
		return fmt.Errorf("extracting downloaded backup archive: %w", err)
	}
	// Drain any trailing gzip bytes to verify the gzip checksum and size limit.
	if _, err := io.Copy(io.Discard, gzReader); err != nil {
		return fmt.Errorf("verifying downloaded gzip stream: %w", err)
	}
	if err := gzReader.Close(); err != nil {
		return fmt.Errorf("closing downloaded gzip stream: %w", err)
	}

	return nil
}
