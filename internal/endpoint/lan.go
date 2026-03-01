package endpoint

import (
	"context"
	"fmt"
	"log"
	"net/http"
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

	// Check if we can reach the service via Traefik locally
	url := fmt.Sprintf("http://localhost:%d/", 80)
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(url)
	if err != nil {
		diag["traefik_reachable"] = fmt.Sprintf("error: %v", err)
	} else {
		resp.Body.Close()
		diag["traefik_reachable"] = "ok"
	}

	status := "active"
	message := "LAN endpoint healthy"
	if diag["traefik_reachable"] != "ok" {
		status = "degraded"
		message = "Traefik not reachable on port 80"
	}

	return &ProviderStatus{
		Status:      status,
		Message:     message,
		VerifiedBy:  "agent",
		Diagnostics: diag,
	}, nil
}
