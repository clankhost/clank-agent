package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/docker/docker/api/types/container"
)

// ImageMeta holds metadata extracted from a Docker image inspection.
type ImageMeta struct {
	ExposedPorts []int
	Healthcheck  *HealthcheckMeta
	Cmd          []string
	Entrypoint   []string
	Volumes      []string // VOLUME directive paths from the Dockerfile
}

// HealthcheckMeta describes a HEALTHCHECK instruction from a Dockerfile.
type HealthcheckMeta struct {
	Test     []string
	Interval string
	Timeout  string
	Retries  int
}

// ContainerInspection holds state extracted from a running/stopped container.
type ContainerInspection struct {
	State    string // "running", "exited", etc.
	ExitCode int
	OOMKilled bool
	IP       string
	Networks []string
	Ports    []DiscoveredPort
}

// DiscoveredPort represents a port found via image EXPOSE, config, or probing.
type DiscoveredPort struct {
	Port     int
	Protocol string // "http", "tcp", "closed"
	Source   string // "config", "expose", "probe"
}

const maxStartupLogBytes = 32 * 1024 // 32 KB cap

// InspectImage extracts metadata from a Docker image.
func (m *Manager) InspectImage(ctx context.Context, imageRef string) (*ImageMeta, error) {
	inspect, _, err := m.cli.ImageInspectWithRaw(ctx, imageRef)
	if err != nil {
		return nil, fmt.Errorf("inspecting image %s: %w", imageRef, err)
	}

	meta := &ImageMeta{}

	// Extract EXPOSE ports
	if inspect.Config != nil {
		for portSpec := range inspect.Config.ExposedPorts {
			// portSpec is nat.Port, e.g. "8080/tcp" — extract the number before "/"
			portStr := strings.Split(string(portSpec), "/")[0]
			if p, err := strconv.Atoi(portStr); err == nil {
				meta.ExposedPorts = append(meta.ExposedPorts, p)
			}
		}
		sort.Ints(meta.ExposedPorts)

		meta.Cmd = inspect.Config.Cmd
		meta.Entrypoint = inspect.Config.Entrypoint

		// Extract VOLUME directives
		if len(inspect.Config.Volumes) > 0 {
			for path := range inspect.Config.Volumes {
				meta.Volumes = append(meta.Volumes, path)
			}
			sort.Strings(meta.Volumes)
		}

		// Extract HEALTHCHECK
		if inspect.Config.Healthcheck != nil && len(inspect.Config.Healthcheck.Test) > 0 {
			hc := inspect.Config.Healthcheck
			meta.Healthcheck = &HealthcheckMeta{
				Test:     hc.Test,
				Interval: hc.Interval.String(),
				Timeout:  hc.Timeout.String(),
				Retries:  hc.Retries,
			}
		}
	}

	return meta, nil
}

// InspectContainer extracts state from a container.
func (m *Manager) InspectContainer(ctx context.Context, containerID string) (*ContainerInspection, error) {
	inspect, err := m.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("inspecting container %s: %w", containerID, err)
	}

	ci := &ContainerInspection{}

	if inspect.State != nil {
		ci.State = inspect.State.Status
		ci.ExitCode = inspect.State.ExitCode
		ci.OOMKilled = inspect.State.OOMKilled
	}

	if inspect.NetworkSettings != nil {
		ci.IP = inspect.NetworkSettings.IPAddress
		for name, ep := range inspect.NetworkSettings.Networks {
			ci.Networks = append(ci.Networks, name)
			if ci.IP == "" && ep.IPAddress != "" {
				ci.IP = ep.IPAddress
			}
		}
		sort.Strings(ci.Networks)
	}

	return ci, nil
}

// GetHealthStatus returns the Docker HEALTHCHECK status of a container:
// "healthy", "unhealthy", "starting", or "" if no healthcheck is configured.
func (m *Manager) GetHealthStatus(ctx context.Context, containerID string) string {
	inspect, err := m.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return ""
	}
	if inspect.State == nil || inspect.State.Health == nil {
		return ""
	}
	return inspect.State.Health.Status
}

// GetStartupLogs returns the last N lines of container logs, capped at 32KB.
// Strips Docker multiplexed stream headers (8-byte prefix per frame).
func (m *Manager) GetStartupLogs(ctx context.Context, containerID string, lines int) (string, error) {
	if lines <= 0 {
		lines = 100
	}

	reader, err := m.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     false,
		Tail:       strconv.Itoa(lines),
		Timestamps: false,
	})
	if err != nil {
		return "", fmt.Errorf("getting container logs: %w", err)
	}
	defer reader.Close()

	raw, err := io.ReadAll(io.LimitReader(reader, int64(maxStartupLogBytes+8*int(lines))))
	if err != nil {
		return "", fmt.Errorf("reading container logs: %w", err)
	}

	// Strip Docker multiplexed stream headers.
	// Each frame: [1 byte stream type][3 bytes padding][4 bytes big-endian size][payload]
	cleaned := stripDockerStreamHeaders(raw)

	if len(cleaned) > maxStartupLogBytes {
		cleaned = cleaned[:maxStartupLogBytes]
	}

	result := strings.TrimSpace(string(cleaned))
	// Proto3 string fields must be valid UTF-8. Container logs can contain
	// arbitrary binary data, so replace invalid sequences with U+FFFD.
	if !utf8.ValidString(result) {
		result = strings.ToValidUTF8(result, "\uFFFD")
	}
	return result, nil
}

// stripDockerStreamHeaders removes the 8-byte header prefix from each frame
// in Docker's multiplexed log stream.
func stripDockerStreamHeaders(data []byte) []byte {
	var buf bytes.Buffer
	for len(data) >= 8 {
		// header: [stream_type(1)][padding(3)][size(4 big-endian)]
		frameSize := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
		data = data[8:]
		if frameSize <= 0 {
			continue
		}
		if frameSize > len(data) {
			frameSize = len(data)
		}
		buf.Write(data[:frameSize])
		data = data[frameSize:]
	}
	// If nothing was parsed (non-multiplexed output), return original
	if buf.Len() == 0 && len(data) > 0 {
		return data
	}
	return buf.Bytes()
}
