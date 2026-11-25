package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/docker"
)

func buildTestTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar WriteHeader(%s): %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar Write(%s): %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		t.Fatalf("gzip Write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

func allowLoopbackValidator(rawURL string) error {
	return validateOffsiteURL(rawURL, true)
}

func TestOffsiteUploadSuccessAddsMarkerAndStreamsValidGzip(t *testing.T) {
	tarPayload := buildTestTar(t, map[string]string{
		"dump.sql":      "SELECT 1;\n",
		"metadata.json": `{"backup_id":"b-1"}`,
	})

	var gotMethod string
	var gotContentType string
	var gotContentLength int64
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotContentLength = r.ContentLength
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		gotBody = body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dm := &fakeDockerManager{
		imagePresent: true,
		logStdout:    "512\n",
		copyFromData: tarPayload,
	}
	exec := newExecutor(dm, newHelperImageCoordinator(), time.Second)
	exec.httpClient = srv.Client()
	exec.urlValidator = allowLoopbackValidator

	uploadURL := srv.URL + "/backups/proj/db/b-1.tar.gz?X-Amz-Signature=secret"
	res := exec.Execute(context.Background(), &clankv1.BackupCommand{
		CommandId:     "cmd-up-1",
		BackupId:      "b-1",
		ProjectSlug:   "proj",
		ServiceSlug:   "db",
		ContainerName: "clank-db",
		BackupType:    "database",
		DatabaseType:  "postgres",
		EnvVars: map[string]string{
			"POSTGRES_USER":            "app",
			"POSTGRES_DB":              "appdb",
			"POSTGRES_PASSWORD":        "pw123",
			envOffsiteUploadURL:        uploadURL,
			"__CLANK_INTERNAL_SECRET":  "must-not-leak",
		},
	})

	if !res.GetSuccess() {
		t.Fatalf("Execute() failed: %s", res.GetErrorMessage())
	}
	if !slices.Contains(res.GetFiles(), offsiteUploadedMarker) {
		t.Fatalf("expected %q in files, got %v", offsiteUploadedMarker, res.GetFiles())
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("HTTP method = %q, want PUT", gotMethod)
	}
	if gotContentType != "application/gzip" {
		t.Fatalf("Content-Type = %q, want application/gzip", gotContentType)
	}
	if gotContentLength <= 0 || gotContentLength != int64(len(gotBody)) {
		t.Fatalf("ContentLength = %d, body len = %d", gotContentLength, len(gotBody))
	}

	// Verify uploaded body is valid gzip wrapping the original tar archive
	gzr, err := gzip.NewReader(bytes.NewReader(gotBody))
	if err != nil {
		t.Fatalf("gzip.NewReader on uploaded body failed: %v", err)
	}
	defer gzr.Close()
	unzipped, err := io.ReadAll(gzr)
	if err != nil {
		t.Fatalf("reading gunzipped body: %v", err)
	}
	if !bytes.Equal(unzipped, tarPayload) {
		t.Fatalf("gunzipped payload mismatch: got %d bytes, want %d bytes", len(unzipped), len(tarPayload))
	}

	// Verify CopyFromContainer path
	if len(dm.copyFromPaths) != 1 || dm.copyFromPaths[0] != "/backups/proj/db/b-1/." {
		t.Fatalf("copyFromPaths = %v, want [/backups/proj/db/b-1/.]", dm.copyFromPaths)
	}

	// Verify __CLANK_* keys never leaked into any container Env
	for _, c := range dm.containers() {
		for k := range c.Env {
			if strings.HasPrefix(k, "__CLANK_") {
				t.Fatalf("internal key %q leaked into container %s Env", k, c.Name)
			}
		}
	}
}

func TestOffsiteUpload500KeepsLocalBackupSuccessfulWithoutMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("storage temporarily unavailable"))
	}))
	defer srv.Close()

	dm := &fakeDockerManager{
		imagePresent: true,
		copyFromData: buildTestTar(t, map[string]string{"metadata.json": "{}"}),
	}
	exec := newExecutor(dm, newHelperImageCoordinator(), time.Second)
	exec.httpClient = srv.Client()
	exec.urlValidator = allowLoopbackValidator

	cmd := newVolumeBackupCommand("33333333-3333-3333-3333-333333333333")
	cmd.EnvVars = map[string]string{
		envOffsiteUploadURL: srv.URL + "/upload.tar.gz",
	}

	res := exec.Execute(context.Background(), cmd)
	if !res.GetSuccess() {
		t.Fatalf("local backup should remain successful on offsite 500, got error: %s", res.GetErrorMessage())
	}
	if slices.Contains(res.GetFiles(), offsiteUploadedMarker) {
		t.Fatalf("offsite_uploaded marker must not be present after HTTP 500, got files=%v", res.GetFiles())
	}
	if !slices.Contains(res.GetFiles(), "metadata.json") {
		t.Fatalf("expected metadata.json in files, got %v", res.GetFiles())
	}
}

func TestOffsiteUploadRejectsNonHTTPSURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"plain http remote", "http://s3.example.com/bucket/backup.tar.gz"},
		{"plain http loopback with prod validator", "http://127.0.0.1:9000/bucket/backup.tar.gz"},
		{"file scheme", "file:///etc/passwd"},
		{"ftp scheme", "ftp://example.com/backup.tar.gz"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := &fakeDockerManager{imagePresent: true}
			// Uses default production validateHTTPSOffsiteURL
			exec := newExecutor(dm, newHelperImageCoordinator(), time.Second)

			cmd := newVolumeBackupCommand("44444444-4444-4444-4444-444444444444")
			cmd.EnvVars = map[string]string{
				envOffsiteUploadURL: tc.url,
			}

			res := exec.Execute(context.Background(), cmd)
			if !res.GetSuccess() {
				t.Fatalf("local backup should still succeed when offsite URL is rejected: %s", res.GetErrorMessage())
			}
			if slices.Contains(res.GetFiles(), offsiteUploadedMarker) {
				t.Fatalf("offsite_uploaded must not be set for rejected URL %q", tc.url)
			}
			if len(dm.copyFromPaths) != 0 {
				t.Fatalf("expected no CopyFromContainer call on rejected URL, got %v", dm.copyFromPaths)
			}
		})
	}
}

func TestRestoreDatabasePostgresMySQLMongo(t *testing.T) {
	cases := []struct {
		name            string
		dbType          string
		envVars         map[string]string
		wantImage       string
		wantEnvKey      string
		wantEnvVal      string
		wantCmdContains string
		wantArgs        []string
	}{
		{
			name:   "postgres",
			dbType: "postgres",
			envVars: map[string]string{
				"POSTGRES_USER":     "pguser",
				"POSTGRES_DB":       "pgdb",
				"POSTGRES_PASSWORD": "pg'secret;echo",
				"__CLANK_INTERNAL":  "no-leak",
			},
			wantImage:       "postgres:16-alpine",
			wantEnvKey:      "PGPASSWORD",
			wantEnvVal:      "pg'secret;echo",
			wantCmdContains: `[ -r "$4" ] || exit 1; { printf 'DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public; GRANT ALL ON SCHEMA public TO PUBLIC;\n'; cat "$4"; } | psql -1 -v ON_ERROR_STOP=1 -h "$1" -U "$2" -d "$3"`,
			wantArgs:        []string{"mydb", "pguser", "pgdb", "/backups/myproj/mydb/bkp-1/dump.sql"},
		},
		{
			name:   "mysql",
			dbType: "mysql",
			envVars: map[string]string{
				"MYSQL_ROOT_PASSWORD": "root'pw",
			},
			wantImage:       "mysql:8.0",
			wantEnvKey:      "MYSQL_PWD",
			wantEnvVal:      "root'pw",
			wantCmdContains: `mysql -h "$1" -u root < "$2"`,
			wantArgs:        []string{"mydb", "/backups/myproj/mydb/bkp-1/dump.sql"},
		},
		{
			name:            "mongo",
			dbType:          "mongo",
			envVars:         map[string]string{},
			wantImage:       "mongo:7",
			wantCmdContains: `mongorestore --host "$1" --archive="$2" --drop`,
			wantArgs:        []string{"mydb", "/backups/myproj/mydb/bkp-1/dump.archive"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := &fakeDockerManager{imagePresent: true}
			exec := newExecutor(dm, newHelperImageCoordinator(), time.Second)

			res := exec.Execute(context.Background(), &clankv1.BackupCommand{
				CommandId:    "cmd-restore-" + tc.dbType,
				BackupId:     "bkp-1",
				ProjectSlug:  "myproj",
				ServiceSlug:  "mydb",
				BackupType:   "restore:database",
				DatabaseType: tc.dbType,
				EnvVars:      tc.envVars,
			})

			if !res.GetSuccess() {
				t.Fatalf("Execute() failed: %s", res.GetErrorMessage())
			}
			if !slices.Equal(res.GetFiles(), []string{restoreCompletedMarker}) {
				t.Fatalf("Files = %v, want [%s]", res.GetFiles(), restoreCompletedMarker)
			}

			containers := dm.containers()
			// containers[0] = local backup check helper, containers[1] = db restore helper
			if len(containers) != 2 {
				t.Fatalf("expected 2 containers (check + restore), got %d", len(containers))
			}
			restoreContainer := containers[1]
			if restoreContainer.Image != tc.wantImage {
				t.Fatalf("image = %q, want %q", restoreContainer.Image, tc.wantImage)
			}
			if restoreContainer.Network != "clank-project-myproj" {
				t.Fatalf("network = %q, want clank-project-myproj", restoreContainer.Network)
			}
			if tc.wantEnvKey != "" && restoreContainer.Env[tc.wantEnvKey] != tc.wantEnvVal {
				t.Fatalf("env[%s] = %q, want %q", tc.wantEnvKey, restoreContainer.Env[tc.wantEnvKey], tc.wantEnvVal)
			}
			for k := range restoreContainer.Env {
				if strings.HasPrefix(k, "__CLANK_") {
					t.Fatalf("internal key %q leaked into restore container", k)
				}
			}
			if len(restoreContainer.Command) < 2 || restoreContainer.Command[1] != "--" {
				t.Fatalf("expected '--' positional separator in Command, got %v", restoreContainer.Command)
			}
			if restoreContainer.Command[0] != tc.wantCmdContains {
				t.Fatalf("Command[0] = %q, want %q", restoreContainer.Command[0], tc.wantCmdContains)
			}
			gotArgs := restoreContainer.Command[2:]
			if !slices.Equal(gotArgs, tc.wantArgs) {
				t.Fatalf("positional args = %v, want %v", gotArgs, tc.wantArgs)
			}
			// clank-backups must be mounted read-only
			if len(restoreContainer.Volumes) != 1 || restoreContainer.Volumes[0].Name != backupVolumeName || !restoreContainer.Volumes[0].ReadOnly {
				t.Fatalf("expected clank-backups mounted read-only, got %+v", restoreContainer.Volumes)
			}
		})
	}
}

func TestRestoreDatabaseErrorHandling(t *testing.T) {
	t.Run("unsupported database type", func(t *testing.T) {
		dm := &fakeDockerManager{imagePresent: true}
		exec := newExecutor(dm, newHelperImageCoordinator(), time.Second)

		res := exec.Execute(context.Background(), &clankv1.BackupCommand{
			CommandId:    "cmd-bad-db",
			BackupId:     "bkp-1",
			ProjectSlug:  "proj",
			ServiceSlug:  "svc",
			BackupType:   "restore:database",
			DatabaseType: "sqlite",
		})
		if res.GetSuccess() {
			t.Fatal("expected failure for unsupported database type")
		}
		if !strings.Contains(res.GetErrorMessage(), "unsupported database type") {
			t.Fatalf("unexpected error: %q", res.GetErrorMessage())
		}
	})

	t.Run("non-zero exit code", func(t *testing.T) {
		dm := &fakeDockerManager{
			imagePresent: true,
			waitFn: func(_ string, opts docker.RunOpts) (int64, error) {
				if strings.HasPrefix(opts.Name, "clank-restore-db-") {
					return 2, nil
				}
				return 0, nil
			},
		}
		exec := newExecutor(dm, newHelperImageCoordinator(), time.Second)

		res := exec.Execute(context.Background(), &clankv1.BackupCommand{
			CommandId:    "cmd-fail-db",
			BackupId:     "bkp-1",
			ProjectSlug:  "proj",
			ServiceSlug:  "svc",
			BackupType:   "restore:database",
			DatabaseType: "postgres",
		})
		if res.GetSuccess() {
			t.Fatal("expected failure when restore helper exits non-zero")
		}
		if !strings.Contains(res.GetErrorMessage(), "exited with code 2") {
			t.Fatalf("unexpected error: %q", res.GetErrorMessage())
		}
	})
}

func TestRestoreVolumePositionalArgsAndTraversalRejection(t *testing.T) {
	t.Run("valid volume restore", func(t *testing.T) {
		dm := &fakeDockerManager{imagePresent: true}
		exec := newExecutor(dm, newHelperImageCoordinator(), time.Second)

		res := exec.Execute(context.Background(), &clankv1.BackupCommand{
			CommandId:   "cmd-rvol",
			BackupId:    "bkp-vol-1",
			ProjectSlug: "proj",
			ServiceSlug: "svc",
			BackupType:  "restore:volume",
			VolumeMounts: []*clankv1.VolumeMount{
				{Name: "clank-proj-svc-data", MountPath: "/app/data"},
			},
		})
		if !res.GetSuccess() {
			t.Fatalf("Execute() failed: %s", res.GetErrorMessage())
		}
		if !slices.Equal(res.GetFiles(), []string{restoreCompletedMarker}) {
			t.Fatalf("Files = %v, want [%s]", res.GetFiles(), restoreCompletedMarker)
		}

		containers := dm.containers()
		if len(containers) != 2 {
			t.Fatalf("expected 2 containers (check + volume restore), got %d", len(containers))
		}
		volContainer := containers[1]
		if volContainer.Image != alpineHelperImage {
			t.Fatalf("image = %q, want %q", volContainer.Image, alpineHelperImage)
		}
		if len(volContainer.Command) < 4 || volContainer.Command[1] != "--" {
			t.Fatalf("expected positional args with '--', got %v", volContainer.Command)
		}
		if !strings.Contains(volContainer.Command[0], `tar -xzf "$1" -C /target`) {
			t.Fatalf("unexpected command script: %q", volContainer.Command[0])
		}
		if volContainer.Command[2] != "/backups/proj/svc/bkp-vol-1/clank-proj-svc-data.tar.gz" {
			t.Fatalf("$1 = %q", volContainer.Command[2])
		}
		// Check mounts: target RW and clank-backups RO
		var sawTargetRW, sawBackupsRO bool
		for _, m := range volContainer.Volumes {
			if m.Name == "clank-proj-svc-data" && m.MountPath == "/target" && !m.ReadOnly {
				sawTargetRW = true
			}
			if m.Name == backupVolumeName && m.MountPath == "/backups" && m.ReadOnly {
				sawBackupsRO = true
			}
		}
		if !sawTargetRW || !sawBackupsRO {
			t.Fatalf("unexpected volume mounts: %+v", volContainer.Volumes)
		}
	})

	t.Run("traversal volume name rejected", func(t *testing.T) {
		badNames := []string{"../etc", "a/b", "vol;rm -rf /", "..", ""}
		for _, bad := range badNames {
			dm := &fakeDockerManager{imagePresent: true}
			exec := newExecutor(dm, newHelperImageCoordinator(), time.Second)

			res := exec.Execute(context.Background(), &clankv1.BackupCommand{
				CommandId:   "cmd-bad-vol",
				BackupId:    "bkp-vol-1",
				ProjectSlug: "proj",
				ServiceSlug: "svc",
				BackupType:  "restore:volume",
				VolumeMounts: []*clankv1.VolumeMount{
					{Name: bad, MountPath: "/app/data"},
				},
			})
			if res.GetSuccess() {
				t.Fatalf("expected failure for unsafe volume name %q", bad)
			}
			// Only the local check container should have run, never a volume restore helper.
			for _, c := range dm.containers() {
				if strings.HasPrefix(c.Name, "clank-restore-vol-") {
					t.Fatalf("volume restore helper must not run for unsafe volume name %q", bad)
				}
			}
		}
	})
}

func TestRestoreOffsiteDownloadFallback(t *testing.T) {
	tarPayload := buildTestTar(t, map[string]string{
		"dump.sql":      "CREATE TABLE t(id int);\n",
		"metadata.json": `{"backup_id":"bkp-offsite"}`,
	})
	gzPayload := gzipBytes(t, tarPayload)

	t.Run("downloads and extracts when local dir missing", func(t *testing.T) {
		var gotMethod string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			w.Header().Set("Content-Type", "application/gzip")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(gzPayload)
		}))
		defer srv.Close()

		dm := &fakeDockerManager{
			imagePresent: true,
			waitFn: func(_ string, opts docker.RunOpts) (int64, error) {
				if strings.HasPrefix(opts.Name, "clank-restore-check-") {
					return 1, nil // Simulate missing local backup directory
				}
				return 0, nil
			},
		}
		exec := newExecutor(dm, newHelperImageCoordinator(), time.Second)
		exec.httpClient = srv.Client()
		exec.urlValidator = allowLoopbackValidator

		res := exec.Execute(context.Background(), &clankv1.BackupCommand{
			CommandId:    "cmd-restore-offsite",
			BackupId:     "bkp-offsite",
			ProjectSlug:  "proj",
			ServiceSlug:  "db",
			BackupType:   "restore:all",
			DatabaseType: "postgres",
			VolumeMounts: []*clankv1.VolumeMount{
				{Name: "clank-proj-db-data", MountPath: "/var/lib/postgresql/data"},
			},
			EnvVars: map[string]string{
				"POSTGRES_USER":       "postgres",
				"POSTGRES_DB":         "app",
				"POSTGRES_PASSWORD":   "secret",
				envOffsiteDownloadURL: srv.URL + "/backup.tar.gz?token=abc",
			},
		})
		if !res.GetSuccess() {
			t.Fatalf("Execute() failed: %s", res.GetErrorMessage())
		}
		if !slices.Equal(res.GetFiles(), []string{restoreCompletedMarker}) {
			t.Fatalf("Files = %v, want [%s]", res.GetFiles(), restoreCompletedMarker)
		}
		if gotMethod != http.MethodGet {
			t.Fatalf("HTTP method = %q, want GET", gotMethod)
		}
		if len(dm.copyToPaths) != 1 || dm.copyToPaths[0] != "/backups/proj/db/bkp-offsite" {
			t.Fatalf("copyToPaths = %v", dm.copyToPaths)
		}
		if !bytes.Equal(dm.copyToData, tarPayload) {
			t.Fatalf("extracted tar mismatch: got %d bytes, want %d bytes", len(dm.copyToData), len(tarPayload))
		}
	})

	t.Run("fails when local dir missing and no download URL", func(t *testing.T) {
		dm := &fakeDockerManager{
			imagePresent: true,
			waitFn: func(_ string, opts docker.RunOpts) (int64, error) {
				if strings.HasPrefix(opts.Name, "clank-restore-check-") {
					return 1, nil
				}
				return 0, nil
			},
		}
		exec := newExecutor(dm, newHelperImageCoordinator(), time.Second)

		res := exec.Execute(context.Background(), &clankv1.BackupCommand{
			CommandId:    "cmd-restore-missing",
			BackupId:     "bkp-missing",
			ProjectSlug:  "proj",
			ServiceSlug:  "db",
			BackupType:   "restore:database",
			DatabaseType: "postgres",
		})
		if res.GetSuccess() {
			t.Fatal("expected failure when local backup is missing and no offsite URL is set")
		}
		if !strings.Contains(res.GetErrorMessage(), "not found") {
			t.Fatalf("unexpected error: %q", res.GetErrorMessage())
		}
	})

	t.Run("fails when offsite download exceeds max size", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(gzPayload)
		}))
		defer srv.Close()

		dm := &fakeDockerManager{
			imagePresent: true,
			waitFn: func(_ string, opts docker.RunOpts) (int64, error) {
				if strings.HasPrefix(opts.Name, "clank-restore-check-") {
					return 1, nil
				}
				return 0, nil
			},
		}
		exec := newExecutor(dm, newHelperImageCoordinator(), time.Second)
		exec.httpClient = srv.Client()
		exec.urlValidator = allowLoopbackValidator
		exec.maxDownloadBytes = 10 // smaller than gzPayload

		res := exec.Execute(context.Background(), &clankv1.BackupCommand{
			CommandId:    "cmd-restore-toobig",
			BackupId:     "bkp-big",
			ProjectSlug:  "proj",
			ServiceSlug:  "db",
			BackupType:   "restore:database",
			DatabaseType: "postgres",
			EnvVars: map[string]string{
				envOffsiteDownloadURL: srv.URL + "/huge.tar.gz",
			},
		})
		if res.GetSuccess() {
			t.Fatal("expected failure when download exceeds maxDownloadBytes")
		}
		if !strings.Contains(res.GetErrorMessage(), "exceeds maximum allowed size") {
			t.Fatalf("unexpected error: %q", res.GetErrorMessage())
		}
	})
}
