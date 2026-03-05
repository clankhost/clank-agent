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
// publicIP binds Traefik to a specific IP (avoids conflicts with Tailscale on 0.0.0.0).
// Pass "" to bind to all interfaces (default behavior).
func (m *Manager) EnsureTraefik(ctx context.Context, publicIP string) error {
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
		PortBindings: traefikPortBindings(publicIP),
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

	// Verify port bindings took effect (Docker can silently skip bindings on conflict)
	inspect, inspErr := m.cli.ContainerInspect(ctx, resp.ID)
	if inspErr == nil && inspect.NetworkSettings != nil {
		bound := 0
		for _, bindings := range inspect.NetworkSettings.Ports {
			bound += len(bindings)
		}
		if bound == 0 {
			log.Printf("Warning: Traefik started but has no port bindings — another process may hold ports 80/443")
		} else {
			log.Printf("Traefik port bindings OK (%d bindings)", bound)
		}
	}

	log.Printf("Traefik started (container %s)", resp.ID[:12])
	return nil
}

// ReconfigureTraefikACME stops and recreates Traefik with Let's Encrypt ACME
// support enabled.  Called when the first public_direct endpoint is created.
// Uses HTTP-01 challenge on the existing :80 entrypoint.
func (m *Manager) ReconfigureTraefikACME(ctx context.Context, publicIP string) error {
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
		PortBindings: traefikPortBindings(publicIP),
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

	// Verify port bindings took effect
	inspect, inspErr := m.cli.ContainerInspect(ctx, resp.ID)
	if inspErr == nil && inspect.NetworkSettings != nil {
		bound := 0
		for _, bindings := range inspect.NetworkSettings.Ports {
			bound += len(bindings)
		}
		if bound == 0 {
			log.Printf("Warning: Traefik ACME started but has no port bindings — another process may hold ports 80/443")
		} else {
			log.Printf("Traefik ACME port bindings OK (%d bindings)", bound)
		}
	}

	log.Printf("Traefik with ACME started (container %s)", resp.ID[:12])
	return nil
}

// traefikPortBindings builds port bindings for Traefik.
// When bindIP is set (e.g. Tailscale conflict), we dual-bind: LAN IP (for
// external NAT traffic) + 127.0.0.1 (for Tailscale Serve and local health checks).
// When bindIP is "", we bind to 0.0.0.0 which already includes localhost.
func traefikPortBindings(bindIP string) nat.PortMap {
	if bindIP == "" {
		return nat.PortMap{
			"80/tcp":  {{HostIP: "", HostPort: "80"}},
			"443/tcp": {{HostIP: "", HostPort: "443"}},
		}
	}
	return nat.PortMap{
		"80/tcp": {
			{HostIP: bindIP, HostPort: "80"},
			{HostIP: "127.0.0.1", HostPort: "80"},
		},
		"443/tcp": {
			{HostIP: bindIP, HostPort: "443"},
			{HostIP: "127.0.0.1", HostPort: "443"},
		},
	}
}

// HasACME checks if the running Traefik instance has ACME configured.
func (m *Manager) HasACME(ctx context.Context) bool {
	id, _, err := m.FindContainerByLabel(ctx, "clank.traefik.acme", "true")
	return err == nil && id != ""
}
