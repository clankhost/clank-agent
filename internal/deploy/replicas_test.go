package deploy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clankhost/clank-agent/internal/docker"
)

type fakeReplicaDocker struct {
	mu               sync.Mutex
	containers       map[string]docker.ContainerRef
	labels           map[string]map[string]string
	runOpts          []docker.RunOpts
	stoppedIDs       []string
	stoppedBeforeRun []string
	containerIPs     map[string]string
	defaultIP        string
	nextSeq          int
}

func (f *fakeReplicaDocker) EnsureNetwork(ctx context.Context, name string) error { return nil }

func (f *fakeReplicaDocker) FindContainerByLabel(ctx context.Context, key, value string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, c := range f.containers {
		if c.State == "running" && f.labels[id][key] == value {
			return c.ID, c.Name, nil
		}
	}
	return "", "", nil
}

func (f *fakeReplicaDocker) FindAllContainersByLabel(ctx context.Context, key, value string) ([]docker.ContainerRef, error) {
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

func (f *fakeReplicaDocker) FindContainerByName(ctx context.Context, name string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.Name == name {
			return c.ID, c.Name, nil
		}
	}
	return "", "", nil
}

func (f *fakeReplicaDocker) FindTraefikContainer(ctx context.Context) string { return "" }

func (f *fakeReplicaDocker) ConnectToNetworkIfNeeded(ctx context.Context, containerID, networkName string) error {
	return nil
}

func (f *fakeReplicaDocker) PullImage(ctx context.Context, img string, auth *docker.RegistryAuth, onLog func(string)) error {
	return nil
}

func (f *fakeReplicaDocker) InspectImage(ctx context.Context, imageRef string) (*docker.ImageMeta, error) {
	return &docker.ImageMeta{}, nil
}

func (f *fakeReplicaDocker) GetImageDigest(ctx context.Context, imageRef string) (string, error) {
	return "sha256:replicadigest", nil
}

func (f *fakeReplicaDocker) EnsureVolumeOwnership(ctx context.Context, imageRef string, volumes []docker.VolumeMount) error {
	return nil
}

func (f *fakeReplicaDocker) RunContainer(ctx context.Context, opts docker.RunOpts) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSeq++
	id := fmt.Sprintf("new-replica-%04d", f.nextSeq)

	// Copy env and labels so tests can inspect exact values passed at container creation
	envCopy := make(map[string]string, len(opts.Env))
	for k, v := range opts.Env {
		envCopy[k] = v
	}
	lblsCopy := make(map[string]string, len(opts.Labels))
	for k, v := range opts.Labels {
		lblsCopy[k] = v
	}
	recorded := opts
	recorded.Env = envCopy
	recorded.Labels = lblsCopy
	f.runOpts = append(f.runOpts, recorded)

	f.containers[id] = docker.ContainerRef{
		ID:    id,
		Name:  opts.Name,
		State: "running",
	}
	f.labels[id] = lblsCopy
	return id, nil
}

func (f *fakeReplicaDocker) InspectContainer(ctx context.Context, containerID string) (*docker.ContainerInspection, error) {
	ip, _ := f.GetContainerIP(ctx, containerID, "")
	return &docker.ContainerInspection{
		State: "running",
		IP:    ip,
	}, nil
}

func (f *fakeReplicaDocker) GetContainerIP(ctx context.Context, containerID, networkName string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ip, ok := f.containerIPs[containerID]; ok {
		return ip, nil
	}
	return f.defaultIP, nil
}

func (f *fakeReplicaDocker) GetHealthStatus(ctx context.Context, containerID string) string {
	return "healthy"
}

func (f *fakeReplicaDocker) GetStartupLogs(ctx context.Context, containerID string, tailLines int) (string, error) {
	return "replica logs", nil
}

func (f *fakeReplicaDocker) StopAndRemove(ctx context.Context, containerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stoppedIDs = append(f.stoppedIDs, containerID)
	if len(f.runOpts) == 0 {
		f.stoppedBeforeRun = append(f.stoppedBeforeRun, containerID)
	}
	delete(f.containers, containerID)
	delete(f.labels, containerID)
	return nil
}

func TestExtractReplicasAndSanitizeEnv(t *testing.T) {
	raw := map[string]string{
		"APP_ENV":          "production",
		"__CLANK_REPLICAS": "4",
		"__CLANK_INTERNAL": "secret-flag",
	}
	n, clean := extractReplicasAndSanitizeEnv(raw)
	if n != 4 {
		t.Fatalf("expected replicas=4, got %d", n)
	}
	if clean["APP_ENV"] != "production" {
		t.Fatalf("expected APP_ENV preserved, got %q", clean["APP_ENV"])
	}
	if _, ok := clean["__CLANK_REPLICAS"]; ok {
		t.Fatal("__CLANK_REPLICAS must be stripped from runtime env")
	}
	if _, ok := clean["__CLANK_INTERNAL"]; ok {
		t.Fatal("__CLANK_* internal keys must be stripped from runtime env")
	}
	// Original map must not be mutated
	if raw["__CLANK_REPLICAS"] != "4" {
		t.Fatal("input env map should not be mutated")
	}

	// Clamping checks
	if got, _ := extractReplicasAndSanitizeEnv(map[string]string{"__CLANK_REPLICAS": "99"}); got != 10 {
		t.Fatalf("expected upper clamp to 10, got %d", got)
	}
	if got, _ := extractReplicasAndSanitizeEnv(map[string]string{"__CLANK_REPLICAS": "0"}); got != 1 {
		t.Fatalf("expected lower clamp to 1, got %d", got)
	}
	if got, _ := extractReplicasAndSanitizeEnv(map[string]string{"__CLANK_REPLICAS": "invalid"}); got != 1 {
		t.Fatalf("expected fallback to 1 for invalid value, got %d", got)
	}
}

func TestDeploySingleReplicaPreservesPathAndStripsControlEnv(t *testing.T) {
	ip, port, srv := startHealthTestServer(t)

	fake := &fakeReplicaDocker{
		containers: map[string]docker.ContainerRef{
			"old-single-00001": {ID: "old-single-00001", Name: "clank-api-old0", State: "running"},
		},
		labels: map[string]map[string]string{
			"old-single-00001": {"clank.service_slug": "api"},
		},
		defaultIP: ip,
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
		DeploymentID: "dep-11112222",
		ServiceSlug:  "api",
		ImageTag:     "clank-api:v1",
		Port:         port,
		Env: map[string]string{
			"PORT":             strconv.Itoa(port),
			"__CLANK_REPLICAS": "1",
			"__CLANK_INTERNAL": "strip-me",
		},
		Domains: []string{"api.example.com"},
		HealthConfig: HealthConfig{
			Path:           "/health",
			Retries:        1,
			TimeoutSeconds: 2,
		},
	}, func(status, message, containerID, containerName string) {})
	if err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}

	if len(fake.runOpts) != 1 {
		t.Fatalf("expected 1 container started, got %d", len(fake.runOpts))
	}
	if fake.runOpts[0].Name != "clank-api-dep-1111" {
		t.Fatalf("expected single container name clank-api-dep-1111, got %s", fake.runOpts[0].Name)
	}
	if _, ok := fake.runOpts[0].Env["__CLANK_REPLICAS"]; ok {
		t.Fatal("__CLANK_REPLICAS leaked into single container env")
	}
	if _, ok := fake.runOpts[0].Env["__CLANK_INTERNAL"]; ok {
		t.Fatal("__CLANK_INTERNAL leaked into single container env")
	}

	remaining, _ := fake.FindAllContainersByLabel(context.Background(), "clank.service_slug", "api")
	if len(remaining) != 1 || remaining[0].ID != "new-replica-0001" {
		t.Fatalf("expected only new-replica-0001 remaining, got %+v", remaining)
	}
}

func TestDeployMultiReplicaThreeContainersStripsEnvAndSweepsOld(t *testing.T) {
	ip, port, srv := startHealthTestServer(t)

	fake := &fakeReplicaDocker{
		containers: map[string]docker.ContainerRef{
			"old-rep-00000001": {ID: "old-rep-00000001", Name: "clank-api-prev-r0", State: "running"},
			"old-rep-00000002": {ID: "old-rep-00000002", Name: "clank-api-prev-r1", State: "running"},
			"other-svc-000001": {ID: "other-svc-000001", Name: "clank-web-1", State: "running"},
		},
		labels: map[string]map[string]string{
			"old-rep-00000001": {"clank.service_slug": "api"},
			"old-rep-00000002": {"clank.service_slug": "api"},
			"other-svc-000001": {"clank.service_slug": "web"},
		},
		defaultIP: ip,
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
		DeploymentID: "dep-abcdef12",
		ServiceSlug:  "api",
		ImageTag:     "clank-api:v3",
		Port:         port,
		Env: map[string]string{
			"APP_KEY":          "value",
			"__CLANK_REPLICAS": "3",
			"__CLANK_DEBUG":    "true",
		},
		Domains: []string{"api.example.com"},
		HealthConfig: HealthConfig{
			Path:           "/health",
			Retries:        1,
			TimeoutSeconds: 2,
		},
	}, func(status, message, containerID, containerName string) {})
	if err != nil {
		t.Fatalf("Multi-replica Deploy failed: %v", err)
	}

	if len(fake.stoppedBeforeRun) != 0 {
		t.Fatalf("multi-replica must keep old containers running until new replicas are healthy, stopped early: %v", fake.stoppedBeforeRun)
	}

	if len(fake.runOpts) != 3 {
		t.Fatalf("expected 3 containers started, got %d", len(fake.runOpts))
	}

	for i, ro := range fake.runOpts {
		wantName := fmt.Sprintf("clank-api-dep-abcd-r%d", i)
		if ro.Name != wantName {
			t.Fatalf("replica %d name = %q, want %q", i, ro.Name, wantName)
		}
		if ro.Env["APP_KEY"] != "value" {
			t.Fatalf("replica %d missing APP_KEY", i)
		}
		if _, leaked := ro.Env["__CLANK_REPLICAS"]; leaked {
			t.Fatalf("replica %d leaked __CLANK_REPLICAS in container env", i)
		}
		if _, leaked := ro.Env["__CLANK_DEBUG"]; leaked {
			t.Fatalf("replica %d leaked __CLANK_DEBUG in container env", i)
		}
		if ro.Labels["clank.service_slug"] != "api" {
			t.Fatalf("replica %d missing clank.service_slug=api label", i)
		}
		if ro.Labels["traefik.http.services.clank-api.loadbalancer.server.port"] != strconv.Itoa(port) {
			t.Fatalf("replica %d missing Traefik loadbalancer port label: %+v", i, ro.Labels)
		}
	}

	// All 3 new replicas must remain, and both old containers must be swept
	remaining, _ := fake.FindAllContainersByLabel(context.Background(), "clank.service_slug", "api")
	if len(remaining) != 3 {
		t.Fatalf("expected 3 remaining replicas for api, got %d (%+v)", len(remaining), remaining)
	}
	wantIDs := map[string]bool{
		"new-replica-0001": false,
		"new-replica-0002": false,
		"new-replica-0003": false,
	}
	for _, c := range remaining {
		if _, ok := wantIDs[c.ID]; !ok {
			t.Fatalf("unexpected remaining container ID: %s", c.ID)
		}
		wantIDs[c.ID] = true
	}
	for id, seen := range wantIDs {
		if !seen {
			t.Fatalf("expected new replica %s to remain running", id)
		}
	}

	if len(fake.stoppedIDs) != 2 {
		t.Fatalf("expected 2 old containers swept, got %d (%v)", len(fake.stoppedIDs), fake.stoppedIDs)
	}
	if _, ok := fake.containers["other-svc-000001"]; !ok {
		t.Fatal("unrelated service container should remain untouched")
	}
}

func TestDeployMultiReplicaRollsBackAllNewReplicasWhenReplica2FailsHealthCheck(t *testing.T) {
	// Healthy server for replica 1 & 3, unhealthy server for replica 2.
	// We share one listener port and differentiate by request count:
	// ProbeAllPorts + replica 1 health check succeed, then replica 2 returns 500.
	var mu sync.Mutex
	healthHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			mu.Lock()
			healthHits++
			hit := healthHits
			mu.Unlock()
			if hit == 1 {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
				return
			}
			// Replica #2 health check fails
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("unhealthy"))
			return
		}
		w.WriteHeader(http.StatusOK)
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

	fake := &fakeReplicaDocker{
		containers: map[string]docker.ContainerRef{
			"old-healthy-0001": {ID: "old-healthy-0001", Name: "clank-api-old0", State: "running"},
			"old-healthy-0002": {ID: "old-healthy-0002", Name: "clank-api-old1", State: "running"},
		},
		labels: map[string]map[string]string{
			"old-healthy-0001": {"clank.service_slug": "api"},
			"old-healthy-0002": {"clank.service_slug": "api"},
		},
		defaultIP: host,
	}

	d := &Deployer{
		docker:              fake,
		traefikEndpoint:     srv.URL,
		traefikReadyTimeout: time.Second,
		traefikPollInterval: 10 * time.Millisecond,
		crashCheckDelay:     -1,
		cutoverDelay:        -1,
	}

	_, err = d.Deploy(context.Background(), DeployOpts{
		DeploymentID: "dep-rollback1",
		ServiceSlug:  "api",
		ImageTag:     "clank-api:v-bad",
		Port:         port,
		Env: map[string]string{
			"__CLANK_REPLICAS": "3",
		},
		Domains: []string{"api.example.com"},
		HealthConfig: HealthConfig{
			Path:            "/health",
			Retries:         1,
			IntervalSeconds: 1,
			TimeoutSeconds:  2,
		},
	}, func(status, message, containerID, containerName string) {})
	if err == nil {
		t.Fatal("expected Deploy to fail when replica #2 fails health check")
	}
	if !strings.Contains(err.Error(), "replica 2/3") {
		t.Fatalf("expected error referencing replica 2/3, got: %v", err)
	}

	// All 3 newly started replicas must have been rolled back (stopped & removed)
	if len(fake.stoppedIDs) != 3 {
		t.Fatalf("expected all 3 newly started replicas to be stopped/removed, got %d (%v)", len(fake.stoppedIDs), fake.stoppedIDs)
	}
	for _, id := range fake.stoppedIDs {
		if !strings.HasPrefix(id, "new-replica-") {
			t.Fatalf("rollback must never stop pre-existing container %s", id)
		}
	}

	// Both pre-existing healthy containers must still be running untouched
	remaining, _ := fake.FindAllContainersByLabel(context.Background(), "clank.service_slug", "api")
	if len(remaining) != 2 {
		t.Fatalf("expected both old healthy containers untouched, got %d (%+v)", len(remaining), remaining)
	}
	if _, ok := fake.containers["old-healthy-0001"]; !ok {
		t.Fatal("old-healthy-0001 must remain running")
	}
	if _, ok := fake.containers["old-healthy-0002"]; !ok {
		t.Fatal("old-healthy-0002 must remain running")
	}
}
