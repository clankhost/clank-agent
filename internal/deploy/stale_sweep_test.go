package deploy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/clankhost/clank-agent/internal/docker"
)

type fakeDeployDocker struct {
	mu                sync.Mutex
	containers        map[string]docker.ContainerRef
	labels            map[string]map[string]string
	stoppedIDs        []string
	stoppedBeforeRun  []string
	runCalled         bool
	newContainerID    string
	containerIP       string
}

func (f *fakeDeployDocker) EnsureNetwork(ctx context.Context, name string) error { return nil }

func (f *fakeDeployDocker) FindContainerByLabel(ctx context.Context, key, value string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, c := range f.containers {
		if c.State == "running" && f.labels[id][key] == value {
			return c.ID, c.Name, nil
		}
	}
	return "", "", nil
}

func (f *fakeDeployDocker) FindAllContainersByLabel(ctx context.Context, key, value string) ([]docker.ContainerRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []docker.ContainerRef
	for id, c := range f.containers {
		if f.labels[id][key] == value {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeDeployDocker) FindContainerByName(ctx context.Context, name string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.Name == name {
			return c.ID, c.Name, nil
		}
	}
	return "", "", nil
}

func (f *fakeDeployDocker) FindTraefikContainer(ctx context.Context) string { return "" }

func (f *fakeDeployDocker) ConnectToNetworkIfNeeded(ctx context.Context, containerID, networkName string) error {
	return nil
}

func (f *fakeDeployDocker) PullImage(ctx context.Context, img string, auth *docker.RegistryAuth, onLog func(string)) error {
	return nil
}

func (f *fakeDeployDocker) InspectImage(ctx context.Context, imageRef string) (*docker.ImageMeta, error) {
	return &docker.ImageMeta{}, nil
}

func (f *fakeDeployDocker) GetImageDigest(ctx context.Context, imageRef string) (string, error) {
	return "sha256:testdigest", nil
}

func (f *fakeDeployDocker) EnsureVolumeOwnership(ctx context.Context, imageRef string, volumes []docker.VolumeMount) error {
	return nil
}

func (f *fakeDeployDocker) RunContainer(ctx context.Context, opts docker.RunOpts) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCalled = true
	id := f.newContainerID
	f.containers[id] = docker.ContainerRef{
		ID:    id,
		Name:  opts.Name,
		State: "running",
	}
	lbls := make(map[string]string, len(opts.Labels))
	for k, v := range opts.Labels {
		lbls[k] = v
	}
	f.labels[id] = lbls
	return id, nil
}

func (f *fakeDeployDocker) InspectContainer(ctx context.Context, containerID string) (*docker.ContainerInspection, error) {
	return &docker.ContainerInspection{
		State: "running",
		IP:    f.containerIP,
	}, nil
}

func (f *fakeDeployDocker) GetContainerIP(ctx context.Context, containerID, networkName string) (string, error) {
	return f.containerIP, nil
}

func (f *fakeDeployDocker) GetHealthStatus(ctx context.Context, containerID string) string {
	return "healthy"
}

func (f *fakeDeployDocker) GetStartupLogs(ctx context.Context, containerID string, tailLines int) (string, error) {
	return "", nil
}

func (f *fakeDeployDocker) StopAndRemove(ctx context.Context, containerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stoppedIDs = append(f.stoppedIDs, containerID)
	if !f.runCalled {
		f.stoppedBeforeRun = append(f.stoppedBeforeRun, containerID)
	}
	delete(f.containers, containerID)
	delete(f.labels, containerID)
	return nil
}

func startHealthTestServer(t *testing.T) (string, int, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return host, port, srv
}

func TestDeployBlueGreenSweepsAllStaleContainers(t *testing.T) {
	ip, port, srv := startHealthTestServer(t)

	fake := &fakeDeployDocker{
		containers: map[string]docker.ContainerRef{
			"old-running-0001": {ID: "old-running-0001", Name: "clank-api-old1", State: "running"},
			"old-running-0002": {ID: "old-running-0002", Name: "clank-api-old2", State: "running"},
			"old-stopped-0003": {ID: "old-stopped-0003", Name: "clank-api-old3", State: "exited"},
			"other-svc-000001": {ID: "other-svc-000001", Name: "clank-web-1", State: "running"},
		},
		labels: map[string]map[string]string{
			"old-running-0001": {"clank.service_slug": "api"},
			"old-running-0002": {"clank.service_slug": "api"},
			"old-stopped-0003": {"clank.service_slug": "api"},
			"other-svc-000001": {"clank.service_slug": "web"},
		},
		newContainerID: "new-container-01",
		containerIP:    ip,
	}

	d := &Deployer{
		docker:              fake,
		traefikEndpoint:     srv.URL,
		traefikReadyTimeout: time.Second,
		traefikPollInterval: 10 * time.Millisecond,
		crashCheckDelay:     -1,
		cutoverDelay:        -1,
	}

	_, err := d.Deploy(context.Background(), DeployOpts{
		DeploymentID: "dep-12345678",
		ServiceSlug:  "api",
		ImageTag:     "clank-api:v2",
		Port:         port,
		Domains:      []string{"api.example.com"},
		HealthConfig: HealthConfig{
			Path:           "/health",
			Retries:        1,
			TimeoutSeconds: 2,
		},
	}, func(status, message, containerID, containerName string) {})
	if err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	if len(fake.stoppedBeforeRun) != 0 {
		t.Fatalf("blue-green must not remove old containers before starting new, removed: %v", fake.stoppedBeforeRun)
	}

	// Only new-container-01 should remain for slug "api"
	remaining, err := fake.FindAllContainersByLabel(context.Background(), "clank.service_slug", "api")
	if err != nil {
		t.Fatalf("FindAllContainersByLabel: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != "new-container-01" {
		t.Fatalf("expected only new-container-01 remaining for api, got: %+v", remaining)
	}

	// All 3 stale containers must have been stopped and removed
	if len(fake.stoppedIDs) != 3 {
		t.Fatalf("expected 3 stale containers removed, got %d (%v)", len(fake.stoppedIDs), fake.stoppedIDs)
	}

	// Unrelated service container must be untouched
	if _, ok := fake.containers["other-svc-000001"]; !ok {
		t.Fatal("unrelated service container should not have been removed")
	}
}

func TestDeployRecreateVolumeServiceSweepsAllStaleContainers(t *testing.T) {
	ip, port, srv := startHealthTestServer(t)

	fake := &fakeDeployDocker{
		containers: map[string]docker.ContainerRef{
			"old-db-run-00001": {ID: "old-db-run-00001", Name: "clank-db-old1", State: "running"},
			"old-db-run-00002": {ID: "old-db-run-00002", Name: "clank-db-old2", State: "running"},
			"old-db-stp-00003": {ID: "old-db-stp-00003", Name: "clank-db-old3", State: "exited"},
		},
		labels: map[string]map[string]string{
			"old-db-run-00001": {"clank.service_slug": "db"},
			"old-db-run-00002": {"clank.service_slug": "db"},
			"old-db-stp-00003": {"clank.service_slug": "db"},
		},
		newContainerID: "new-db-000000001",
		containerIP:    ip,
	}

	d := &Deployer{
		docker:              fake,
		traefikEndpoint:     srv.URL,
		traefikReadyTimeout: time.Second,
		traefikPollInterval: 10 * time.Millisecond,
		crashCheckDelay:     -1,
		cutoverDelay:        -1,
	}

	_, err := d.Deploy(context.Background(), DeployOpts{
		DeploymentID: "dep-db123456",
		ServiceSlug:  "db",
		ImageTag:     "clank-db:v2",
		Port:         port,
		Volumes: []docker.VolumeMount{
			{Name: "clank-db-data", MountPath: "/data"},
		},
		HealthConfig: HealthConfig{
			Path:           "/health",
			Retries:        1,
			TimeoutSeconds: 2,
		},
	}, func(status, message, containerID, containerName string) {})
	if err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	// All 3 stale containers must have been removed BEFORE RunContainer
	if len(fake.stoppedBeforeRun) != 3 {
		t.Fatalf("expected 3 stale containers removed before recreate, got %d (%v)", len(fake.stoppedBeforeRun), fake.stoppedBeforeRun)
	}

	remaining, err := fake.FindAllContainersByLabel(context.Background(), "clank.service_slug", "db")
	if err != nil {
		t.Fatalf("FindAllContainersByLabel: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != "new-db-000000001" {
		t.Fatalf("expected only new-db-000000001 remaining for db, got: %+v", remaining)
	}
}
