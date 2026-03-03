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

// localTraefikCheck verifies service reachability by hitting Traefik on
// localhost:80 with the Host header set to the Tailscale hostname.
// This avoids the TLS certificate mismatch that happens when the agent
// tries to reach the external HTTPS URL from the same machine — the
// request would bypass Tailscale Serve and hit Traefik's port 443
// directly, where Traefik presents its self-signed default cert instead
// of the Tailscale-issued cert.
func localTraefikCheck(hostname, pathPrefix string, timeout time.Duration) (*http.Response, error) {
	url := fmt.Sprintf("http://127.0.0.1:80%s", pathPrefix)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Host = hostname
	client := &http.Client{Timeout: timeout}
	return client.Do(req)
}

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

	log.Printf("[tailscale] Endpoint %s: serve configured, checking reachability via Traefik at http://127.0.0.1:80%s (Host: %s)", cfg.EndpointID, pathPrefix, tsHostname)

	// Check reachability by hitting Traefik directly on localhost:80 with the
	// Host header set to the Tailscale hostname. We can't use the external
	// https:// URL from the same machine because it would bypass Tailscale
	// Serve and hit Traefik's port 443 directly, causing a TLS cert mismatch
	// (Traefik's self-signed cert vs the Tailscale hostname).
	resp, err := localTraefikCheck(tsHostname, pathPrefix, 5*time.Second)
	if err != nil {
		log.Printf("[tailscale] Endpoint %s: serve configured but not yet reachable: %v", cfg.EndpointID, err)
		return &ProviderStatus{
			Status:      "provisioning",
			Message:     fmt.Sprintf("Tailscale Serve configured. Waiting for service to become reachable at %s", resolvedURL),
			ResolvedURL: resolvedURL,
			VerifiedBy:  "agent",
			Diagnostics: map[string]string{
				"tailscale_hostname": tsHostname,
				"path_prefix":       pathPrefix,
			},
		}, nil
	}
	resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return &ProviderStatus{
			Status:      "active",
			Message:     fmt.Sprintf("Tailscale HTTPS endpoint active at %s", resolvedURL),
			ResolvedURL: resolvedURL,
			VerifiedBy:  "agent",
			Diagnostics: map[string]string{
				"tailscale_hostname": tsHostname,
				"path_prefix":       pathPrefix,
				"http_status":       fmt.Sprintf("%d", resp.StatusCode),
			},
		}, nil
	}

	// Got a response but it's an error (404, 502, etc.) — service not ready
	return &ProviderStatus{
		Status:      "provisioning",
		Message:     fmt.Sprintf("Tailscale Serve configured but service returned HTTP %d. Deploy the service and try again.", resp.StatusCode),
		ResolvedURL: resolvedURL,
		VerifiedBy:  "agent",
		Diagnostics: map[string]string{
			"tailscale_hostname": tsHostname,
			"path_prefix":       pathPrefix,
			"http_status":       fmt.Sprintf("%d", resp.StatusCode),
		},
	}, nil
}

func (p *TailscaleProvider) Disable(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	// tailscale serve proxies ALL HTTPS to Traefik on :80. Individual service
	// routes are isolated by Traefik path-based routing, so we don't turn off
	// tailscale serve here — other services may still need it. The Traefik
	// labels for this service are removed on redeploy.
	log.Printf("[tailscale] Endpoint %s disabled (path route removed on redeploy)", cfg.EndpointID)
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

	// Check reachability by hitting Traefik directly on localhost:80 with the
	// Host header. See comment in Ensure() for why we can't use the external URL.
	resp, err := localTraefikCheck(tsHostname, pathPrefix, 10*time.Second)
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
	diag["agent_verify"] = fmt.Sprintf("status %d", resp.StatusCode)

	// 2xx/3xx = service is healthy; 401/403 = service is up but requires auth (still active)
	if resp.StatusCode >= 200 && resp.StatusCode < 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		return &ProviderStatus{
			Status:      "active",
			Message:     fmt.Sprintf("Tailscale endpoint healthy at %s (HTTP %d)", resolvedURL, resp.StatusCode),
			ResolvedURL: resolvedURL,
			VerifiedBy:  "agent",
			Diagnostics: diag,
		}, nil
	}

	// 404/502/503/504 = Traefik is responding but service container isn't routable
	return &ProviderStatus{
		Status:      "degraded",
		Message:     fmt.Sprintf("Tailscale Serve is working but service returned HTTP %d. Is the service deployed?", resp.StatusCode),
		ResolvedURL: resolvedURL,
		VerifiedBy:  "agent",
		Diagnostics: diag,
	}, nil
}
