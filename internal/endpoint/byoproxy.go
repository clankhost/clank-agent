package endpoint

import (
	"context"
	"fmt"
	"log"
)

// BYOProxyProvider handles byo_proxy endpoints.
// The user manages their own reverse proxy (nginx, caddy, etc.).
// Traefik labels are applied at deploy time — user's proxy forwards to Traefik port 80.
type BYOProxyProvider struct{}

func (p *BYOProxyProvider) Name() string { return "byo_proxy" }

func (p *BYOProxyProvider) Ensure(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	if cfg.Hostname == "" {
		return &ProviderStatus{
			Status:  "error",
			Message: "Hostname is required for byo_proxy endpoints",
		}, nil
	}

	msg := fmt.Sprintf("Configure your reverse proxy to forward %s to http://<server-ip>:80", cfg.Hostname)
	log.Printf("[byo_proxy] Endpoint %s: %s", cfg.EndpointID, msg)

	return &ProviderStatus{
		Status:      "active",
		Message:     msg,
		ResolvedURL: fmt.Sprintf("http://%s", cfg.Hostname),
		VerifiedBy:  "agent",
	}, nil
}

func (p *BYOProxyProvider) Disable(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	return &ProviderStatus{
		Status:  "disabled",
		Message: "BYO proxy endpoint disabled",
	}, nil
}

func (p *BYOProxyProvider) Doctor(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	// Cannot verify external proxy — opaque to us
	return &ProviderStatus{
		Status:      "active",
		Message:     fmt.Sprintf("BYO proxy endpoint configured for %s. Verify your proxy is forwarding to http://<server-ip>:80", cfg.Hostname),
		VerifiedBy:  "agent",
		Diagnostics: map[string]string{"note": "Cannot verify external proxy configuration"},
	}, nil
}
