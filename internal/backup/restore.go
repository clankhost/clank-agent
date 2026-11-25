package backup

import (
	"context"
	"fmt"
	"log"
	"path"
	"strings"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/docker"
)

// executeRestore handles restore:database, restore:volume, and restore:all commands.
func (e *Executor) executeRestore(ctx context.Context, cmd *clankv1.BackupCommand, backupDir, backupType string) *clankv1.BackupResult {
	result := &clankv1.BackupResult{
		CommandId: cmd.GetCommandId(),
		BackupId:  cmd.GetBackupId(),
	}

	switch backupType {
	case "restore:database", "restore:volume", "restore:all":
	default:
		result.ErrorMessage = fmt.Sprintf("unknown restore type: %s", backupType)
		return result
	}

	exists, err := e.backupDirExists(ctx, cmd, backupDir)
	if err != nil {
		result.ErrorMessage = fmt.Sprintf("checking local backup directory: %v", err)
		return result
	}

	if !exists {
		downloadURL := strings.TrimSpace(cmd.GetEnvVars()[envOffsiteDownloadURL])
		if downloadURL == "" {
			result.ErrorMessage = fmt.Sprintf("local backup directory /backups/%s not found and no offsite download URL provided", backupDir)
			return result
		}
		log.Printf("Local backup %s missing; downloading from offsite storage (%s)", cmd.GetBackupId(), redactURL(downloadURL))
		if err := e.downloadOffsite(ctx, cmd, backupDir, downloadURL); err != nil {
			result.ErrorMessage = fmt.Sprintf("offsite backup download failed: %v", err)
			return result
		}
	}

	switch backupType {
	case "restore:database":
		if err := e.restoreDatabase(ctx, cmd, backupDir); err != nil {
			result.ErrorMessage = fmt.Sprintf("database restore failed: %v", err)
			return result
		}
	case "restore:volume":
		if err := e.restoreVolumes(ctx, cmd, backupDir); err != nil {
			result.ErrorMessage = fmt.Sprintf("volume restore failed: %v", err)
			return result
		}
	case "restore:all":
		if err := e.restoreDatabase(ctx, cmd, backupDir); err != nil {
			result.ErrorMessage = fmt.Sprintf("database restore failed: %v", err)
			return result
		}
		if err := e.restoreVolumes(ctx, cmd, backupDir); err != nil {
			result.ErrorMessage = fmt.Sprintf("volume restore failed: %v", err)
			return result
		}
	}

	result.Success = true
	result.Files = []string{restoreCompletedMarker}
	return result
}

// backupDirExists checks whether /backups/<backupDir> exists and is non-empty in clank-backups.
func (e *Executor) backupDirExists(ctx context.Context, cmd *clankv1.BackupCommand, backupDir string) (bool, error) {
	backupRoot := path.Join("/backups", backupDir)
	helperName := fmt.Sprintf("clank-restore-check-%s", shortID(cmd.GetBackupId(), 8))

	helperID, err := e.runAlpineHelper(ctx, docker.RunOpts{
		Name:       helperName,
		Entrypoint: []string{"sh", "-c"},
		Command:    []string{`test -d "$1" && [ -n "$(ls -A -- "$1" 2>/dev/null)" ]`, "--", backupRoot},
		Volumes: []docker.VolumeMount{
			{Name: backupVolumeName, MountPath: "/backups", ReadOnly: true},
		},
	})
	if err != nil {
		return false, fmt.Errorf("starting backup check helper: %w", err)
	}
	defer func() { _ = e.docker.StopAndRemove(ctx, helperID) }()

	exitCode, err := e.docker.WaitContainer(ctx, helperID)
	if err != nil {
		return false, fmt.Errorf("waiting for backup check helper: %w", err)
	}
	return exitCode == 0, nil
}

// restoreDatabase restores a database dump from /backups/<backupDir> into the target service container.
func (e *Executor) restoreDatabase(ctx context.Context, cmd *clankv1.BackupCommand, backupDir string) error {
	dbType := cmd.GetDatabaseType()
	envVars := sanitizeDBEnvVars(cmd.GetEnvVars())
	serviceSlug := cmd.GetServiceSlug()
	backupRoot := path.Join("/backups", backupDir)

	var image string
	var command []string
	var env map[string]string

	switch dbType {
	case "postgres":
		user := envOrDefault(envVars, "POSTGRES_USER", "postgres")
		db := envOrDefault(envVars, "POSTGRES_DB", "postgres")
		dumpPath := path.Join(backupRoot, "dump.sql")
		image = "postgres:16-alpine"
		env = map[string]string{
			"PGPASSWORD": envOrDefault(envVars, "POSTGRES_PASSWORD", ""),
		}
		command = []string{
			`psql -h "$1" -U "$2" -d "$3" -v ON_ERROR_STOP=1 -c "DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;" && psql -h "$1" -U "$2" -d "$3" -v ON_ERROR_STOP=1 < "$4"`,
			"--",
			serviceSlug, user, db, dumpPath,
		}

	case "mysql":
		dumpPath := path.Join(backupRoot, "dump.sql")
		image = "mysql:8.0"
		env = map[string]string{
			"MYSQL_PWD": envOrDefault(envVars, "MYSQL_ROOT_PASSWORD", ""),
		}
		command = []string{
			`mysql -h "$1" -u root < "$2"`,
			"--",
			serviceSlug, dumpPath,
		}

	case "mongo":
		dumpPath := path.Join(backupRoot, "dump.archive")
		image = "mongo:7"
		command = []string{
			`mongorestore --host "$1" --archive="$2" --drop`,
			"--",
			serviceSlug, dumpPath,
		}

	default:
		return fmt.Errorf("unsupported database type: %s", dbType)
	}

	log.Printf("Running database restore for %s (%s)", serviceSlug, dbType)

	projectNetwork := fmt.Sprintf("clank-project-%s", cmd.GetProjectSlug())
	helperName := fmt.Sprintf("clank-restore-db-%s", shortID(cmd.GetBackupId(), 8))

	helperID, err := e.docker.RunContainer(ctx, docker.RunOpts{
		Image:      image,
		Name:       helperName,
		Env:        env,
		Entrypoint: []string{"sh", "-c"},
		Command:    command,
		Network:    projectNetwork,
		Volumes: []docker.VolumeMount{
			{Name: backupVolumeName, MountPath: "/backups", ReadOnly: true},
		},
	})
	if err != nil {
		return fmt.Errorf("starting database restore helper: %w", err)
	}
	defer func() { _ = e.docker.StopAndRemove(ctx, helperID) }()

	exitCode, err := e.docker.WaitContainer(ctx, helperID)
	if err != nil {
		return fmt.Errorf("waiting for database restore: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("database restore helper exited with code %d", exitCode)
	}
	return nil
}

// restoreVolumes extracts each backed-up volume archive into its corresponding Docker volume.
func (e *Executor) restoreVolumes(ctx context.Context, cmd *clankv1.BackupCommand, backupDir string) error {
	volumeMounts := cmd.GetVolumeMounts()
	if len(volumeMounts) == 0 {
		return nil
	}

	backupRoot := path.Join("/backups", backupDir)
	log.Printf("Running volume restore for %s (%d volumes)", cmd.GetServiceSlug(), len(volumeMounts))

	for i, vm := range volumeMounts {
		volName := vm.GetName()
		if err := validateBackupSegment("volume_name", volName); err != nil {
			return err
		}

		archivePath := path.Join(backupRoot, volName+".tar.gz")
		fallbackPath := path.Join(backupRoot, fmt.Sprintf("volume_%d.tar.gz", i))
		helperName := fmt.Sprintf("clank-restore-vol-%s-%d", shortID(cmd.GetBackupId(), 8), i)

		helperID, err := e.runAlpineHelper(ctx, docker.RunOpts{
			Name:       helperName,
			Entrypoint: []string{"sh", "-c"},
			Command: []string{
				`if [ -f "$1" ]; then tar -xzf "$1" -C /target; elif [ -f "$2" ]; then tar -xzf "$2" -C /target; else echo "volume archive not found: $1" >&2; exit 1; fi`,
				"--",
				archivePath,
				fallbackPath,
			},
			Volumes: []docker.VolumeMount{
				{Name: volName, MountPath: "/target", ReadOnly: false},
				{Name: backupVolumeName, MountPath: "/backups", ReadOnly: true},
			},
		})
		if err != nil {
			return fmt.Errorf("starting volume restore helper for %s: %w", volName, err)
		}

		exitCode, waitErr := e.docker.WaitContainer(ctx, helperID)
		_ = e.docker.StopAndRemove(ctx, helperID)
		if waitErr != nil {
			return fmt.Errorf("waiting for volume restore of %s: %w", volName, waitErr)
		}
		if exitCode != 0 {
			return fmt.Errorf("volume restore helper for %s exited with code %d", volName, exitCode)
		}
	}

	return nil
}
