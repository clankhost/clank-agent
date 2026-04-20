package endpoint

import (
	"context"
	"fmt"
	"log"
	"time"
)

// LANProvider handles lan_only endpoints.
// Labels are applied at deploy time via active_endpoints in DeployCommand.
// This provider just reports status and runs diagnostics.
type LANProvider struct{}

func (p *LANProvider) Name() string { return "lan_only" }

func (p *LANProvider) Ensure(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	hostname := cfg.Hostname
	if hostname == "" {
		hostname = cfg.ServiceSlug + ".local"
	}

	log.Printf("[lan] Endpoint %s: LAN-only at %s (labels applied at deploy time)", cfg.EndpointID, hostname)

	return &ProviderStatus{
		Status:      "active",
		Message:     fmt.Sprintf("LAN endpoint active at http://%s", hostname),
		ResolvedURL: fmt.Sprintf("http://%s", hostname),
		VerifiedBy:  "agent",
	}, nil
}

func (p *LANProvider) Disable(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	log.Printf("[lan] Endpoint %s disabled (labels removed on next redeploy)", cfg.EndpointID)
	return &ProviderStatus{
		Status:  "disabled",
		Message: "LAN endpoint disabled",
	}, nil
}

func (p *LANProvider) Doctor(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	diag := map[string]string{}
	hostname := cfg.Hostname
	if hostname == "" {
		hostname = cfg.ServiceSlug + ".local"
	}
	routeStatus, routeMessage, routeHTTP, routeErr := probeRoutedEndpoint(hostname, cfg.PathPrefix, 5*time.Second)
	if routeErr != nil {
		diag["route_check"] = routeMessage
	} else {
		diag["route_check"] = fmt.Sprintf("%s (status %d)", routeStatus, routeHTTP)
	}

	status := "active"
	message := "LAN endpoint healthy"
	if routeStatus != "healthy" {
		status = "degraded"
		message = "LAN route is not reachable through Traefik"
	}

	return &ProviderStatus{
		Status:       status,
		Message:      message,
		VerifiedBy:   "agent",
		RouteStatus:  routeStatus,
		PublicStatus: "not_applicable",
		Diagnostics:  diag,
	}, nil
}
