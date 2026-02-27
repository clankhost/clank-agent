package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	goarchive "github.com/moby/go-archive"
)

// Manager wraps the Docker Engine API.
type Manager struct {
	cli *client.Client
}

// NewManager creates a Docker manager from the environment.
func NewManager() (*Manager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}
	return &Manager{cli: cli}, nil
}

// PullImage pulls a Docker image, logging progress.
func (m *Manager) PullImage(ctx context.Context, img string, onLog func(string)) error {
	onLog(fmt.Sprintf("Pulling image %s...", img))

	reader, err := m.cli.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pulling image %s: %w", img, err)
	}
	defer reader.Close()

	// Drain the pull output (JSON stream)
	dec := json.NewDecoder(reader)
	for {
		var msg struct {
			Status   string `json:"status"`
			Progress string `json:"progress"`
			Error    string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("reading pull progress: %w", err)
		}
		if msg.Error != "" {
			return fmt.Errorf("pull error: %s", msg.Error)
		}
	}

	onLog(fmt.Sprintf("Image %s pulled", img))
	return nil
}

// BuildImage builds a Docker image from a context directory.
func (m *Manager) BuildImage(ctx context.Context, contextPath, tag, dockerfile string, onLog func(string)) error {
	onLog(fmt.Sprintf("Building image %s...", tag))

	// Create a tar archive of the build context
	tar, err := goarchive.TarWithOptions(contextPath, &goarchive.TarOptions{})
	if err != nil {
		return fmt.Errorf("creating build context tar: %w", err)
	}
	defer tar.Close()

	resp, err := m.cli.ImageBuild(ctx, tar, build.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: dockerfile,
		Remove:     true,
	})
	if err != nil {
		return fmt.Errorf("building image: %w", err)
	}
	defer resp.Body.Close()

	// Stream build output
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("reading build output: %w", err)
		}
		if msg.Error != "" {
			return fmt.Errorf("build error: %s", msg.Error)
		}
		if msg.Stream != "" {
			line := strings.TrimSpace(msg.Stream)
			if line != "" {
				onLog(line)
			}
		}
	}

	onLog(fmt.Sprintf("Image %s built", tag))
	return nil
}

// RunContainer creates and starts a container with the given options.
// Returns the container ID.
func (m *Manager) RunContainer(ctx context.Context, opts RunOpts) (string, error) {
	// Build env slice
	var env []string
	for k, v := range opts.Env {
		env = append(env, k+"="+v)
	}

	// Port for exposed ports
	exposedPort, _ := nat.NewPort("tcp", fmt.Sprintf("%d", opts.Port))

	// Resource limits
	var resources container.Resources
	if opts.CPULimit > 0 {
		resources.NanoCPUs = int64(opts.CPULimit * 1e9)
	}
	if opts.MemoryLimitMB > 0 {
		resources.Memory = int64(opts.MemoryLimitMB) * 1024 * 1024
	}
	resources.PidsLimit = int64Ptr(512)

	config := &container.Config{
		Image:        opts.Image,
		Env:          env,
		Labels:       opts.Labels,
		ExposedPorts: nat.PortSet{exposedPort: struct{}{}},
	}

	hostConfig := &container.HostConfig{
		Resources:     resources,
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		CapDrop:       []string{"ALL"},
		SecurityOpt:   []string{"no-new-privileges"},
	}

	networkConfig := &network.NetworkingConfig{}
	if opts.Network != "" {
		networkConfig.EndpointsConfig = map[string]*network.EndpointSettings{
			opts.Network: {},
		}
	}

	resp, err := m.cli.ContainerCreate(ctx, config, hostConfig, networkConfig, nil, opts.Name)
	if err != nil {
		return "", fmt.Errorf("creating container %s: %w", opts.Name, err)
	}

	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up the created container
		_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("starting container %s: %w", opts.Name, err)
	}

	return resp.ID, nil
}

// StopAndRemove stops and removes a container.
func (m *Manager) StopAndRemove(ctx context.Context, containerID string) error {
	timeout := 10
	_ = m.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
	return m.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// StopContainer stops a container.
func (m *Manager) StopContainer(ctx context.Context, containerID string) error {
	timeout := 10
	return m.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
}

// StartContainer starts a stopped container.
func (m *Manager) StartContainer(ctx context.Context, containerID string) error {
	return m.cli.ContainerStart(ctx, containerID, container.StartOptions{})
}

// RestartContainer restarts a container.
func (m *Manager) RestartContainer(ctx context.Context, containerID string) error {
	timeout := 10
	return m.cli.ContainerRestart(ctx, containerID, container.StopOptions{Timeout: &timeout})
}

// EnsureNetwork creates a network if it doesn't exist.
func (m *Manager) EnsureNetwork(ctx context.Context, name string) error {
	networks, err := m.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return fmt.Errorf("listing networks: %w", err)
	}
	for _, n := range networks {
		if n.Name == name {
			return nil
		}
	}

	_, err = m.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
	})
	if err != nil {
		return fmt.Errorf("creating network %s: %w", name, err)
	}
	return nil
}

// ConnectToNetwork connects a container to a network with optional aliases.
func (m *Manager) ConnectToNetwork(ctx context.Context, containerID, networkName string, aliases []string) error {
	return m.cli.NetworkConnect(ctx, networkName, containerID, &network.EndpointSettings{
		Aliases: aliases,
	})
}

// FindContainerByLabel finds the first running container with the given label key=value.
// Returns the container ID and name, or empty strings if not found.
func (m *Manager) FindContainerByLabel(ctx context.Context, key, value string) (string, string, error) {
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", key+"="+value)),
	})
	if err != nil {
		return "", "", fmt.Errorf("listing containers by label: %w", err)
	}
	if len(containers) == 0 {
		return "", "", nil
	}
	c := containers[0]
	name := ""
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}
	return c.ID, name, nil
}

// ListManagedContainers lists all containers with the clank.managed=true label.
func (m *Manager) ListManagedContainers(ctx context.Context) ([]ContainerInfo, error) {
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "clank.managed=true")),
	})
	if err != nil {
		return nil, fmt.Errorf("listing managed containers: %w", err)
	}

	var result []ContainerInfo
	for _, c := range containers {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		result = append(result, ContainerInfo{
			ContainerID: c.ID[:12],
			Name:        name,
			State:       c.State,
			Image:       c.Image,
			Labels:      c.Labels,
		})
	}
	return result, nil
}

// GetContainerIP returns the IP address of a container on a specific network.
func (m *Manager) GetContainerIP(ctx context.Context, containerID, networkName string) (string, error) {
	inspect, err := m.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspecting container: %w", err)
	}

	if networkName != "" {
		if ep, ok := inspect.NetworkSettings.Networks[networkName]; ok && ep.IPAddress != "" {
			return ep.IPAddress, nil
		}
	}

	// Fallback: use the default bridge IP
	if inspect.NetworkSettings.IPAddress != "" {
		return inspect.NetworkSettings.IPAddress, nil
	}

	// Try any network
	for _, ep := range inspect.NetworkSettings.Networks {
		if ep.IPAddress != "" {
			return ep.IPAddress, nil
		}
	}

	return "", fmt.Errorf("no IP address found for container %s", containerID)
}

// ContainerLogs returns a multiplexed ReadCloser for streaming container logs.
// If follow is true, the stream blocks for new output. The tail parameter
// controls how many existing lines to return ("0" for none, "all" for all).
func (m *Manager) ContainerLogs(ctx context.Context, containerID string, follow bool, tail string) (io.ReadCloser, error) {
	return m.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       tail,
		Timestamps: true,
	})
}

// ContainerStatsOneShot returns a single stats snapshot for a container.
// The caller must close the Body on the returned StatsResponseReader.
func (m *Manager) ContainerStatsOneShot(ctx context.Context, containerID string) (container.StatsResponseReader, error) {
	return m.cli.ContainerStatsOneShot(ctx, containerID)
}

func int64Ptr(v int64) *int64 {
	return &v
}
