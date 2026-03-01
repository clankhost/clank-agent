package endpoint

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// DirectProvider handles public_direct endpoints with Let's Encrypt HTTPS.
// Traefik labels (with certresolver=letsencrypt) are applied at deploy time.
// This provider checks DNS, port reachability, and cert status.
type DirectProvider struct{}

func (p *DirectProvider) Name() string { return "public_direct" }

func (p *DirectProvider) Ensure(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	if cfg.Hostname == "" {
		return &ProviderStatus{
			Status:  "error",
			Message: "Hostname is required for public_direct endpoints",
		}, nil
	}

	diag := map[string]string{}

	// DNS check
	addrs, err := net.LookupHost(cfg.Hostname)
	if err != nil {
		diag["dns"] = fmt.Sprintf("error: %v", err)
		return &ProviderStatus{
			Status:      "error",
			Message:     fmt.Sprintf("DNS record for %s doesn't resolve. Create an A record pointing to this server's public IP.", cfg.Hostname),
			VerifiedBy:  "agent",
			Diagnostics: diag,
		}, nil
	}
	diag["dns"] = fmt.Sprintf("resolved: %v", addrs)

	log.Printf("[direct] Endpoint %s: DNS OK for %s (%v), labels applied at deploy time", cfg.EndpointID, cfg.Hostname, addrs)

	return &ProviderStatus{
		Status:      "active",
		Message:     fmt.Sprintf("Public endpoint active at https://%s (Let's Encrypt)", cfg.Hostname),
		ResolvedURL: fmt.Sprintf("https://%s", cfg.Hostname),
		VerifiedBy:  "agent",
		Diagnostics: diag,
	}, nil
}

func (p *DirectProvider) Disable(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	log.Printf("[direct] Endpoint %s disabled (labels removed on next redeploy)", cfg.EndpointID)
	return &ProviderStatus{
		Status:  "disabled",
		Message: "Public direct endpoint disabled",
	}, nil
}

func (p *DirectProvider) Doctor(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error) {
	diag := map[string]string{}

	// DNS resolution
	addrs, err := net.LookupHost(cfg.Hostname)
	if err != nil {
		diag["dns"] = fmt.Sprintf("error: %v", err)
		return &ProviderStatus{
			Status:      "error",
			Message:     fmt.Sprintf("DNS record for %s doesn't resolve to this server. Create an A record pointing to your server's public IP.", cfg.Hostname),
			VerifiedBy:  "agent",
			Diagnostics: diag,
		}, nil
	}
	diag["dns"] = fmt.Sprintf("resolved: %v", addrs)

	// Port 80 check
	conn80, err := net.DialTimeout("tcp", cfg.Hostname+":80", 5*time.Second)
	if err != nil {
		diag["port_80"] = fmt.Sprintf("error: %v", err)
	} else {
		conn80.Close()
		diag["port_80"] = "ok"
	}

	// Port 443 check
	conn443, err := net.DialTimeout("tcp", cfg.Hostname+":443", 5*time.Second)
	if err != nil {
		diag["port_443"] = fmt.Sprintf("error: %v", err)
	} else {
		conn443.Close()
		diag["port_443"] = "ok"
	}

	// HTTPS check (best-effort)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("https://%s/", cfg.Hostname))
	if err != nil {
		diag["https_check"] = fmt.Sprintf("error: %v", err)
	} else {
		resp.Body.Close()
		diag["https_check"] = fmt.Sprintf("ok (status %d)", resp.StatusCode)
	}

	status := "active"
	message := fmt.Sprintf("Public endpoint healthy at https://%s", cfg.Hostname)
	verified := "public"

	if diag["port_80"] != "ok" {
		status = "degraded"
		message = "Port 80 is not reachable. Check your firewall settings."
	}
	if diag["port_443"] != "ok" {
		if status != "degraded" {
			status = "degraded"
		}
		message = "Port 443 is not reachable. Check your firewall settings."
	}
	if diag["https_check"] != "" && diag["https_check"][:2] != "ok" {
		status = "degraded"
		message = "Let's Encrypt certificate may not be issued yet. Ensure DNS resolves and ports 80/443 are open."
		verified = "agent"
	}

	return &ProviderStatus{
		Status:      status,
		Message:     message,
		ResolvedURL: fmt.Sprintf("https://%s", cfg.Hostname),
		VerifiedBy:  verified,
		Diagnostics: diag,
	}, nil
}
