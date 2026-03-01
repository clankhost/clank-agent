package endpoint

import "context"

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
	Status      string            // active | error | disabled | degraded
	Message     string
	ResolvedURL string
	VerifiedBy  string            // public | agent
	Diagnostics map[string]string
}

// Provider defines the interface for endpoint access providers.
type Provider interface {
	Name() string
	Ensure(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error)
	Disable(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error)
	Doctor(ctx context.Context, cfg ProviderConfig) (*ProviderStatus, error)
}
