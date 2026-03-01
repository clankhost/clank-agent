package docker

import (
	"context"
	"fmt"
	"log"

	"github.com/docker/docker/api/types/container"
)

const (
	cloudflaredContainerName = "clank-cloudflared"
	cloudflaredImage         = "cloudflare/cloudflared:latest"
)

// EnsureCloudflared ensures a cloudflared tunnel container is running.
// If one is already running, it is stopped and recreated (token may have changed).
func (m *Manager) EnsureCloudflared(ctx context.Context, tunnelToken string) error {
	if tunnelToken == "" {
		return fmt.Errorf("tunnel token is empty")
	}

	// Check if clank-cloudflared is already running
	id, _, err := m.FindContainerByLabel(ctx, "clank.cloudflared", "true")
	if err != nil {
		return fmt.Errorf("checking for cloudflared: %w", err)
	}
	if id != "" {
		log.Println("Stopping existing cloudflared container...")
		if err := m.StopAndRemove(ctx, id); err != nil {
			log.Printf("Warning: failed to remove old cloudflared: %v", err)
		}
	}

	log.Println("Starting cloudflared...")

	// Pull image
	if err := m.PullImage(ctx, cloudflaredImage, func(msg string) {
		log.Printf("  %s", msg)
	}); err != nil {
		return fmt.Errorf("pulling cloudflared image: %w", err)
	}

	config := &container.Config{
		Image: cloudflaredImage,
		Cmd:   []string{"tunnel", "run", "--token", tunnelToken},
		Labels: map[string]string{
			"clank.cloudflared": "true",
			"clank.managed":    "true",
		},
	}

	hostConfig := &container.HostConfig{
		NetworkMode:   "host",
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
	}

	resp, err := m.cli.ContainerCreate(ctx, config, hostConfig, nil, nil, cloudflaredContainerName)
	if err != nil {
		return fmt.Errorf("creating cloudflared container: %w", err)
	}

	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return fmt.Errorf("starting cloudflared container: %w", err)
	}

	log.Printf("Cloudflared started (container %s)", resp.ID[:12])
	return nil
}
