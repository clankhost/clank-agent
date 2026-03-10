package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
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
	if len(opts.Command) > 0 {
		config.Cmd = opts.Command
	}
	if opts.Entrypoint != nil {
		config.Entrypoint = opts.Entrypoint
	}

	// Build volume mounts
	var mounts []mount.Mount
	for _, vol := range opts.Volumes {
		if !isValidMountPath(vol.MountPath) {
			return "", fmt.Errorf("invalid volume mount path: %s", vol.MountPath)
		}
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: vol.Name,
			Target: vol.MountPath,
		})
	}

	// Security: drop ALL, then add back the Docker-default capabilities minus
	// the truly dangerous ones (NET_RAW, SYS_CHROOT, AUDIT_WRITE, SETPCAP,
	// SETFCAP, MKNOD).  This lets most images (wordpress, postgres, etc.)
	// work while still being significantly more restrictive than defaults.
	hostConfig := &container.HostConfig{
		Resources:     resources,
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		Mounts:        mounts,
		CapDrop:       []string{"ALL"},
		CapAdd: []string{
			"CHOWN",
			"DAC_OVERRIDE",
			"FOWNER",
			"FSETID",
			"KILL",
			"SETGID",
			"SETUID",
			"NET_BIND_SERVICE",
		},
		SecurityOpt: []string{"no-new-privileges"},
	}

	networkConfig := &network.NetworkingConfig{}
	if opts.Network != "" {
		epSettings := &network.EndpointSettings{}
		if opts.NetworkAlias != "" {
			epSettings.Aliases = []string{opts.NetworkAlias}
		}
		networkConfig.EndpointsConfig = map[string]*network.EndpointSettings{
			opts.Network: epSettings,
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
// Uses create-then-ignore-exists to avoid TOCTOU race when multiple
// deploys for the same project run concurrently.
func (m *Manager) EnsureNetwork(ctx context.Context, name string) error {
	_, err := m.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge",
	})
	if err != nil {
		// Another goroutine (concurrent deploy) may have created it first — that's fine.
		if strings.Contains(err.Error(), "already exists") {
			return nil
		}
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

// ListContainersByLabel returns all containers (including stopped) with the given label key=value.
func (m *Manager) ListContainersByLabel(ctx context.Context, key, value string) ([]ContainerInfo, error) {
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", key+"="+value)),
	})
	if err != nil {
		return nil, fmt.Errorf("listing containers by label %s=%s: %w", key, value, err)
	}

	var result []ContainerInfo
	for _, c := range containers {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		result = append(result, ContainerInfo{
			ContainerID: c.ID,
			Name:        name,
			State:       c.State,
			Image:       c.Image,
			Labels:      c.Labels,
		})
	}
	return result, nil
}

// RemoveImages removes all images matching a tag prefix (e.g. "clank-myapp:").
// Best-effort: errors are logged but not returned.
func (m *Manager) RemoveImages(ctx context.Context, tagPrefix string) {
	images, err := m.cli.ImageList(ctx, image.ListOptions{All: false})
	if err != nil {
		fmt.Printf("Warning: failed to list images for cleanup: %v\n", err)
		return
	}
	for _, img := range images {
		for _, tag := range img.RepoTags {
			if strings.HasPrefix(tag, tagPrefix) {
				_, err := m.cli.ImageRemove(ctx, tag, image.RemoveOptions{Force: false})
				if err != nil {
					fmt.Printf("Warning: failed to remove image %s: %v\n", tag, err)
				} else {
					fmt.Printf("Removed build image %s\n", tag)
				}
			}
		}
	}
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

// DetectDockerSocket returns the Docker socket URI for the current platform.
// It checks DOCKER_HOST first, then falls back to platform-specific defaults.
func DetectDockerSocket() string {
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		return host
	}
	switch runtime.GOOS {
	case "windows":
		return "npipe:////./pipe/docker_engine"
	case "darwin":
		if _, err := os.Stat("/var/run/docker.sock"); err == nil {
			return "unix:///var/run/docker.sock"
		}
		if home, err := os.UserHomeDir(); err == nil {
			alt := filepath.Join(home, ".docker", "run", "docker.sock")
			if _, err := os.Stat(alt); err == nil {
				return "unix://" + alt
			}
		}
		return "unix:///var/run/docker.sock"
	default:
		return "unix:///var/run/docker.sock"
	}
}

// IsDockerAvailable checks if the Docker daemon is reachable.
// Returns true and the Docker version string on success,
// or false and an error description on failure.
func IsDockerAvailable() (bool, string) {
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return false, fmt.Sprintf("docker not reachable: %v", err)
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return false, "docker returned empty version"
	}
	return true, version
}

// FindTraefikContainer returns the ID of the running Traefik container
// (identified by the clank.traefik=true label), or empty string if not found.
func (m *Manager) FindTraefikContainer(ctx context.Context) string {
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", "clank.traefik=true")),
	})
	if err != nil || len(containers) == 0 {
		return ""
	}
	return containers[0].ID
}

// ListClankProjectNetworks returns all Docker networks whose name starts with "clank-project-".
func (m *Manager) ListClankProjectNetworks(ctx context.Context) ([]NetworkInfo, error) {
	networks, err := m.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing networks: %w", err)
	}
	var result []NetworkInfo
	for _, n := range networks {
		if strings.HasPrefix(n.Name, "clank-project-") {
			result = append(result, NetworkInfo{ID: n.ID, Name: n.Name})
		}
	}
	return result, nil
}

// RemoveNetworkIfEmpty inspects a network and removes it only if it has
// zero connected containers. Returns true if the network was removed.
func (m *Manager) RemoveNetworkIfEmpty(ctx context.Context, networkID string) (bool, error) {
	inspect, err := m.cli.NetworkInspect(ctx, networkID, network.InspectOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, fmt.Errorf("inspecting network: %w", err)
	}
	if len(inspect.Containers) > 0 {
		return false, nil
	}
	if err := m.cli.NetworkRemove(ctx, networkID); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, fmt.Errorf("removing network %s: %w", inspect.Name, err)
	}
	return true, nil
}

// ConnectToNetworkIfNeeded connects a container to a network, skipping
// if already connected.
func (m *Manager) ConnectToNetworkIfNeeded(ctx context.Context, containerID, networkName string) error {
	inspect, err := m.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return fmt.Errorf("inspecting container: %w", err)
	}
	if _, ok := inspect.NetworkSettings.Networks[networkName]; ok {
		return nil // already connected
	}
	return m.cli.NetworkConnect(ctx, networkName, containerID, &network.EndpointSettings{})
}

func int64Ptr(v int64) *int64 {
	return &v
}

// blockedMountPaths are system paths that must never be volume-mounted.
var blockedMountPaths = []string{
	"/var/run", "/proc", "/sys", "/dev", "/etc",
	"/root", "/boot", "/lib", "/sbin", "/bin",
}

// isValidMountPath checks that a mount path is absolute and not in the blocked set.
func isValidMountPath(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return false
	}
	for _, blocked := range blockedMountPaths {
		if path == blocked || strings.HasPrefix(path, blocked+"/") {
			return false
		}
	}
	return true
}

// EnsureVolumeOwnership runs an ephemeral init container to chown volume
// mount paths to the image's default user. This prevents permission errors
// when Docker creates volumes as root but the image runs as a non-root user.
// Skips if the image runs as root or has no user set.
func (m *Manager) EnsureVolumeOwnership(ctx context.Context, imageName string, volumes []VolumeMount) error {
	if len(volumes) == 0 {
		return nil
	}

	// Inspect image to find its default user
	inspect, _, err := m.cli.ImageInspectWithRaw(ctx, imageName)
	if err != nil {
		fmt.Printf("Warning: could not inspect image %s for volume ownership: %v\n", imageName, err)
		return nil // best-effort — don't block deploy
	}

	imgUser := inspect.Config.User
	if imgUser == "" || imgUser == "root" || imgUser == "0" || imgUser == "0:0" {
		return nil // root user — no chown needed
	}

	// Build volume mounts for the init container
	var initMounts []mount.Mount
	var mountPaths []string
	for _, vol := range volumes {
		initMounts = append(initMounts, mount.Mount{
			Type:   mount.TypeVolume,
			Source: vol.Name,
			Target: vol.MountPath,
		})
		mountPaths = append(mountPaths, vol.MountPath)
	}

	chownCmd := fmt.Sprintf("chown -R %s %s", imgUser, strings.Join(mountPaths, " "))
	initName := fmt.Sprintf("clank-vol-init-%d", os.Getpid())

	resp, err := m.cli.ContainerCreate(ctx, &container.Config{
		Image:      imageName,
		Entrypoint: []string{"/bin/sh", "-c", chownCmd},
		User:       "root",
	}, &container.HostConfig{
		Mounts: initMounts,
	}, nil, nil, initName)
	if err != nil {
		fmt.Printf("Warning: failed to create volume init container: %v\n", err)
		return nil
	}

	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		fmt.Printf("Warning: failed to start volume init container: %v\n", err)
		return nil
	}

	// Wait for the init container to finish
	statusCh, errCh := m.cli.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			fmt.Printf("Warning: volume init container error: %v\n", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			fmt.Printf("Warning: volume init container exited with code %d\n", status.StatusCode)
		}
	}

	// Remove the init container
	_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
	fmt.Printf("Volume ownership fixed for user %s on %d mount(s)\n", imgUser, len(volumes))
	return nil
}

// RemoveVolume removes a named Docker volume by name.
func (m *Manager) RemoveVolume(ctx context.Context, name string) error {
	return m.cli.VolumeRemove(ctx, name, false)
}

// VolumeUsage holds the name and disk consumption of a single Docker volume.
type VolumeUsage struct {
	Name      string
	SizeBytes int64
}

// DiskUsageResult summarises Docker disk consumption by category.
type DiskUsageResult struct {
	ImagesBytes     int64
	BuildCacheBytes int64
	ContainersBytes int64
	Volumes         []VolumeUsage
}

// DiskUsage calls the Docker /system/df endpoint and returns an aggregate
// breakdown of disk consumed by images, build cache, container writable layers,
// and per-volume sizes.
func (m *Manager) DiskUsage(ctx context.Context) (*DiskUsageResult, error) {
	du, err := m.cli.DiskUsage(ctx, dockertypes.DiskUsageOptions{})
	if err != nil {
		return nil, fmt.Errorf("docker disk usage: %w", err)
	}

	// Use LayersSize for deduplicated total — summing individual image sizes
	// double-counts shared layers and can exceed actual disk usage.
	imagesBytes := du.LayersSize

	var containersBytes int64
	for _, c := range du.Containers {
		if c != nil {
			containersBytes += c.SizeRw
		}
	}

	var buildCacheBytes int64
	for _, bc := range du.BuildCache {
		if bc != nil {
			buildCacheBytes += bc.Size
		}
	}

	var volumes []VolumeUsage
	for _, v := range du.Volumes {
		if v == nil {
			continue
		}
		size := int64(0)
		if v.UsageData != nil && v.UsageData.Size >= 0 {
			size = v.UsageData.Size
		}
		volumes = append(volumes, VolumeUsage{Name: v.Name, SizeBytes: size})
	}

	return &DiskUsageResult{
		ImagesBytes:     imagesBytes,
		BuildCacheBytes: buildCacheBytes,
		ContainersBytes: containersBytes,
		Volumes:         volumes,
	}, nil
}
