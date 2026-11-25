package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/docker"
	"github.com/docker/docker/pkg/stdcopy"
)

func encodeDockerLogs(stdout, stderr string) []byte {
	var buf bytes.Buffer
	if stdout != "" {
		w := stdcopy.NewStdWriter(&buf, stdcopy.Stdout)
		_, _ = w.Write([]byte(stdout))
	}
	if stderr != "" {
		w := stdcopy.NewStdWriter(&buf, stdcopy.Stderr)
		_, _ = w.Write([]byte(stderr))
	}
	return buf.Bytes()
}

type fakeDockerManager struct {
	mu sync.Mutex

	imagePresent      bool
	imageCheckErr     error
	pullErr           error
	pullWaitForCancel bool
	pullStarted       chan struct{}
	pullRelease       chan struct{}

	imageChecks    int
	pulls          int
	execCalls      int
	runBeforeImage bool
	runOpts        []docker.RunOpts
	logStdout      string
	logStderr      string

	waitFn         func(containerID string, opts docker.RunOpts) (int64, error)
	copyFromData   []byte
	copyFromErr    error
	copyFromPaths  []string
	copyToData     []byte
	copyToPaths    []string
	copyToErr      error
}

func (f *fakeDockerManager) ImageExists(ctx context.Context, image string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imageChecks++
	return f.imagePresent, f.imageCheckErr
}

func (f *fakeDockerManager) PullImage(ctx context.Context, image string, auth *docker.RegistryAuth, onLog func(string)) error {
	f.mu.Lock()
	f.pulls++
	started := f.pullStarted
	release := f.pullRelease
	waitForCancel := f.pullWaitForCancel
	pullErr := f.pullErr
	f.mu.Unlock()

	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if waitForCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
		}
	}
	if pullErr != nil {
		return pullErr
	}

	f.mu.Lock()
	f.imagePresent = true
	f.mu.Unlock()
	return nil
}

func (f *fakeDockerManager) FindContainerByLabel(context.Context, string, string) (string, string, error) {
	return "container-id", "container-name", nil
}

func (f *fakeDockerManager) ContainerExec(context.Context, string, []string) (int, string, error) {
	f.mu.Lock()
	f.execCalls++
	f.mu.Unlock()
	return 0, "", nil
}

func (f *fakeDockerManager) RunContainer(ctx context.Context, opts docker.RunOpts) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if opts.Image == alpineHelperImage && !f.imagePresent {
		f.runBeforeImage = true
	}
	f.runOpts = append(f.runOpts, opts)
	return fmt.Sprintf("container-%d", len(f.runOpts)), nil
}

func (f *fakeDockerManager) StopAndRemove(context.Context, string) error {
	return nil
}

func (f *fakeDockerManager) WaitContainer(_ context.Context, containerID string) (int64, error) {
	f.mu.Lock()
	fn := f.waitFn
	var matched docker.RunOpts
	for i, opts := range f.runOpts {
		if fmt.Sprintf("container-%d", i+1) == containerID {
			matched = opts
			break
		}
	}
	f.mu.Unlock()
	if fn != nil {
		return fn(containerID, matched)
	}
	return 0, nil
}

func (f *fakeDockerManager) CopyFromContainer(_ context.Context, _ string, srcPath string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copyFromPaths = append(f.copyFromPaths, srcPath)
	if f.copyFromErr != nil {
		return nil, f.copyFromErr
	}
	data := f.copyFromData
	if data == nil {
		data = []byte("fake-tar-archive-bytes")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeDockerManager) CopyToContainer(_ context.Context, _ string, dstPath string, content io.Reader) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copyToPaths = append(f.copyToPaths, dstPath)
	f.copyToData = data
	return f.copyToErr
}

func (f *fakeDockerManager) ContainerLogs(context.Context, string, bool, string) (io.ReadCloser, error) {
	f.mu.Lock()
	stdout := f.logStdout
	stderr := f.logStderr
	f.mu.Unlock()
	if stdout == "" && stderr == "" {
		stdout = "128\n"
	}
	return io.NopCloser(bytes.NewReader(encodeDockerLogs(stdout, stderr))), nil
}

func (f *fakeDockerManager) counts() (checks, pulls, runs int, runBeforeImage bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.imageChecks, f.pulls, len(f.runOpts), f.runBeforeImage
}

func (f *fakeDockerManager) containers() []docker.RunOpts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]docker.RunOpts(nil), f.runOpts...)
}

func newVolumeBackupCommand(backupID string) *clankv1.BackupCommand {
	return &clankv1.BackupCommand{
		CommandId:   "command-" + backupID,
		BackupId:    backupID,
		ServiceSlug: "optionidea-daily-span",
		ProjectSlug: "optionidea",
		BackupType:  "volume",
		VolumeMounts: []*clankv1.VolumeMount{
			{
				Name:      "clank-e946acc3-optionidea-daily-span-data",
				MountPath: "/app/data",
			},
		},
	}
}

func TestAlpineHelperImageAlreadyPresent(t *testing.T) {
	dm := &fakeDockerManager{imagePresent: true}
	executor := newExecutor(dm, newHelperImageCoordinator(), time.Second)

	_, err := executor.runAlpineHelper(context.Background(), docker.RunOpts{Name: "helper"})
	if err != nil {
		t.Fatalf("runAlpineHelper() error = %v", err)
	}

	checks, pulls, runs, ranBeforeImage := dm.counts()
	if checks != 1 || pulls != 0 || runs != 1 || ranBeforeImage {
		t.Fatalf("checks=%d pulls=%d runs=%d ranBeforeImage=%v", checks, pulls, runs, ranBeforeImage)
	}
}

func TestAlpineHelperImagePulledWhenAbsent(t *testing.T) {
	dm := &fakeDockerManager{}
	executor := newExecutor(dm, newHelperImageCoordinator(), time.Second)

	_, err := executor.runAlpineHelper(context.Background(), docker.RunOpts{Name: "helper"})
	if err != nil {
		t.Fatalf("runAlpineHelper() error = %v", err)
	}

	checks, pulls, runs, ranBeforeImage := dm.counts()
	if checks != 1 || pulls != 1 || runs != 1 || ranBeforeImage {
		t.Fatalf("checks=%d pulls=%d runs=%d ranBeforeImage=%v", checks, pulls, runs, ranBeforeImage)
	}
}

func TestVolumeBackupReportsHelperImagePullFailure(t *testing.T) {
	dm := &fakeDockerManager{pullErr: errors.New("registry unavailable")}
	executor := newExecutor(dm, newHelperImageCoordinator(), time.Second)

	result := executor.Execute(context.Background(), newVolumeBackupCommand("11111111-1111-1111-1111-111111111111"))
	if result.GetSuccess() {
		t.Fatal("Execute() succeeded after helper image pull failure")
	}
	if !strings.Contains(result.GetErrorMessage(), "ensuring backup helper image alpine:3.20") ||
		!strings.Contains(result.GetErrorMessage(), "registry unavailable") {
		t.Fatalf("Execute() error = %q", result.GetErrorMessage())
	}
	_, pulls, runs, _ := dm.counts()
	if pulls != 1 || runs != 0 {
		t.Fatalf("pulls=%d runs=%d, want one pull and no helper container", pulls, runs)
	}
}

func TestAlpineHelperImageRespectsCancellationAndTimeout(t *testing.T) {
	t.Run("canceled before image check", func(t *testing.T) {
		dm := &fakeDockerManager{}
		executor := newExecutor(dm, newHelperImageCoordinator(), time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := executor.runAlpineHelper(ctx, docker.RunOpts{Name: "helper"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runAlpineHelper() error = %v, want context.Canceled", err)
		}
		checks, pulls, runs, _ := dm.counts()
		if checks != 0 || pulls != 0 || runs != 0 {
			t.Fatalf("checks=%d pulls=%d runs=%d after canceled context", checks, pulls, runs)
		}
	})

	t.Run("pull timeout", func(t *testing.T) {
		dm := &fakeDockerManager{pullWaitForCancel: true}
		executor := newExecutor(dm, newHelperImageCoordinator(), 20*time.Millisecond)

		_, err := executor.runAlpineHelper(context.Background(), docker.RunOpts{Name: "helper"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("runAlpineHelper() error = %v, want context.DeadlineExceeded", err)
		}
		_, pulls, runs, _ := dm.counts()
		if pulls != 1 || runs != 0 {
			t.Fatalf("pulls=%d runs=%d after timeout", pulls, runs)
		}
	})
}

func TestConcurrentVolumeBackupsPullHelperImageOnce(t *testing.T) {
	const backupCount = 8
	dm := &fakeDockerManager{
		pullStarted: make(chan struct{}, 1),
		pullRelease: make(chan struct{}),
	}
	coordinator := newHelperImageCoordinator()
	results := make(chan *clankv1.BackupResult, backupCount)

	var wg sync.WaitGroup
	for i := 0; i < backupCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			executor := newExecutor(dm, coordinator, time.Second)
			backupID := fmt.Sprintf("%08d-1111-1111-1111-111111111111", i)
			results <- executor.Execute(context.Background(), newVolumeBackupCommand(backupID))
		}(i)
	}

	select {
	case <-dm.pullStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for helper image pull to start")
	}
	close(dm.pullRelease)
	wg.Wait()
	close(results)

	for result := range results {
		if !result.GetSuccess() {
			t.Fatalf("concurrent backup failed: %s", result.GetErrorMessage())
		}
	}
	_, pulls, _, ranBeforeImage := dm.counts()
	if pulls != 1 {
		t.Fatalf("pulls=%d, want 1", pulls)
	}
	if ranBeforeImage {
		t.Fatal("a helper container ran before the image was available")
	}
}

func TestVolumeBackupProceedsAfterHelperImagePull(t *testing.T) {
	dm := &fakeDockerManager{}
	executor := newExecutor(dm, newHelperImageCoordinator(), time.Second)

	result := executor.Execute(context.Background(), newVolumeBackupCommand("22222222-2222-2222-2222-222222222222"))
	if !result.GetSuccess() {
		t.Fatalf("Execute() failed: %s", result.GetErrorMessage())
	}
	if got := strings.Join(result.GetFiles(), ","); got != "volume_0.tar.gz,metadata.json" {
		t.Fatalf("files=%q", got)
	}

	containers := dm.containers()
	if len(containers) < 1 {
		t.Fatal("no helper container was started")
	}
	volumeHelper := containers[0]
	if volumeHelper.Image != alpineHelperImage {
		t.Fatalf("helper image=%q", volumeHelper.Image)
	}
	foundVolume := false
	for _, mount := range volumeHelper.Volumes {
		if mount.Name == "clank-e946acc3-optionidea-daily-span-data" && mount.MountPath == "/src0" {
			foundVolume = true
		}
	}
	if !foundVolume {
		t.Fatalf("volume helper mounts=%v", volumeHelper.Volumes)
	}
	_, pulls, _, ranBeforeImage := dm.counts()
	if pulls != 1 || ranBeforeImage {
		t.Fatalf("pulls=%d ranBeforeImage=%v", pulls, ranBeforeImage)
	}
	if result.GetSizeBytes() != 128 {
		t.Fatalf("size=%d, want 128 (demuxed from Docker log frame)", result.GetSizeBytes())
	}
}

func TestExecuteRejectsUnsafeIdentifiers(t *testing.T) {
	cases := []struct {
		name        string
		projectSlug string
		serviceSlug string
		backupID    string
		deleteOnly  bool
	}{
		{"traversal project", "../etc", "svc", "backup-1", false},
		{"slash in service", "proj", "a/b", "backup-1", false},
		{"shell metachar in backupID", "proj", "svc", "id;rm -rf /", false},
		{"dotdot in deleteOnly", "proj", "svc", "..", true},
		{"empty backupID", "proj", "svc", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := &fakeDockerManager{imagePresent: true}
			executor := newExecutor(dm, newHelperImageCoordinator(), time.Second)
			res := executor.Execute(context.Background(), &clankv1.BackupCommand{
				CommandId:   "cmd-1",
				ProjectSlug: tc.projectSlug,
				ServiceSlug: tc.serviceSlug,
				BackupId:    tc.backupID,
				BackupType:  "volume",
				DeleteOnly:  tc.deleteOnly,
			})
			if res.GetSuccess() {
				t.Fatalf("expected failure for unsafe identifier %+v", tc)
			}
			if _, _, runs, _ := dm.counts(); runs != 0 {
				t.Fatalf("expected 0 container runs on rejected input, got %d", runs)
			}
		})
	}
}

func TestDatabaseBackupNoDoubleDumpAndNoShellInjection(t *testing.T) {
	dm := &fakeDockerManager{imagePresent: true, logStdout: "4096\n", logStderr: "warning: ignored\n"}
	executor := newExecutor(dm, newHelperImageCoordinator(), time.Second)

	secret := "p@ss'word;rm -rf /"
	res := executor.Execute(context.Background(), &clankv1.BackupCommand{
		CommandId:     "cmd-db",
		BackupId:      "short", // < 8 chars, must not panic
		ProjectSlug:   "proj-1",
		ServiceSlug:   "db-1",
		ContainerName: "clank-db-1",
		BackupType:    "database",
		DatabaseType:  "postgres",
		EnvVars: map[string]string{
			"POSTGRES_USER":     "admin",
			"POSTGRES_DB":       "appdb",
			"POSTGRES_PASSWORD": secret,
		},
	})
	if !res.GetSuccess() {
		t.Fatalf("Execute() failed: %s", res.GetErrorMessage())
	}
	if dm.execCalls != 0 {
		t.Fatalf("ContainerExec called %d times, want 0 (no double-dump)", dm.execCalls)
	}
	if res.GetSizeBytes() != 4096 {
		t.Fatalf("SizeBytes = %d, want 4096", res.GetSizeBytes())
	}

	containers := dm.containers()
	if len(containers) < 1 {
		t.Fatal("expected dump helper container")
	}
	dumpHelper := containers[0]
	if dumpHelper.Env["PGPASSWORD"] != secret {
		t.Fatalf("expected PGPASSWORD in Env, got %q", dumpHelper.Env["PGPASSWORD"])
	}
	if strings.Contains(strings.Join(dumpHelper.Command, " "), secret) {
		t.Fatalf("secret must not appear in container command: %v", dumpHelper.Command)
	}
	if len(dumpHelper.Command) < 2 || dumpHelper.Command[1] != "--" {
		t.Fatalf("expected positional args separator '--' in Command, got %v", dumpHelper.Command)
	}
	if !strings.Contains(dumpHelper.Command[0], "pg_dump --clean --if-exists") {
		t.Fatalf("expected pg_dump --clean --if-exists in Command[0], got %q", dumpHelper.Command[0])
	}
}

func TestExecInHelperDemuxesStdcopy(t *testing.T) {
	dm := &fakeDockerManager{
		imagePresent: true,
		logStdout:    "clean-output\n",
		logStderr:    "noisy-stderr\n",
	}
	executor := newExecutor(dm, newHelperImageCoordinator(), time.Second)

	out, err := executor.execInHelper(context.Background(), `echo "$1"`, "arg1")
	if err != nil {
		t.Fatalf("execInHelper() error = %v", err)
	}
	if out != "clean-output\n" {
		t.Fatalf("execInHelper() = %q, want %q", out, "clean-output\n")
	}
}
