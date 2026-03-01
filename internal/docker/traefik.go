package docker

import (
	"context"
	"fmt"
	"log"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
)

const (
	traefikContainerName = "clank-traefik"
	traefikImage         = "traefik:v3.6"
	servicesNetwork      = "clank-services"
)

// EnsureTraefik ensures a Traefik container is running on the agent host.
// If it's already running, this is a no-op.
func (m *Manager) EnsureTraefik(ctx context.Context) error {
	// Check if clank-traefik is already running
	id, _, err := m.FindContainerByLabel(ctx, "clank.traefik", "true")
	if err != nil {
		return fmt.Errorf("checking for traefik: %w", err)
	}
	if id != "" {
		log.Println("Traefik already running")
		return nil
	}

	// Ensure the services network exists
	if err := m.EnsureNetwork(ctx, servicesNetwork); err != nil {
		return fmt.Errorf("ensuring services network: %w", err)
	}

	log.Println("Starting Traefik...")

	// Pull traefik image
	if err := m.PullImage(ctx, traefikImage, func(msg string) {
		log.Printf("  %s", msg)
	}); err != nil {
		return fmt.Errorf("pulling traefik image: %w", err)
	}

	config := &container.Config{
		Image: traefikImage,
		Cmd: []string{
			"--providers.docker=true",
			"--providers.docker.exposedByDefault=false",
			"--providers.docker.network=" + servicesNetwork,
			"--entrypoints.web.address=:80",
			"--entrypoints.websecure.address=:443",
			"--api.insecure=true",
		},
		Labels: map[string]string{
			"clank.traefik": "true",
			"clank.managed": "true",
		},
		ExposedPorts: nat.PortSet{
			"80/tcp":   {},
			"443/tcp":  {},
			"8080/tcp": {},
		},
	}

	hostConfig := &container.HostConfig{
		PortBindings: nat.PortMap{
			"80/tcp":  {{HostIP: "", HostPort: "80"}},
			"443/tcp": {{HostIP: "", HostPort: "443"}},
		},
		Mounts: []mount.Mount{
			{
				Type:     mount.TypeBind,
				Source:   "/var/run/docker.sock",
				Target:   "/var/run/docker.sock",
				ReadOnly: true,
			},
		},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
	}

	networkConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			servicesNetwork: {},
		},
	}

	resp, err := m.cli.ContainerCreate(ctx, config, hostConfig, networkConfig, nil, traefikContainerName)
	if err != nil {
		return fmt.Errorf("creating traefik container: %w", err)
	}

	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return fmt.Errorf("starting traefik container: %w", err)
	}

	log.Printf("Traefik started (container %s)", resp.ID[:12])
	return nil
}

// ReconfigureTraefikACME stops and recreates Traefik with Let's Encrypt ACME
// support enabled.  Called when the first public_direct endpoint is created.
// Uses HTTP-01 challenge on the existing :80 entrypoint.
func (m *Manager) ReconfigureTraefikACME(ctx context.Context) error {
	// Stop existing Traefik
	id, _, err := m.FindContainerByLabel(ctx, "clank.traefik", "true")
	if err != nil {
		return fmt.Errorf("checking for traefik: %w", err)
	}
	if id != "" {
		log.Println("Stopping Traefik for ACME reconfiguration...")
		if err := m.StopAndRemove(ctx, id); err != nil {
			return fmt.Errorf("stopping traefik: %w", err)
		}
	}

	if err := m.EnsureNetwork(ctx, servicesNetwork); err != nil {
		return fmt.Errorf("ensuring services network: %w", err)
	}

	log.Println("Starting Traefik with ACME (Let's Encrypt)...")

	if err := m.PullImage(ctx, traefikImage, func(msg string) {
		log.Printf("  %s", msg)
	}); err != nil {
		return fmt.Errorf("pulling traefik image: %w", err)
	}

	config := &container.Config{
		Image: traefikImage,
		Cmd: []string{
			"--providers.docker=true",
			"--providers.docker.exposedByDefault=false",
			"--providers.docker.network=" + servicesNetwork,
			"--entrypoints.web.address=:80",
			"--entrypoints.websecure.address=:443",
			"--certificatesresolvers.letsencrypt.acme.httpchallenge.entrypoint=web",
			"--certificatesresolvers.letsencrypt.acme.storage=/acme/acme.json",
			"--api.insecure=true",
		},
		Labels: map[string]string{
			"clank.traefik":      "true",
			"clank.traefik.acme": "true",
			"clank.managed":      "true",
		},
		ExposedPorts: nat.PortSet{
			"80/tcp":   {},
			"443/tcp":  {},
			"8080/tcp": {},
		},
	}

	hostConfig := &container.HostConfig{
		PortBindings: nat.PortMap{
			"80/tcp":  {{HostIP: "", HostPort: "80"}},
			"443/tcp": {{HostIP: "", HostPort: "443"}},
		},
		Mounts: []mount.Mount{
			{
				Type:     mount.TypeBind,
				Source:   "/var/run/docker.sock",
				Target:   "/var/run/docker.sock",
				ReadOnly: true,
			},
			{
				Type:   mount.TypeVolume,
				Source: "clank-acme",
				Target: "/acme",
			},
		},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
	}

	networkConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			servicesNetwork: {},
		},
	}

	resp, err := m.cli.ContainerCreate(ctx, config, hostConfig, networkConfig, nil, traefikContainerName)
	if err != nil {
		return fmt.Errorf("creating traefik container (ACME): %w", err)
	}

	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return fmt.Errorf("starting traefik container (ACME): %w", err)
	}

	log.Printf("Traefik with ACME started (container %s)", resp.ID[:12])
	return nil
}

// HasACME checks if the running Traefik instance has ACME configured.
func (m *Manager) HasACME(ctx context.Context) bool {
	id, _, err := m.FindContainerByLabel(ctx, "clank.traefik.acme", "true")
	return err == nil && id != ""
}
