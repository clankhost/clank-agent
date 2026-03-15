package deploy

import (
	"strings"
	"testing"

	"github.com/anaremore/clank/apps/agent/internal/docker"
)

// ── generateLegacyLabels ────────────────────────────────────────────────

func TestGenerateLegacyLabels_LocalhostDefault(t *testing.T) {
	labels := make(map[string]string)
	generateLegacyLabels(labels, "myapp", nil, nil)

	rule := labels["traefik.http.routers.clank-myapp.rule"]
	if rule == "" {
		t.Fatal("expected router rule to be set")
	}
	if !strings.Contains(rule, "Host(`myapp.localhost`)") {
		t.Errorf("expected localhost Host rule, got %q", rule)
	}
	assertLabel(t, labels, "traefik.http.routers.clank-myapp.entrypoints", "web")
}

func TestGenerateLegacyLabels_LANIPsSslip(t *testing.T) {
	labels := make(map[string]string)
	generateLegacyLabels(labels, "wordpress", nil, []string{"192.168.1.10"})

	rule := labels["traefik.http.routers.clank-wordpress.rule"]
	if !strings.Contains(rule, "Host(`wordpress.192.168.1.10.sslip.io`)") {
		t.Errorf("expected sslip.io Host rule for LAN IP, got %q", rule)
	}
	if !strings.Contains(rule, "Host(`wordpress.localhost`)") {
		t.Errorf("expected localhost fallback in rules, got %q", rule)
	}
}

func TestGenerateLegacyLabels_MultipleLANIPs(t *testing.T) {
	labels := make(map[string]string)
	generateLegacyLabels(labels, "app", nil, []string{"10.0.0.1", "172.16.0.5"})

	rule := labels["traefik.http.routers.clank-app.rule"]
	if !strings.Contains(rule, "Host(`app.10.0.0.1.sslip.io`)") {
		t.Errorf("missing first LAN IP sslip.io rule in %q", rule)
	}
	if !strings.Contains(rule, "Host(`app.172.16.0.5.sslip.io`)") {
		t.Errorf("missing second LAN IP sslip.io rule in %q", rule)
	}
	// All rules joined with ||
	if strings.Count(rule, "||") < 2 {
		t.Errorf("expected at least 3 rules (localhost + 2 IPs) joined with ||, got %q", rule)
	}
}

func TestGenerateLegacyLabels_DomainsPassedThrough(t *testing.T) {
	labels := make(map[string]string)
	generateLegacyLabels(labels, "blog", []string{"blog.example.com"}, nil)

	rule := labels["traefik.http.routers.clank-blog.rule"]
	if !strings.Contains(rule, "Host(`blog.example.com`)") {
		t.Errorf("expected explicit domain in rules, got %q", rule)
	}
}

func TestGenerateLegacyLabels_UnsafeDomainRejected(t *testing.T) {
	labels := make(map[string]string)
	generateLegacyLabels(labels, "app", []string{"evil<script>.com"}, nil)

	rule := labels["traefik.http.routers.clank-app.rule"]
	if strings.Contains(rule, "evil") {
		t.Errorf("unsafe domain should have been rejected, got %q", rule)
	}
	// Should fall back to localhost
	if !strings.Contains(rule, "Host(`app.localhost`)") {
		t.Errorf("expected localhost fallback after domain rejection, got %q", rule)
	}
}

func TestGenerateLegacyLabels_HTTPSRedirect(t *testing.T) {
	labels := make(map[string]string)
	generateLegacyLabels(labels, "myapp", nil, []string{"192.168.1.10"})

	// HTTPS redirect router exists
	assertLabel(t, labels, "traefik.http.routers.clank-myapp-https-redir.entrypoints", "websecure")
	assertLabel(t, labels, "traefik.http.routers.clank-myapp-https-redir.tls", "true")
	assertLabel(t, labels, "traefik.http.routers.clank-myapp-https-redir.priority", "1")
	assertLabel(t, labels, "traefik.http.routers.clank-myapp-https-redir.middlewares", "clank-myapp-to-http")

	// Middleware does the redirect
	assertLabel(t, labels, "traefik.http.middlewares.clank-myapp-to-http.redirectscheme.scheme", "http")
	assertLabel(t, labels, "traefik.http.middlewares.clank-myapp-to-http.redirectscheme.permanent", "false")

	// HTTPS redirect rule matches the same hosts as the HTTP router
	httpRule := labels["traefik.http.routers.clank-myapp.rule"]
	httpsRule := labels["traefik.http.routers.clank-myapp-https-redir.rule"]
	if httpRule != httpsRule {
		t.Errorf("HTTPS redirect rule should match HTTP rule\n  HTTP:  %s\n  HTTPS: %s", httpRule, httpsRule)
	}
}

func TestGenerateLegacyLabels_HTTPSRedirectNoLANIPs(t *testing.T) {
	// Even without LAN IPs, HTTPS redirect labels should still be generated
	// (for the localhost rule)
	labels := make(map[string]string)
	generateLegacyLabels(labels, "myapp", nil, nil)

	assertLabel(t, labels, "traefik.http.routers.clank-myapp-https-redir.entrypoints", "websecure")
	assertLabel(t, labels, "traefik.http.middlewares.clank-myapp-to-http.redirectscheme.scheme", "http")
}

// ── generateEndpointLabels ──────────────────────────────────────────────

func TestEndpointLabels_PublicDirect(t *testing.T) {
	labels := make(map[string]string)
	eps := []EndpointInfo{
		{EndpointID: "ep1", Provider: "public_direct", Hostname: "myapp.example.com", TLSMode: "lets_encrypt_http01"},
	}
	generateEndpointLabels(labels, "myapp", 8080, eps)

	// Secure router with Let's Encrypt
	assertLabel(t, labels, "traefik.http.routers.clank-myapp-ep0-secure.rule", "Host(`myapp.example.com`)")
	assertLabel(t, labels, "traefik.http.routers.clank-myapp-ep0-secure.entrypoints", "websecure")
	assertLabel(t, labels, "traefik.http.routers.clank-myapp-ep0-secure.tls.certresolver", "letsencrypt")
	assertLabel(t, labels, "traefik.http.routers.clank-myapp-ep0-secure.service", "clank-myapp")

	// HTTP → HTTPS redirect
	assertLabel(t, labels, "traefik.http.routers.clank-myapp-ep0-http.rule", "Host(`myapp.example.com`)")
	assertLabel(t, labels, "traefik.http.routers.clank-myapp-ep0-http.entrypoints", "web")
	assertLabel(t, labels, "traefik.http.routers.clank-myapp-ep0-http.middlewares", "clank-myapp-ep0-redirect")
	assertLabel(t, labels, "traefik.http.middlewares.clank-myapp-ep0-redirect.redirectscheme.scheme", "https")
}

func TestEndpointLabels_CloudflareTunnel(t *testing.T) {
	labels := make(map[string]string)
	eps := []EndpointInfo{
		{EndpointID: "ep1", Provider: "public_tunnel_cloudflare", Hostname: "app.raiseculture.com", TLSMode: "cloudflare_edge"},
	}
	generateEndpointLabels(labels, "webapp", 3000, eps)

	// CF terminates TLS, so just web entrypoint
	assertLabel(t, labels, "traefik.http.routers.clank-webapp-ep0.rule", "Host(`app.raiseculture.com`)")
	assertLabel(t, labels, "traefik.http.routers.clank-webapp-ep0.entrypoints", "web")
	assertLabel(t, labels, "traefik.http.routers.clank-webapp-ep0.service", "clank-webapp")

	// Should NOT have websecure/TLS labels
	if _, ok := labels["traefik.http.routers.clank-webapp-ep0.tls.certresolver"]; ok {
		t.Error("CF tunnel endpoints should not have certresolver")
	}
}

func TestEndpointLabels_TailscaleHTTPS(t *testing.T) {
	labels := make(map[string]string)
	eps := []EndpointInfo{
		{EndpointID: "ep1", Provider: "private_tailscale_https", Hostname: "mynode.tail1234.ts.net", TLSMode: "tailscale_https"},
	}
	generateEndpointLabels(labels, "n8n", 5678, eps)

	// Tailscale routes through web entrypoint (Tailscale terminates TLS externally)
	assertLabel(t, labels, "traefik.http.routers.clank-n8n-ep0.rule", "Host(`mynode.tail1234.ts.net`)")
	assertLabel(t, labels, "traefik.http.routers.clank-n8n-ep0.entrypoints", "web")
	assertLabel(t, labels, "traefik.http.routers.clank-n8n-ep0.service", "clank-n8n")
}

func TestEndpointLabels_TailscaleWithPathPrefix(t *testing.T) {
	labels := make(map[string]string)
	eps := []EndpointInfo{
		{EndpointID: "ep1", Provider: "private_tailscale_https", Hostname: "mynode.tail1234.ts.net", PathPrefix: "/n8n", TLSMode: "tailscale_https"},
	}
	generateEndpointLabels(labels, "n8n", 5678, eps)

	// Rule includes PathPrefix
	assertLabel(t, labels, "traefik.http.routers.clank-n8n-ep0.rule", "Host(`mynode.tail1234.ts.net`) && PathPrefix(`/n8n`)")

	// StripPrefix middleware
	assertLabel(t, labels, "traefik.http.routers.clank-n8n-ep0.middlewares", "clank-n8n-ep0-strip")
	assertLabel(t, labels, "traefik.http.middlewares.clank-n8n-ep0-strip.stripprefix.prefixes", "/n8n")
}

func TestEndpointLabels_LANOnly(t *testing.T) {
	labels := make(map[string]string)
	eps := []EndpointInfo{
		{EndpointID: "ep1", Provider: "lan_only", Hostname: "wordpress.192.168.1.5.sslip.io", TLSMode: "off"},
	}
	generateEndpointLabels(labels, "wordpress", 80, eps)

	assertLabel(t, labels, "traefik.http.routers.clank-wordpress-ep0.rule", "Host(`wordpress.192.168.1.5.sslip.io`)")
	assertLabel(t, labels, "traefik.http.routers.clank-wordpress-ep0.entrypoints", "web")
	assertLabel(t, labels, "traefik.http.routers.clank-wordpress-ep0.service", "clank-wordpress")

	// No TLS-related labels
	if _, ok := labels["traefik.http.routers.clank-wordpress-ep0.tls.certresolver"]; ok {
		t.Error("LAN-only endpoints should not have certresolver")
	}
}

func TestEndpointLabels_BYOProxy(t *testing.T) {
	labels := make(map[string]string)
	eps := []EndpointInfo{
		{EndpointID: "ep1", Provider: "byo_proxy", Hostname: "app.mycompany.internal", TLSMode: "off"},
	}
	generateEndpointLabels(labels, "app", 3000, eps)

	assertLabel(t, labels, "traefik.http.routers.clank-app-ep0.rule", "Host(`app.mycompany.internal`)")
	assertLabel(t, labels, "traefik.http.routers.clank-app-ep0.entrypoints", "web")
	assertLabel(t, labels, "traefik.http.routers.clank-app-ep0.service", "clank-app")
}

func TestEndpointLabels_MultipleEndpoints(t *testing.T) {
	labels := make(map[string]string)
	eps := []EndpointInfo{
		{EndpointID: "ep1", Provider: "public_direct", Hostname: "app.example.com", TLSMode: "lets_encrypt_http01"},
		{EndpointID: "ep2", Provider: "lan_only", Hostname: "app.10.0.0.1.sslip.io", TLSMode: "off"},
	}
	generateEndpointLabels(labels, "app", 80, eps)

	// First endpoint: ep0 (public_direct)
	if _, ok := labels["traefik.http.routers.clank-app-ep0-secure.rule"]; !ok {
		t.Error("expected ep0 secure router for public_direct")
	}

	// Second endpoint: ep1 (lan_only)
	assertLabel(t, labels, "traefik.http.routers.clank-app-ep1.rule", "Host(`app.10.0.0.1.sslip.io`)")
	assertLabel(t, labels, "traefik.http.routers.clank-app-ep1.entrypoints", "web")
}

func TestEndpointLabels_EmptyHostnameSkipped(t *testing.T) {
	labels := make(map[string]string)
	eps := []EndpointInfo{
		{EndpointID: "ep1", Provider: "public_direct", Hostname: "", TLSMode: "lets_encrypt_http01"},
	}
	generateEndpointLabels(labels, "app", 80, eps)

	// No router should be created for empty hostname
	for k := range labels {
		if strings.Contains(k, "router") {
			t.Errorf("no router labels expected for empty hostname, found %s", k)
		}
	}
}

func TestEndpointLabels_UnsafeHostnameSkipped(t *testing.T) {
	labels := make(map[string]string)
	eps := []EndpointInfo{
		{EndpointID: "ep1", Provider: "public_direct", Hostname: "bad<host>.com", TLSMode: "lets_encrypt_http01"},
	}
	generateEndpointLabels(labels, "app", 80, eps)

	for k := range labels {
		if strings.Contains(k, "router") {
			t.Errorf("no router labels expected for unsafe hostname, found %s", k)
		}
	}
}

// ── generateTraefikLabels (orchestrator) ────────────────────────────────

func TestGenerateTraefikLabels_HTTPService(t *testing.T) {
	labels := generateTraefikLabels("deploy-123", "myapp", nil, 8080, nil, []string{"10.0.0.5"}, true)

	assertLabel(t, labels, "traefik.enable", "true")
	assertLabel(t, labels, "clank.managed", "true")
	assertLabel(t, labels, "clank.service_slug", "myapp")
	assertLabel(t, labels, "clank.deployment_id", "deploy-123")
	assertLabel(t, labels, "traefik.http.services.clank-myapp.loadbalancer.server.port", "8080")

	// Legacy labels should exist
	if _, ok := labels["traefik.http.routers.clank-myapp.rule"]; !ok {
		t.Error("expected legacy router rule")
	}
}

func TestGenerateTraefikLabels_NonHTTPService(t *testing.T) {
	labels := generateTraefikLabels("deploy-456", "redis", nil, 6379, nil, nil, false)

	assertLabel(t, labels, "traefik.enable", "false")
	assertLabel(t, labels, "clank.managed", "true")
	assertLabel(t, labels, "clank.service_slug", "redis")

	// No HTTP routing labels
	if _, ok := labels["traefik.http.routers.clank-redis.rule"]; ok {
		t.Error("non-HTTP service should not have router rules")
	}
	if _, ok := labels["traefik.http.services.clank-redis.loadbalancer.server.port"]; ok {
		t.Error("non-HTTP service should not have loadbalancer port")
	}
}

func TestGenerateTraefikLabels_WithEndpoints(t *testing.T) {
	eps := []EndpointInfo{
		{EndpointID: "ep1", Provider: "public_direct", Hostname: "myapp.example.com", TLSMode: "lets_encrypt_http01"},
	}
	labels := generateTraefikLabels("deploy-789", "myapp", nil, 3000, eps, []string{"10.0.0.1"}, true)

	// Both legacy and endpoint labels should exist
	if _, ok := labels["traefik.http.routers.clank-myapp.rule"]; !ok {
		t.Error("expected legacy router")
	}
	if _, ok := labels["traefik.http.routers.clank-myapp-ep0-secure.rule"]; !ok {
		t.Error("expected endpoint-specific secure router")
	}
}


// ── Traefik health check labels (blue-green deploy support) ─────────────

func TestGenerateTraefikLabels_HealthCheckLabels(t *testing.T) {
	labels := generateTraefikLabels("abc123", "myapp", nil, 8080, nil, nil, true, "/health")
	assertLabel(t, labels, "traefik.http.services.clank-myapp.loadbalancer.healthcheck.path", "/health")
	assertLabel(t, labels, "traefik.http.services.clank-myapp.loadbalancer.healthcheck.interval", "5s")
	assertLabel(t, labels, "traefik.http.services.clank-myapp.loadbalancer.healthcheck.timeout", "3s")
	assertLabel(t, labels, "traefik.http.services.clank-myapp.loadbalancer.healthcheck.followredirects", "false")
}

func TestGenerateTraefikLabels_NoHealthCheckLabelsWhenEmpty(t *testing.T) {
	labels := generateTraefikLabels("abc123", "myapp", nil, 8080, nil, nil, true)
	if _, ok := labels["traefik.http.services.clank-myapp.loadbalancer.healthcheck.path"]; ok {
		t.Error("health check labels should not be set when path is empty")
	}
}

func TestGenerateTraefikLabels_NoHealthCheckLabelsForEmptyString(t *testing.T) {
	labels := generateTraefikLabels("abc123", "myapp", nil, 8080, nil, nil, true, "")
	if _, ok := labels["traefik.http.services.clank-myapp.loadbalancer.healthcheck.path"]; ok {
		t.Error("health check labels should not be set for empty string path")
	}
}

func TestGenerateTraefikLabels_HealthCheckWithEndpoints(t *testing.T) {
	eps := []EndpointInfo{
		{EndpointID: "ep1", Provider: "public_direct", Hostname: "app.example.com", TLSMode: "lets_encrypt_http01"},
	}
	labels := generateTraefikLabels("deploy-1", "webapp", nil, 3000, eps, nil, true, "/healthz")
	// Health check labels should coexist with endpoint labels
	assertLabel(t, labels, "traefik.http.services.clank-webapp.loadbalancer.healthcheck.path", "/healthz")
	if _, ok := labels["traefik.http.routers.clank-webapp-ep0-secure.rule"]; !ok {
		t.Error("endpoint labels should still be present")
	}
}

// ── shouldBlueGreen ─────────────────────────────────────────────────────

func TestShouldBlueGreen_WithHealthNoVolumes(t *testing.T) {
	opts := DeployOpts{
		HealthConfig: HealthConfig{Path: "/health"},
		Volumes:      nil,
	}
	if !shouldBlueGreen(opts) {
		t.Error("expected blue-green for health-checked service without volumes")
	}
}

func TestShouldBlueGreen_NoHealthCheck(t *testing.T) {
	opts := DeployOpts{
		HealthConfig: HealthConfig{Path: ""},
		Volumes:      nil,
	}
	if shouldBlueGreen(opts) {
		t.Error("expected recreate for service without health check")
	}
}

func TestShouldBlueGreen_WithVolumes(t *testing.T) {
	opts := DeployOpts{
		HealthConfig: HealthConfig{Path: "/health"},
		Volumes:      []docker.VolumeMount{{Name: "data", MountPath: "/data"}},
	}
	if shouldBlueGreen(opts) {
		t.Error("expected recreate for service with volumes even if health check exists")
	}
}

// ── helpers ─────────────────────────────────────────────────────────────

func assertLabel(t *testing.T, labels map[string]string, key, expected string) {
	t.Helper()
	got, ok := labels[key]
	if !ok {
		t.Errorf("label %q not found", key)
		return
	}
	if got != expected {
		t.Errorf("label %q = %q, want %q", key, got, expected)
	}
}
