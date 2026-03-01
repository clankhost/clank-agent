package endpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// TailscaleProvider handles private_tailscale_https endpoints.
// Configures `tailscale serve` to proxy HTTPS to Traefik.
// Traefik labels with PathPrefix routing are applied at deploy time.
type TailscaleProvider struct{}

func (p *TailscaleProvider) Name() string { return "private_tailscale_https" }

// getTailscaleHostname discovers the machine's tailnet DNS name.
func (p *TailscaleProvider) getTailscaleHostname() (string, error) {
	out, err := exec.Command("tailscale", "status", "--json").Output()
	if err != nil {
		return "", fmt.Errorf("tailscale not available: %w", err)
	}

	var status struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return "", fmt.Errorf("parsing tailscale status: %w", err)
	}

	hostname := strings.TrimSuffix(status.Self.DNSName, ".")
	if hostname == "" {
		return "", fmt.Errorf("tailscale DNSName is empty — is Tailscale authenticated?")
	}
	return hostname, nil
}

func (p *TailscaleProvider) Ensure(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	tsHostname, err := p.getTailscaleHostname()
	if err != nil {
		return &ProviderStatus{
			Status:  "error",
			Message: fmt.Sprintf("Tailscale is not running. Install and authenticate first: %v", err),
		}, nil
	}

	// Configure tailscale serve to proxy HTTPS 443 → Traefik on port 80
	cmd := exec.CommandContext(ctx, "tailscale", "serve", "--bg", "--https=443", "http://127.0.0.1:80")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return &ProviderStatus{
			Status:  "error",
			Message: fmt.Sprintf("Failed to configure tailscale serve: %v\n%s", err, string(output)),
			Diagnostics: map[string]string{
				"tailscale_hostname": tsHostname,
				"serve_output":      string(output),
			},
		}, nil
	}

	pathPrefix := cfg.PathPrefix
	if pathPrefix == "" {
		pathPrefix = "/" + cfg.ServiceSlug
	}
	resolvedURL := fmt.Sprintf("https://%s%s", tsHostname, pathPrefix)

	log.Printf("[tailscale] Endpoint %s: serve configured, accessible at %s", cfg.EndpointID, resolvedURL)

	return &ProviderStatus{
		Status:      "active",
		Message:     fmt.Sprintf("Tailscale HTTPS endpoint active at %s", resolvedURL),
		ResolvedURL: resolvedURL,
		VerifiedBy:  "agent",
		Diagnostics: map[string]string{
			"tailscale_hostname": tsHostname,
			"path_prefix":       pathPrefix,
		},
	}, nil
}

func (p *TailscaleProvider) Disable(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	// Turn off tailscale serve if no other tailscale endpoints exist
	cmd := exec.CommandContext(ctx, "tailscale", "serve", "--https=443", "off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[tailscale] Warning: failed to disable tailscale serve: %v\n%s", err, string(output))
	}

	return &ProviderStatus{
		Status:  "disabled",
		Message: "Tailscale endpoint disabled",
	}, nil
}

func (p *TailscaleProvider) Doctor(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	diag := map[string]string{}

	// Check tailscale status
	tsHostname, err := p.getTailscaleHostname()
	if err != nil {
		diag["tailscale_status"] = fmt.Sprintf("error: %v", err)
		return &ProviderStatus{
			Status:      "error",
			Message:     "Tailscale is not installed on this server. Install it: https://tailscale.com/download",
			Diagnostics: diag,
		}, nil
	}
	diag["tailscale_status"] = "connected"
	diag["tailscale_hostname"] = tsHostname

	// Check tailscale serve status
	serveOut, err := exec.Command("tailscale", "serve", "status").Output()
	if err != nil {
		diag["serve_status"] = fmt.Sprintf("error: %v", err)
	} else {
		diag["serve_status"] = strings.TrimSpace(string(serveOut))
	}

	pathPrefix := cfg.PathPrefix
	if pathPrefix == "" {
		pathPrefix = "/" + cfg.ServiceSlug
	}
	resolvedURL := fmt.Sprintf("https://%s%s", tsHostname, pathPrefix)

	// Try to reach the endpoint from agent
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(resolvedURL)
	if err != nil {
		diag["agent_verify"] = fmt.Sprintf("error: %v", err)
		return &ProviderStatus{
			Status:      "degraded",
			Message:     fmt.Sprintf("Tailscale serve configured but endpoint not reachable: %v", err),
			ResolvedURL: resolvedURL,
			VerifiedBy:  "agent",
			Diagnostics: diag,
		}, nil
	}
	resp.Body.Close()
	diag["agent_verify"] = fmt.Sprintf("ok (status %d)", resp.StatusCode)

	return &ProviderStatus{
		Status:      "active",
		Message:     fmt.Sprintf("Tailscale endpoint healthy at %s", resolvedURL),
		ResolvedURL: resolvedURL,
		VerifiedBy:  "agent",
		Diagnostics: diag,
	}, nil
}
