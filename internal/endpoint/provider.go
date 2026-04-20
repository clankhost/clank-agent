package endpoint

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// ProviderConfig holds configuration for an endpoint provider action.
type ProviderConfig struct {
	EndpointID  string
	ServiceSlug string
	Hostname    string
	PathPrefix  string
	Port        int
	TLSMode     string
	Config      map[string]string // decrypted provider-specific config
}

// ProviderStatus is the result of a provider action.
type ProviderStatus struct {
	Status       string // active | error | disabled | degraded
	Message      string
	ResolvedURL  string
	VerifiedBy   string // public | agent
	RouteStatus  string
	PublicStatus string
	Diagnostics  map[string]string
}

// Provider defines the interface for endpoint access providers.
type Provider interface {
	Name() string
	Ensure(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error)
	Disable(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error)
	Doctor(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error)
}

func localTraefikCheck(hostname, pathPrefix string, timeout time.Duration) (*http.Response, error) {
	if pathPrefix == "" {
		pathPrefix = "/"
	}
	url := fmt.Sprintf("http://127.0.0.1:80%s", pathPrefix)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Host = hostname
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client.Do(req)
}

func probeRoutedEndpoint(hostname, pathPrefix string, timeout time.Duration) (string, string, int, error) {
	resp, err := localTraefikCheck(hostname, pathPrefix, timeout)
	if err != nil {
		return "degraded", fmt.Sprintf("local route probe failed: %v", err), 0, err
	}
	defer resp.Body.Close()

	if (resp.StatusCode >= 200 && resp.StatusCode < 400) || resp.StatusCode == 401 || resp.StatusCode == 403 {
		return "healthy", fmt.Sprintf("local route returned HTTP %d", resp.StatusCode), resp.StatusCode, nil
	}
	return "degraded", fmt.Sprintf("local route returned HTTP %d", resp.StatusCode), resp.StatusCode, nil
}

func probePublicURL(url string, timeout time.Duration) (string, string, int, error) {
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return "degraded", fmt.Sprintf("public probe failed: %v", err), 0, err
	}
	defer resp.Body.Close()

	if (resp.StatusCode >= 200 && resp.StatusCode < 400) || resp.StatusCode == 401 || resp.StatusCode == 403 {
		return "healthy", fmt.Sprintf("public probe returned HTTP %d", resp.StatusCode), resp.StatusCode, nil
	}
	return "degraded", fmt.Sprintf("public probe returned HTTP %d", resp.StatusCode), resp.StatusCode, nil
}
