package deploy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSelectTraefikProbeTarget(t *testing.T) {
	tests := []struct {
		name       string
		opts       DeployOpts
		healthPath string
		wantHost   string
		wantPath   string
	}{
		{
			name:       "no endpoints or domains returns empty",
			opts:       DeployOpts{ServiceSlug: "api"},
			healthPath: "/health",
			wantHost:   "",
			wantPath:   "",
		},
		{
			name: "prefers direct web proxy endpoint over public_direct",
			opts: DeployOpts{
				Endpoints: []EndpointInfo{
					{Provider: "public_direct", Hostname: "direct.example.com"},
					{Provider: "public_tunnel_cloudflare", Hostname: "app.example.com"},
				},
			},
			healthPath: "/health",
			wantHost:   "app.example.com",
			wantPath:   "/health",
		},
		{
			name: "uses public_direct when only endpoint",
			opts: DeployOpts{
				Endpoints: []EndpointInfo{
					{Provider: "public_direct", Hostname: "direct.example.com"},
				},
			},
			healthPath: "/healthz",
			wantHost:   "direct.example.com",
			wantPath:   "/healthz",
		},
		{
			name: "tailscale path prefix is prepended",
			opts: DeployOpts{
				Endpoints: []EndpointInfo{
					{Provider: "private_tailscale_https", Hostname: "box.tailnet.ts.net", PathPrefix: "/api"},
				},
			},
			healthPath: "/health",
			wantHost:   "box.tailnet.ts.net",
			wantPath:   "/api/health",
		},
		{
			name: "falls back to legacy domains",
			opts: DeployOpts{
				Domains: []string{"legacy.example.com"},
			},
			healthPath: "",
			wantHost:   "legacy.example.com",
			wantPath:   "/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotHost, gotPath := selectTraefikProbeTarget(tt.opts, tt.healthPath)
			if gotHost != tt.wantHost || gotPath != tt.wantPath {
				t.Fatalf("selectTraefikProbeTarget = (%q, %q), want (%q, %q)", gotHost, gotPath, tt.wantHost, tt.wantPath)
			}
		})
	}
}

func TestWaitForTraefikReady_SucceedsAfterInitial404s(t *testing.T) {
	var hits atomic.Int32
	var seenHost atomic.Value
	var seenPath atomic.Value

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHost.Store(r.Host)
		seenPath.Store(r.URL.Path)
		n := hits.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &Deployer{
		traefikEndpoint:     srv.URL,
		traefikReadyTimeout: 2 * time.Second,
		traefikPollInterval: 20 * time.Millisecond,
	}

	opts := DeployOpts{
		ServiceSlug: "web",
		Endpoints: []EndpointInfo{
			{Provider: "public_tunnel_cloudflare", Hostname: "web.example.com"},
		},
	}

	start := time.Now()
	d.waitForTraefikReady(context.Background(), opts, "/health", nil)
	elapsed := time.Since(start)

	if hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3", hits.Load())
	}
	if got, _ := seenHost.Load().(string); got != "web.example.com" {
		t.Fatalf("Host header = %q, want web.example.com", got)
	}
	if got, _ := seenPath.Load().(string); got != "/health" {
		t.Fatalf("path = %q, want /health", got)
	}
	if elapsed > time.Second {
		t.Fatalf("took too long: %v", elapsed)
	}
}

func TestWaitForTraefikReady_AcceptsRedirectFromPublicDirect(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", "https://app.example.com/health")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer srv.Close()

	d := &Deployer{
		traefikEndpoint:     srv.URL,
		traefikReadyTimeout: 2 * time.Second,
		traefikPollInterval: 20 * time.Millisecond,
	}

	d.waitForTraefikReady(context.Background(), DeployOpts{
		Endpoints: []EndpointInfo{{Provider: "public_direct", Hostname: "app.example.com"}},
	}, "/health", nil)

	if hits.Load() != 1 {
		t.Fatalf("expected immediate success on 301 redirect, got %d hits", hits.Load())
	}
}

func TestWaitForTraefikReady_GracefulFallbackWhenNoHostname(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &Deployer{
		traefikEndpoint:     srv.URL,
		traefikReadyTimeout: 2 * time.Second,
		traefikPollInterval: 20 * time.Millisecond,
	}

	d.waitForTraefikReady(context.Background(), DeployOpts{ServiceSlug: "internal"}, "/health", nil)
	if hits.Load() != 0 {
		t.Fatalf("expected 0 probe requests when no hostname is configured, got %d", hits.Load())
	}
}

func TestWaitForTraefikReady_GracefulFallbackWhenPortUnreachable(t *testing.T) {
	// Reserve and immediately close a local port so dialing gets connection refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	d := &Deployer{
		traefikEndpoint:     "http://" + addr,
		traefikReadyTimeout: 5 * time.Second,
		traefikPollInterval: 20 * time.Millisecond,
	}

	start := time.Now()
	d.waitForTraefikReady(context.Background(), DeployOpts{
		Domains: []string{"app.example.com"},
	}, "/health", nil)
	elapsed := time.Since(start)

	// Should abort quickly after 2 dial errors instead of burning the full 5s timeout.
	if elapsed > 2*time.Second {
		t.Fatalf("unreachable Traefik took %v, expected fast fallback", elapsed)
	}
}

func TestWaitForTraefikReady_BoundedTimeoutFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	d := &Deployer{
		traefikEndpoint:     srv.URL,
		traefikReadyTimeout: 120 * time.Millisecond,
		traefikPollInterval: 25 * time.Millisecond,
	}

	start := time.Now()
	d.waitForTraefikReady(context.Background(), DeployOpts{
		Domains: []string{"app.example.com"},
	}, "/health", nil)
	elapsed := time.Since(start)

	if elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Fatalf("bounded timeout elapsed = %v, want ~120ms", elapsed)
	}
}
