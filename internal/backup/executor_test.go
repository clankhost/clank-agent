package backup

import (
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
)

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
	runBeforeImage bool
	runOpts        []docker.RunOpts
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

func (f *fakeDockerManager) WaitContainer(context.Context, string) (int64, error) {
	return 0, nil
}

func (f *fakeDockerManager) ContainerLogs(context.Context, string, bool, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("128\n")), nil
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
}
