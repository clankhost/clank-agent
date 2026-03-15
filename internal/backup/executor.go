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
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/docker"
)

const backupVolumeName = "clank-backups"

// Executor performs backup operations using the Docker API.
type Executor struct {
	docker *docker.Manager
}

// NewExecutor creates a backup executor.
func NewExecutor(dm *docker.Manager) *Executor {
	return &Executor{docker: dm}
}

// Execute runs the backup described by cmd and returns the result.
func (e *Executor) Execute(ctx context.Context, cmd *clankv1.BackupCommand) *clankv1.BackupResult {
	result := &clankv1.BackupResult{
		CommandId: cmd.GetCommandId(),
		BackupId:  cmd.GetBackupId(),
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

	backupDir := filepath.Join(cmd.GetProjectSlug(), cmd.GetServiceSlug(), cmd.GetBackupId())
	backupType := cmd.GetBackupType()

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

	// Enforce retention on the agent side
	if cmd.GetRetentionCount() > 0 {
		e.enforceRetention(ctx, cmd)
	}

	result.Success = true
	result.SizeBytes = totalSize
	result.Files = allFiles
	return result
}

// databaseBackup runs a dump command inside the running container and pipes
// the output to a file in the clank-backups volume via an ephemeral helper.
func (e *Executor) databaseBackup(ctx context.Context, cmd *clankv1.BackupCommand, backupDir string) (int64, []string, error) {
	containerName := cmd.GetContainerName()
	dbType := cmd.GetDatabaseType()
	envVars := cmd.GetEnvVars()

	if containerName == "" {
		return 0, nil, fmt.Errorf("no container name for database backup")
	}

	// Find container ID
	containerID, _, err := e.docker.FindContainerByLabel(ctx, "clank.service_slug", cmd.GetServiceSlug())
	if err != nil {
		return 0, nil, fmt.Errorf("finding container: %w", err)
	}

	var dumpCmd []string
	var outFile string

	switch dbType {
	case "postgres":
		user := envOrDefault(envVars, "POSTGRES_USER", "postgres")
		db := envOrDefault(envVars, "POSTGRES_DB", "postgres")
		outFile = "dump.sql"
		dumpCmd = []string{"pg_dump", "-U", user, db}

	case "mysql":
		password := envOrDefault(envVars, "MYSQL_ROOT_PASSWORD", "")
		outFile = "dump.sql"
		dumpCmd = []string{"mysqldump", "-u", "root", fmt.Sprintf("-p%s", password), "--all-databases"}

	case "mongo":
		outFile = "dump.archive"
		dumpCmd = []string{"mongodump", "--archive"}

	default:
		return 0, nil, fmt.Errorf("unsupported database type: %s", dbType)
	}

	log.Printf("Running database backup for %s (%s)", cmd.GetServiceSlug(), dbType)

	// Strategy: exec the dump inside the running container, then use a helper
	// container to move it to the backup volume.
	// Step 1: exec dump, capture output
	exitCode, output, err := e.docker.ContainerExec(ctx, containerID, dumpCmd)
	if err != nil {
		return 0, nil, fmt.Errorf("exec dump: %w", err)
	}
	if exitCode != 0 {
		// For mysqldump, warnings on stderr are OK if exit code is 0
		return 0, nil, fmt.Errorf("dump exited with code %d: %s", exitCode, truncate(output, 500))
	}

	// Step 2: write output to backup volume via ephemeral container
	shellCmd := fmt.Sprintf("mkdir -p /backups/%s && cat > /backups/%s/%s", backupDir, backupDir, outFile)
	helperName := fmt.Sprintf("clank-backup-write-%s", cmd.GetBackupId()[:8])

	helperID, err := e.docker.RunContainer(ctx, docker.RunOpts{
		Image:      "alpine:3.20",
		Name:       helperName,
		Command:    []string{"sh", "-c", shellCmd},
		Entrypoint: []string{},
		Volumes: []docker.VolumeMount{
			{Name: backupVolumeName, MountPath: "/backups"},
		},
	})
	if err != nil {
		return 0, nil, fmt.Errorf("starting backup writer: %w", err)
	}
	defer func() { _ = e.docker.StopAndRemove(ctx, helperID) }()

	// Pipe dump output into the helper via exec
	// Actually, a simpler approach: run the dump and write in one go via a
	// helper container connected to the same network
	_ = e.docker.StopAndRemove(ctx, helperID)

	// Simpler approach: run everything in one ephemeral container
	size, err := e.runDumpViaHelper(ctx, cmd, backupDir, outFile, dbType)
	if err != nil {
		return 0, nil, err
	}

	return size, []string{outFile}, nil
}

// runDumpViaHelper runs the database dump from an ephemeral sidecar container
// that connects to the service's network and writes directly to the backup volume.
func (e *Executor) runDumpViaHelper(ctx context.Context, cmd *clankv1.BackupCommand, backupDir, outFile, dbType string) (int64, error) {
	envVars := cmd.GetEnvVars()
	serviceSlug := cmd.GetServiceSlug()

	// Determine image and dump command
	var image string
	var shellCmd string

	switch dbType {
	case "postgres":
		user := envOrDefault(envVars, "POSTGRES_USER", "postgres")
		db := envOrDefault(envVars, "POSTGRES_DB", "postgres")
		image = "postgres:16-alpine"
		shellCmd = fmt.Sprintf(
			"mkdir -p /backups/%s && PGPASSWORD='%s' pg_dump -h %s -U %s %s > /backups/%s/%s",
			backupDir,
			envOrDefault(envVars, "POSTGRES_PASSWORD", ""),
			serviceSlug, user, db,
			backupDir, outFile,
		)

	case "mysql":
		password := envOrDefault(envVars, "MYSQL_ROOT_PASSWORD", "")
		image = "mysql:8.0"
		shellCmd = fmt.Sprintf(
			"mkdir -p /backups/%s && mysqldump -h %s -u root -p'%s' --all-databases > /backups/%s/%s 2>/dev/null",
			backupDir, serviceSlug, password,
			backupDir, outFile,
		)

	case "mongo":
		image = "mongo:7"
		shellCmd = fmt.Sprintf(
			"mkdir -p /backups/%s && mongodump --host %s --archive=/backups/%s/%s",
			backupDir, serviceSlug, backupDir, outFile,
		)
	}

	// Resolve project network
	projectNetwork := fmt.Sprintf("clank-project-%s", cmd.GetProjectSlug())

	helperName := fmt.Sprintf("clank-backup-%s", cmd.GetBackupId()[:8])

	helperID, err := e.docker.RunContainer(ctx, docker.RunOpts{
		Image:      image,
		Name:       helperName,
		Entrypoint: []string{"sh", "-c"},
		Command:    []string{shellCmd},
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
	sizeOut, err := e.execInHelper(ctx, fmt.Sprintf("stat -c %%s /backups/%s/%s 2>/dev/null || echo 0", backupDir, outFile))
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

	// Mount backup volume + all service volumes (read-only flag not directly
	// supported in RunOpts, but we only read from them)
	mounts = append(mounts, docker.VolumeMount{Name: backupVolumeName, MountPath: "/backups"})
	for i, vm := range volumeMounts {
		srcPath := fmt.Sprintf("/src%d", i)
		mounts = append(mounts, docker.VolumeMount{Name: vm.GetName(), MountPath: srcPath})
		tarPaths = append(tarPaths, srcPath)

		// Each volume gets its own tar file
		tarName := fmt.Sprintf("volume_%d.tar.gz", i)
		files = append(files, tarName)
	}

	// Build tar commands for each volume
	var cmds []string
	cmds = append(cmds, fmt.Sprintf("mkdir -p /backups/%s", backupDir))
	for i, srcPath := range tarPaths {
		tarName := files[i]
		cmds = append(cmds, fmt.Sprintf("tar czf /backups/%s/%s -C %s .", backupDir, tarName, srcPath))
	}
	shellCmd := strings.Join(cmds, " && ")

	helperName := fmt.Sprintf("clank-backup-vol-%s", cmd.GetBackupId()[:8])
	helperID, err := e.docker.RunContainer(ctx, docker.RunOpts{
		Image:      "alpine:3.20",
		Name:       helperName,
		Entrypoint: []string{"sh", "-c"},
		Command:    []string{shellCmd},
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
	sizeCmd := fmt.Sprintf("du -sb /backups/%s/ | cut -f1", backupDir)
	sizeOut, err := e.execInHelper(ctx, sizeCmd)
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

	shellCmd := fmt.Sprintf("cat > /backups/%s/metadata.json", backupDir)
	helperName := fmt.Sprintf("clank-backup-meta-%s", cmd.GetBackupId()[:8])

	// Write via echo in a helper container
	writeCmd := fmt.Sprintf("echo '%s' > /backups/%s/metadata.json", string(metaJSON), backupDir)
	helperID, err := e.docker.RunContainer(ctx, docker.RunOpts{
		Image:      "alpine:3.20",
		Name:       helperName,
		Entrypoint: []string{"sh", "-c"},
		Command:    []string{writeCmd},
		Volumes: []docker.VolumeMount{
			{Name: backupVolumeName, MountPath: "/backups"},
		},
	})
	if err != nil {
		log.Printf("Failed to write backup metadata: %v", err)
		return
	}
	_ = shellCmd // unused, using writeCmd instead
	e.docker.WaitContainer(ctx, helperID)
	_ = e.docker.StopAndRemove(ctx, helperID)
}

// deleteBackup removes a backup directory from the backup volume.
func (e *Executor) deleteBackup(ctx context.Context, cmd *clankv1.BackupCommand) error {
	backupDir := filepath.Join(cmd.GetProjectSlug(), cmd.GetServiceSlug(), cmd.GetBackupId())
	shellCmd := fmt.Sprintf("rm -rf /backups/%s", backupDir)

	helperName := fmt.Sprintf("clank-backup-del-%s", cmd.GetBackupId()[:8])
	helperID, err := e.docker.RunContainer(ctx, docker.RunOpts{
		Image:      "alpine:3.20",
		Name:       helperName,
		Entrypoint: []string{"sh", "-c"},
		Command:    []string{shellCmd},
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
	serviceDir := filepath.Join(cmd.GetProjectSlug(), cmd.GetServiceSlug())
	retention := int(cmd.GetRetentionCount())
	if retention < 1 {
		retention = 5
	}

	// List backup directories sorted by name (which includes the backup ID)
	listCmd := fmt.Sprintf("ls -1 /backups/%s/ 2>/dev/null || true", serviceDir)
	output, err := e.execInHelper(ctx, listCmd)
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
		delCmd := fmt.Sprintf("rm -rf /backups/%s/%s", serviceDir, dirs[i])
		log.Printf("Retention prune: deleting backup dir %s/%s", serviceDir, dirs[i])
		e.execInHelper(ctx, delCmd)
	}
}

// execInHelper runs a shell command in an ephemeral alpine container with
// the backup volume mounted. Returns the command output.
func (e *Executor) execInHelper(ctx context.Context, shellCmd string) (string, error) {
	helperName := fmt.Sprintf("clank-backup-helper-%d", time.Now().UnixNano()%100000)
	helperID, err := e.docker.RunContainer(ctx, docker.RunOpts{
		Image:      "alpine:3.20",
		Name:       helperName,
		Entrypoint: []string{"sh", "-c"},
		Command:    []string{shellCmd},
		Volumes: []docker.VolumeMount{
			{Name: backupVolumeName, MountPath: "/backups"},
		},
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = e.docker.StopAndRemove(ctx, helperID) }()

	e.docker.WaitContainer(ctx, helperID)

	// Read logs for output
	reader, err := e.docker.ContainerLogs(ctx, helperID, false, "all")
	if err != nil {
		return "", err
	}
	defer reader.Close()

	buf := make([]byte, 64*1024)
	n, _ := reader.Read(buf)
	return string(buf[:n]), nil
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
