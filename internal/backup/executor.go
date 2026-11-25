// Package backup implements service data backup operations.
//
// Backups are stored in a "clank-backups" Docker volume, organised as:
//
//	<project-slug>/<service-slug>/<backup-id>/
//
// Database dumps use docker exec on the running container. Volume backups
// use an ephemeral alpine container with the service volumes mounted read-only.
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/docker"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	backupVolumeName        = "clank-backups"
	alpineHelperImage       = "alpine:3.20"
	helperImagePullTimeout  = 2 * time.Minute
	maxHelperLogBytes       = 1 << 20 // 1 MiB
	envOffsiteUploadURL     = "__CLANK_OFFSITE_UPLOAD_URL"
	envOffsiteDownloadURL   = "__CLANK_OFFSITE_DOWNLOAD_URL"
	offsiteTransferTimeout  = 15 * time.Minute
	maxOffsiteDownloadBytes = 20 << 30 // 20 GiB
	offsiteUploadedMarker   = "offsite_uploaded"
	restoreCompletedMarker  = "restore_completed"
)

var safeBackupSegmentRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func validateBackupSegment(field, value string) error {
	if value == "" {
		return fmt.Errorf("invalid %s: must not be empty", field)
	}
	if value == "." || value == ".." || strings.Contains(value, "..") || strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("invalid %s %q: path traversal not allowed", field, value)
	}
	if !safeBackupSegmentRe.MatchString(value) {
		return fmt.Errorf("invalid %s %q: contains disallowed characters", field, value)
	}
	return nil
}

func validateBackupCommandIDs(cmd *clankv1.BackupCommand) error {
	if err := validateBackupSegment("project_slug", cmd.GetProjectSlug()); err != nil {
		return err
	}
	if err := validateBackupSegment("service_slug", cmd.GetServiceSlug()); err != nil {
		return err
	}
	if err := validateBackupSegment("backup_id", cmd.GetBackupId()); err != nil {
		return err
	}
	return nil
}

func shortID(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}

type dockerManager interface {
	ImageExists(context.Context, string) (bool, error)
	PullImage(context.Context, string, *docker.RegistryAuth, func(string)) error
	FindContainerByLabel(context.Context, string, string) (string, string, error)
	ContainerExec(context.Context, string, []string) (int, string, error)
	RunContainer(context.Context, docker.RunOpts) (string, error)
	StopAndRemove(context.Context, string) error
	WaitContainer(context.Context, string) (int64, error)
	ContainerLogs(context.Context, string, bool, string) (io.ReadCloser, error)
	CopyFromContainer(context.Context, string, string) (io.ReadCloser, error)
	CopyToContainer(context.Context, string, string, io.Reader) error
}

type helperImageCoordinator struct {
	lock chan struct{}
}

func newHelperImageCoordinator() *helperImageCoordinator {
	lock := make(chan struct{}, 1)
	lock <- struct{}{}
	return &helperImageCoordinator{lock: lock}
}

var sharedHelperImageCoordinator = newHelperImageCoordinator()

// Executor performs backup operations using the Docker API.
type Executor struct {
	docker             dockerManager
	helperImage        *helperImageCoordinator
	helperImageTimeout time.Duration
	httpClient         *http.Client
	urlValidator       func(string) error
	maxDownloadBytes   int64
}

// NewExecutor creates a backup executor.
func NewExecutor(dm *docker.Manager) *Executor {
	return newExecutor(dm, sharedHelperImageCoordinator, helperImagePullTimeout)
}

func newExecutor(dm dockerManager, coordinator *helperImageCoordinator, timeout time.Duration) *Executor {
	return &Executor{
		docker:             dm,
		helperImage:        coordinator,
		helperImageTimeout: timeout,
		httpClient:         &http.Client{},
		urlValidator:       validateHTTPSOffsiteURL,
		maxDownloadBytes:   maxOffsiteDownloadBytes,
	}
}

func (c *helperImageCoordinator) ensure(ctx context.Context, timeout time.Duration, dm dockerManager) error {
	ensureCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ensureCtx.Err(); err != nil {
		return fmt.Errorf("waiting for image check: %w", err)
	}

	select {
	case <-ensureCtx.Done():
		return fmt.Errorf("waiting for image check: %w", ensureCtx.Err())
	case <-c.lock:
	}
	defer func() { c.lock <- struct{}{} }()

	exists, err := dm.ImageExists(ensureCtx, alpineHelperImage)
	if err != nil {
		return fmt.Errorf("checking local image: %w", err)
	}
	if exists {
		return nil
	}

	if err := dm.PullImage(ensureCtx, alpineHelperImage, nil, func(string) {}); err != nil {
		return fmt.Errorf("pull failed: %w", err)
	}
	return nil
}

func (e *Executor) runAlpineHelper(ctx context.Context, opts docker.RunOpts) (string, error) {
	if err := e.helperImage.ensure(ctx, e.helperImageTimeout, e.docker); err != nil {
		return "", fmt.Errorf("ensuring backup helper image %s: %w", alpineHelperImage, err)
	}
	opts.Image = alpineHelperImage
	return e.docker.RunContainer(ctx, opts)
}

// Execute runs the backup described by cmd and returns the result.
func (e *Executor) Execute(ctx context.Context, cmd *clankv1.BackupCommand) *clankv1.BackupResult {
	result := &clankv1.BackupResult{
		CommandId: cmd.GetCommandId(),
		BackupId:  cmd.GetBackupId(),
	}

	if err := validateBackupCommandIDs(cmd); err != nil {
		result.ErrorMessage = err.Error()
		return result
	}

	// Handle delete-only cleanup
	if cmd.GetDeleteOnly() {
		err := e.deleteBackup(ctx, cmd)
		if err != nil {
			result.ErrorMessage = err.Error()
		} else {
			result.Success = true
		}
		return result
	}

	backupDir := path.Join(cmd.GetProjectSlug(), cmd.GetServiceSlug(), cmd.GetBackupId())
	backupType := cmd.GetBackupType()

	// Handle restore operations
	if strings.HasPrefix(backupType, "restore:") {
		return e.executeRestore(ctx, cmd, backupDir, backupType)
	}

	var totalSize int64
	var allFiles []string

	switch backupType {
	case "database":
		size, files, err := e.databaseBackup(ctx, cmd, backupDir)
		if err != nil {
			result.ErrorMessage = fmt.Sprintf("database backup failed: %v", err)
			return result
		}
		totalSize = size
		allFiles = files

	case "volume":
		size, files, err := e.volumeBackup(ctx, cmd, backupDir)
		if err != nil {
			result.ErrorMessage = fmt.Sprintf("volume backup failed: %v", err)
			return result
		}
		totalSize = size
		allFiles = files

	case "all":
		dbSize, dbFiles, err := e.databaseBackup(ctx, cmd, backupDir)
		if err != nil {
			result.ErrorMessage = fmt.Sprintf("database backup failed: %v", err)
			return result
		}
		volSize, volFiles, err := e.volumeBackup(ctx, cmd, backupDir)
		if err != nil {
			result.ErrorMessage = fmt.Sprintf("volume backup failed: %v", err)
			return result
		}
		totalSize = dbSize + volSize
		allFiles = append(dbFiles, volFiles...)

	default:
		result.ErrorMessage = fmt.Sprintf("unknown backup type: %s", backupType)
		return result
	}

	// Write metadata
	e.writeMetadata(ctx, cmd, backupDir, totalSize, allFiles)
	allFiles = append(allFiles, "metadata.json")

	// Best-effort offsite upload when presigned URL is provided
	if uploadURL := strings.TrimSpace(cmd.GetEnvVars()[envOffsiteUploadURL]); uploadURL != "" {
		if err := e.uploadOffsite(ctx, cmd, backupDir, uploadURL); err != nil {
			log.Printf("Offsite upload failed for backup %s: %v", cmd.GetBackupId(), err)
		} else {
			allFiles = append(allFiles, offsiteUploadedMarker)
		}
	}

	// Enforce retention on the agent side
	if cmd.GetRetentionCount() > 0 {
		e.enforceRetention(ctx, cmd)
	}

	result.Success = true
	result.SizeBytes = totalSize
	result.Files = allFiles
	return result
}

// databaseBackup runs a dump from an ephemeral sidecar container connected to
// the service's network and writes directly to the clank-backups volume.
func (e *Executor) databaseBackup(ctx context.Context, cmd *clankv1.BackupCommand, backupDir string) (int64, []string, error) {
	containerName := cmd.GetContainerName()
	dbType := cmd.GetDatabaseType()

	if containerName == "" {
		return 0, nil, fmt.Errorf("no container name for database backup")
	}

	var outFile string
	switch dbType {
	case "postgres", "mysql":
		outFile = "dump.sql"
	case "mongo":
		outFile = "dump.archive"
	default:
		return 0, nil, fmt.Errorf("unsupported database type: %s", dbType)
	}

	log.Printf("Running database backup for %s (%s)", cmd.GetServiceSlug(), dbType)

	size, err := e.runDumpViaHelper(ctx, cmd, backupDir, outFile, dbType)
	if err != nil {
		return 0, nil, err
	}

	return size, []string{outFile}, nil
}

// runDumpViaHelper runs the database dump from an ephemeral sidecar container
// that connects to the service's network and writes directly to the backup volume.
func (e *Executor) runDumpViaHelper(ctx context.Context, cmd *clankv1.BackupCommand, backupDir, outFile, dbType string) (int64, error) {
	envVars := sanitizeDBEnvVars(cmd.GetEnvVars())
	serviceSlug := cmd.GetServiceSlug()
	backupRoot := path.Join("/backups", backupDir)
	outPath := path.Join(backupRoot, outFile)

	// Determine image, env, and dump command (no shell interpolation of untrusted inputs)
	var image string
	var command []string
	var env map[string]string

	switch dbType {
	case "postgres":
		user := envOrDefault(envVars, "POSTGRES_USER", "postgres")
		db := envOrDefault(envVars, "POSTGRES_DB", "postgres")
		image = "postgres:16-alpine"
		env = map[string]string{
			"PGPASSWORD": envOrDefault(envVars, "POSTGRES_PASSWORD", ""),
		}
		command = []string{
			`mkdir -p -- "$1" && pg_dump --clean --if-exists -h "$2" -U "$3" -- "$4" > "$5"`,
			"--",
			backupRoot, serviceSlug, user, db, outPath,
		}

	case "mysql":
		image = "mysql:8.0"
		env = map[string]string{
			"MYSQL_PWD": envOrDefault(envVars, "MYSQL_ROOT_PASSWORD", ""),
		}
		command = []string{
			`mkdir -p -- "$1" && mysqldump -h "$2" -u root --all-databases > "$3" 2>/dev/null`,
			"--",
			backupRoot, serviceSlug, outPath,
		}

	case "mongo":
		image = "mongo:7"
		command = []string{
			`mkdir -p -- "$1" && mongodump --host "$2" --archive="$3"`,
			"--",
			backupRoot, serviceSlug, outPath,
		}

	default:
		return 0, fmt.Errorf("unsupported database type: %s", dbType)
	}

	// Resolve project network
	projectNetwork := fmt.Sprintf("clank-project-%s", cmd.GetProjectSlug())
	helperName := fmt.Sprintf("clank-backup-%s", shortID(cmd.GetBackupId(), 8))

	helperID, err := e.docker.RunContainer(ctx, docker.RunOpts{
		Image:      image,
		Name:       helperName,
		Env:        env,
		Entrypoint: []string{"sh", "-c"},
		Command:    command,
		Network:    projectNetwork,
		Volumes: []docker.VolumeMount{
			{Name: backupVolumeName, MountPath: "/backups"},
		},
	})
	if err != nil {
		return 0, fmt.Errorf("starting dump helper: %w", err)
	}
	defer func() { _ = e.docker.StopAndRemove(ctx, helperID) }()

	// Wait for completion
	exitCode, err := e.docker.WaitContainer(ctx, helperID)
	if err != nil {
		return 0, fmt.Errorf("waiting for dump: %w", err)
	}
	if exitCode != 0 {
		return 0, fmt.Errorf("dump helper exited with code %d", exitCode)
	}

	// Get size via a quick check
	sizeOut, err := e.execInHelper(ctx, `stat -c %s -- "$1" 2>/dev/null || echo 0`, outPath)
	if err != nil {
		return 0, nil // size unknown but backup succeeded
	}
	var size int64
	fmt.Sscanf(strings.TrimSpace(sizeOut), "%d", &size)

	return size, nil
}

// volumeBackup tars the service's mounted volumes into the backup directory.
func (e *Executor) volumeBackup(ctx context.Context, cmd *clankv1.BackupCommand, backupDir string) (int64, []string, error) {
	volumeMounts := cmd.GetVolumeMounts()
	if len(volumeMounts) == 0 {
		return 0, nil, nil // nothing to back up
	}

	log.Printf("Running volume backup for %s (%d volumes)", cmd.GetServiceSlug(), len(volumeMounts))

	var mounts []docker.VolumeMount
	var tarPaths []string
	var files []string

	// Mount backup volume + all service volumes
	mounts = append(mounts, docker.VolumeMount{Name: backupVolumeName, MountPath: "/backups"})
	for i, vm := range volumeMounts {
		srcPath := fmt.Sprintf("/src%d", i)
		mounts = append(mounts, docker.VolumeMount{Name: vm.GetName(), MountPath: srcPath})
		tarPaths = append(tarPaths, srcPath)

		// Each volume gets its own tar file
		tarName := fmt.Sprintf("volume_%d.tar.gz", i)
		files = append(files, tarName)
	}

	backupRoot := path.Join("/backups", backupDir)
	script := `set -e
backup_dir="$1"
shift
mkdir -p -- "$backup_dir"
i=0
for src in "$@"; do
  tar czf "$backup_dir/volume_${i}.tar.gz" -C "$src" .
  i=$((i + 1))
done`

	command := make([]string, 0, 3+len(tarPaths))
	command = append(command, script, "--", backupRoot)
	command = append(command, tarPaths...)

	helperName := fmt.Sprintf("clank-backup-vol-%s", shortID(cmd.GetBackupId(), 8))
	helperID, err := e.runAlpineHelper(ctx, docker.RunOpts{
		Name:       helperName,
		Entrypoint: []string{"sh", "-c"},
		Command:    command,
		Volumes:    mounts,
	})
	if err != nil {
		return 0, nil, fmt.Errorf("starting volume backup helper: %w", err)
	}
	defer func() { _ = e.docker.StopAndRemove(ctx, helperID) }()

	exitCode, err := e.docker.WaitContainer(ctx, helperID)
	if err != nil {
		return 0, nil, fmt.Errorf("waiting for volume backup: %w", err)
	}
	if exitCode != 0 {
		return 0, nil, fmt.Errorf("volume backup helper exited with code %d", exitCode)
	}

	// Get total size
	sizeOut, err := e.execInHelper(ctx, `du -sb -- "$1" | cut -f1`, backupRoot+"/")
	var totalSize int64
	if err == nil {
		fmt.Sscanf(strings.TrimSpace(sizeOut), "%d", &totalSize)
	}

	return totalSize, files, nil
}

// writeMetadata writes a metadata.json file to the backup directory.
func (e *Executor) writeMetadata(ctx context.Context, cmd *clankv1.BackupCommand, backupDir string, size int64, files []string) {
	meta := map[string]interface{}{
		"backup_id":     cmd.GetBackupId(),
		"service_slug":  cmd.GetServiceSlug(),
		"project_slug":  cmd.GetProjectSlug(),
		"backup_type":   cmd.GetBackupType(),
		"database_type": cmd.GetDatabaseType(),
		"size_bytes":    size,
		"files":         files,
		"created_at":    time.Now().UTC().Format(time.RFC3339),
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		log.Printf("Failed to marshal backup metadata: %v", err)
		return
	}

	backupRoot := path.Join("/backups", backupDir)
	helperName := fmt.Sprintf("clank-backup-meta-%s", shortID(cmd.GetBackupId(), 8))

	helperID, err := e.runAlpineHelper(ctx, docker.RunOpts{
		Name:       helperName,
		Env:        map[string]string{"CLANK_BACKUP_META": string(metaJSON)},
		Entrypoint: []string{"sh", "-c"},
		Command:    []string{`mkdir -p -- "$1" && printf '%s\n' "$CLANK_BACKUP_META" > "$1/metadata.json"`, "--", backupRoot},
		Volumes: []docker.VolumeMount{
			{Name: backupVolumeName, MountPath: "/backups"},
		},
	})
	if err != nil {
		log.Printf("Failed to write backup metadata: %v", err)
		return
	}
	defer func() { _ = e.docker.StopAndRemove(ctx, helperID) }()
	_, _ = e.docker.WaitContainer(ctx, helperID)
}

// deleteBackup removes a backup directory from the backup volume.
func (e *Executor) deleteBackup(ctx context.Context, cmd *clankv1.BackupCommand) error {
	if err := validateBackupCommandIDs(cmd); err != nil {
		return err
	}
	backupRoot := path.Join("/backups", cmd.GetProjectSlug(), cmd.GetServiceSlug(), cmd.GetBackupId())

	helperName := fmt.Sprintf("clank-backup-del-%s", shortID(cmd.GetBackupId(), 8))
	helperID, err := e.runAlpineHelper(ctx, docker.RunOpts{
		Name:       helperName,
		Entrypoint: []string{"sh", "-c"},
		Command:    []string{`rm -rf -- "$1"`, "--", backupRoot},
		Volumes: []docker.VolumeMount{
			{Name: backupVolumeName, MountPath: "/backups"},
		},
	})
	if err != nil {
		return fmt.Errorf("starting delete helper: %w", err)
	}
	defer func() { _ = e.docker.StopAndRemove(ctx, helperID) }()

	exitCode, err := e.docker.WaitContainer(ctx, helperID)
	if err != nil {
		return fmt.Errorf("waiting for delete: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("delete helper exited with code %d", exitCode)
	}
	log.Printf("Deleted backup %s/%s/%s", cmd.GetProjectSlug(), cmd.GetServiceSlug(), cmd.GetBackupId())
	return nil
}

// enforceRetention prunes old backups beyond the retention count.
func (e *Executor) enforceRetention(ctx context.Context, cmd *clankv1.BackupCommand) {
	serviceDir := path.Join(cmd.GetProjectSlug(), cmd.GetServiceSlug())
	serviceRoot := path.Join("/backups", serviceDir)
	retention := int(cmd.GetRetentionCount())
	if retention < 1 {
		retention = 5
	}

	// List backup directories sorted by name (which includes the backup ID)
	output, err := e.execInHelper(ctx, `ls -1 -- "$1" 2>/dev/null || true`, serviceRoot)
	if err != nil {
		return
	}

	dirs := strings.Fields(strings.TrimSpace(output))
	if len(dirs) <= retention {
		return
	}

	// Sort by creation time via metadata.json if available, else by name
	sort.Strings(dirs)

	// Delete the oldest ones
	toDelete := len(dirs) - retention
	for i := 0; i < toDelete; i++ {
		if err := validateBackupSegment("retention_dir", dirs[i]); err != nil {
			log.Printf("Retention prune: skipping unsafe directory entry %q: %v", dirs[i], err)
			continue
		}
		targetPath := path.Join(serviceRoot, dirs[i])
		log.Printf("Retention prune: deleting backup dir %s/%s", serviceDir, dirs[i])
		_, _ = e.execInHelper(ctx, `rm -rf -- "$1"`, targetPath)
	}
}

// execInHelper runs a shell command in an ephemeral alpine container with
// the backup volume mounted. Returns the demuxed stdout output.
func (e *Executor) execInHelper(ctx context.Context, shellCmd string, args ...string) (string, error) {
	helperName := fmt.Sprintf("clank-backup-helper-%d", time.Now().UnixNano()%100000)
	command := make([]string, 0, 2+len(args))
	command = append(command, shellCmd, "--")
	command = append(command, args...)

	helperID, err := e.runAlpineHelper(ctx, docker.RunOpts{
		Name:       helperName,
		Entrypoint: []string{"sh", "-c"},
		Command:    command,
		Volumes: []docker.VolumeMount{
			{Name: backupVolumeName, MountPath: "/backups"},
		},
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = e.docker.StopAndRemove(ctx, helperID) }()

	_, _ = e.docker.WaitContainer(ctx, helperID)

	// Read and demux Docker multiplexed stdout/stderr logs
	reader, err := e.docker.ContainerLogs(ctx, helperID, false, "all")
	if err != nil {
		return "", err
	}
	defer reader.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, io.LimitReader(reader, maxHelperLogBytes)); err != nil {
		return "", fmt.Errorf("demuxing helper logs: %w", err)
	}
	return stdout.String(), nil
}

// envOrDefault returns the value for key from the map, or the fallback.
func envOrDefault(m map[string]string, key, fallback string) string {
	if v, ok := m[key]; ok && v != "" {
		return v
	}
	return fallback
}

// truncate shortens a string to maxLen chars.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
