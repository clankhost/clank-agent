package endpoint

import (
	"context"
	"fmt"
	"log"
)

// Manager routes endpoint commands to the appropriate provider.
type Manager struct {
	providers map[string]Provider
}

// NewManager creates an endpoint manager with the given providers.
func NewManager(providers ...Provider) *Manager {
	m := &Manager{providers: make(map[string]Provider)}
	for _, p := range providers {
		m.providers[p.Name()] = p
	}
	return m
}

// Get returns the provider for the given name, or nil if not found.
func (m *Manager) Get(name string) Provider {
	return m.providers[name]
}

// HandleEnsure dispatches an ENSURE action to the appropriate provider.
func (m *Manager) HandleEnsure(ctx context.Context, cfg ProviderConfig, providerName string) (*ProviderStatus, error) {
	p := m.providers[providerName]
	if p == nil {
		return nil, fmt.Errorf("unknown endpoint provider: %s", providerName)
	}
	log.Printf("[endpoint] ENSURE %s endpoint %s (host=%s, slug=%s)", providerName, cfg.EndpointID, cfg.Hostname, cfg.ServiceSlug)
	return p.Ensure(ctx, cfg)
}

// HandleDisable dispatches a DISABLE action to the appropriate provider.
func (m *Manager) HandleDisable(ctx context.Context, cfg ProviderConfig, providerName string) (*ProviderStatus, error) {
	p := m.providers[providerName]
	if p == nil {
		return nil, fmt.Errorf("unknown endpoint provider: %s", providerName)
	}
	log.Printf("[endpoint] DISABLE %s endpoint %s", providerName, cfg.EndpointID)
	return p.Disable(ctx, cfg)
}

// HandleDoctor dispatches a DOCTOR action to the appropriate provider.
func (m *Manager) HandleDoctor(ctx context.Context, cfg ProviderConfig, providerName string) (*ProviderStatus, error) {
	p := m.providers[providerName]
	if p == nil {
		return nil, fmt.Errorf("unknown endpoint provider: %s", providerName)
	}
	log.Printf("[endpoint] DOCTOR %s endpoint %s", providerName, cfg.EndpointID)
	return p.Doctor(ctx, cfg)
}
