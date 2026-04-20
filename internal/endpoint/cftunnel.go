package endpoint

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"time"

	"github.com/clankhost/clank-agent/internal/docker"
)

// CFTunnelProvider handles public_tunnel_cloudflare endpoints.
// Manages BYO cloudflared containers. Traefik labels are applied at deploy time.
type CFTunnelProvider struct {
	docker *docker.Manager
}

// NewCFTunnelProvider creates a Cloudflare tunnel endpoint provider.
func NewCFTunnelProvider(dm *docker.Manager) *CFTunnelProvider {
	return &CFTunnelProvider{docker: dm}
}

func (p *CFTunnelProvider) Name() string { return "public_tunnel_cloudflare" }

func (p *CFTunnelProvider) containerName(token string) string {
	hash := sha256.Sum256([]byte(token))
	return fmt.Sprintf("clank-cftunnel-%x", hash[:6])
}

func (p *CFTunnelProvider) Ensure(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	token := cfg.Config["tunnel_token"]
	if token == "" {
		return &ProviderStatus{
			Status:  "error",
			Message: "Cloudflare tunnel token is required. Generate one from the CF dashboard.",
		}, nil
	}

	name := p.containerName(token)

	if err := p.docker.EnsureCloudflaredNamed(ctx, name, token); err != nil {
		return &ProviderStatus{
			Status:  "error",
			Message: fmt.Sprintf("Failed to start cloudflared: %v", err),
			Diagnostics: map[string]string{
				"container": name,
				"error":     err.Error(),
			},
		}, nil
	}

	msg := fmt.Sprintf("Cloudflare tunnel running (%s). Ensure your Tunnel has a Public Hostname route for %s → http://localhost:80", name, cfg.Hostname)
	log.Printf("[cftunnel] Endpoint %s: %s", cfg.EndpointID, msg)

	return &ProviderStatus{
		Status:      "active",
		Message:     msg,
		ResolvedURL: fmt.Sprintf("https://%s", cfg.Hostname),
		VerifiedBy:  "agent",
		Diagnostics: map[string]string{
			"container": name,
		},
	}, nil
}

func (p *CFTunnelProvider) Disable(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	token := cfg.Config["tunnel_token"]
	if token != "" {
		name := p.containerName(token)
		id, _, err := p.docker.FindContainerByLabel(ctx, "clank.cftunnel.name", name)
		if err == nil && id != "" {
			log.Printf("[cftunnel] Stopping cloudflared container %s", name)
			_ = p.docker.StopAndRemove(ctx, id)
		}
	}

	return &ProviderStatus{
		Status:  "disabled",
		Message: "Cloudflare tunnel endpoint disabled",
	}, nil
}

func (p *CFTunnelProvider) Doctor(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	diag := map[string]string{}

	token := cfg.Config["tunnel_token"]
	if token == "" {
		return &ProviderStatus{
			Status:      "error",
			Message:     "Cloudflare tunnel token is invalid. Generate a new token from the CF dashboard.",
			Diagnostics: diag,
		}, nil
	}

	name := p.containerName(token)
	id, _, err := p.docker.FindContainerByLabel(ctx, "clank.cftunnel.name", name)
	if err != nil {
		diag["container_check"] = fmt.Sprintf("error: %v", err)
	} else if id == "" {
		diag["container_check"] = "not running"
		return &ProviderStatus{
			Status:      "error",
			Message:     fmt.Sprintf("Cloudflared container %s is not running", name),
			Diagnostics: diag,
		}, nil
	} else {
		diag["container_check"] = "running"
	}

	routeStatus, routeMessage, routeHTTP, routeErr := probeRoutedEndpoint(cfg.Hostname, cfg.PathPrefix, 10*time.Second)
	if routeErr != nil {
		diag["route_check"] = routeMessage
	} else {
		diag["route_check"] = fmt.Sprintf("%s (status %d)", routeStatus, routeHTTP)
	}

	publicURL := fmt.Sprintf("https://%s%s", cfg.Hostname, cfg.PathPrefix)
	publicStatus, publicMessage, publicHTTP, publicErr := probePublicURL(publicURL, 10*time.Second)
	if publicErr != nil {
		diag["public_check"] = publicMessage
	} else {
		diag["public_check"] = fmt.Sprintf("%s (status %d)", publicStatus, publicHTTP)
	}

	status := "active"
	message := fmt.Sprintf("Cloudflare tunnel healthy (%s)", name)
	verifiedBy := "public"
	if routeStatus != "healthy" {
		status = "degraded"
		message = fmt.Sprintf("Local route behind the Cloudflare tunnel is unhealthy (%s)", name)
		verifiedBy = "agent"
	} else if publicStatus != "healthy" {
		status = "degraded"
		message = "Cloudflare public endpoint is failing even though local routing works"
		verifiedBy = "agent"
	}

	return &ProviderStatus{
		Status:       status,
		Message:      message,
		ResolvedURL:  publicURL,
		VerifiedBy:   verifiedBy,
		RouteStatus:  routeStatus,
		PublicStatus: publicStatus,
		Diagnostics:  diag,
	}, nil
}
