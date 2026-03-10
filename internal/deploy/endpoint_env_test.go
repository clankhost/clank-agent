package deploy

import (
	"encoding/hex"
	"fmt"
	"testing"
)

func TestResolveEndpointURL(t *testing.T) {
	tests := []struct {
		name string
		ep   EndpointInfo
		want string
	}{
		{
			name: "tailscale with path prefix",
			ep:   EndpointInfo{Hostname: "ffood1.tail3bc261.ts.net", PathPrefix: "/wordpress", TLSMode: "tailscale_https"},
			want: "https://ffood1.tail3bc261.ts.net/wordpress",
		},
		{
			name: "public direct with lets encrypt",
			ep:   EndpointInfo{Hostname: "myapp.example.com", TLSMode: "lets_encrypt_http01"},
			want: "https://myapp.example.com",
		},
		{
			name: "cloudflare edge",
			ep:   EndpointInfo{Hostname: "myapp.example.com", TLSMode: "cloudflare_edge"},
			want: "https://myapp.example.com",
		},
		{
			name: "tls off",
			ep:   EndpointInfo{Hostname: "myapp.local", TLSMode: "off"},
			want: "http://myapp.local",
		},
		{
			name: "empty hostname",
			ep:   EndpointInfo{Hostname: "", TLSMode: "tailscale_https"},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveEndpointURL(tt.ep)
			if got != tt.want {
				t.Errorf("resolveEndpointURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMatchesImageHandler(t *testing.T) {
	positiveWordPress := []string{
		"wordpress:latest",
		"wordpress:6-apache",
		"wordpress:6.7-php8.3-apache",
		"library/wordpress:latest",
		"docker.io/library/wordpress:6",
		"WORDPRESS:latest",
		"WordPress:6.7",
	}
	for _, img := range positiveWordPress {
		h := matchesImageHandler(img)
		if h == nil || h.name != "wordpress" {
			t.Errorf("matchesImageHandler(%q) should return wordpress handler", img)
		}
	}

	positiveOpenClaw := []string{
		"alpine/openclaw:latest",
		"alpine/openclaw:v0.45.3",
		"openclaw/openclaw:latest",
		"ghcr.io/openclaw/openclaw:main",
		"coollabsio/openclaw:latest",
		"Alpine/OpenClaw:latest",
	}
	for _, img := range positiveOpenClaw {
		h := matchesImageHandler(img)
		if h == nil || h.name != "openclaw" {
			t.Errorf("matchesImageHandler(%q) should return openclaw handler", img)
		}
	}

	negative := []string{
		"nginx:latest",
		"mywordpress:latest",
		"wordpress-custom:latest",
		"ghcr.io/someone/wordpress:latest",
		"alpine/something:latest",
		"",
	}
	for _, img := range negative {
		if matchesImageHandler(img) != nil {
			t.Errorf("matchesImageHandler(%q) should return nil", img)
		}
	}
}

func TestBuildWordPressConfig_PathPrefix(t *testing.T) {
	config := buildWordPressConfig("https://ffood1.tail3bc261.ts.net/wordpress", "/wordpress")
	if config == "" {
		t.Fatal("expected non-empty config")
	}
	// Should contain WP_HOME, WP_SITEURL, and $_SERVER fixups
	for _, substr := range []string{
		"define('WP_HOME'",
		"define('WP_SITEURL'",
		"$_SERVER['REQUEST_URI']",
		"$_SERVER['SCRIPT_NAME']",
		"$_SERVER['PHP_SELF']",
		"$clank_prefix = '/wordpress'",
	} {
		if !contains(config, substr) {
			t.Errorf("config missing %q", substr)
		}
	}
}

func TestBuildWordPressConfig_NoPathPrefix(t *testing.T) {
	config := buildWordPressConfig("https://myapp.example.com", "")
	if config == "" {
		t.Fatal("expected non-empty config")
	}
	if !contains(config, "define('WP_HOME', 'https://myapp.example.com')") {
		t.Error("missing WP_HOME define")
	}
	if contains(config, "$_SERVER") {
		t.Error("should not contain $_SERVER fixups without path prefix")
	}
}

func TestBuildWordPressConfig_EmptyURL(t *testing.T) {
	config := buildWordPressConfig("", "/wordpress")
	if config != "" {
		t.Errorf("expected empty config for empty URL, got %q", config)
	}
}

func TestInjectEndpointEnvVars_WordPress(t *testing.T) {
	env := map[string]string{}
	endpoints := []EndpointInfo{
		{Hostname: "ffood1.tail3bc261.ts.net", PathPrefix: "/wordpress", TLSMode: "tailscale_https"},
	}
	injectEndpointEnvVars(env, "wordpress:6-apache", endpoints)

	if env["CLANK_BASE_PATH"] != "/wordpress" {
		t.Errorf("CLANK_BASE_PATH = %q, want /wordpress", env["CLANK_BASE_PATH"])
	}
	if env["CLANK_BASE_URL"] != "https://ffood1.tail3bc261.ts.net/wordpress" {
		t.Errorf("CLANK_BASE_URL = %q", env["CLANK_BASE_URL"])
	}
	if env["WORDPRESS_CONFIG_EXTRA"] == "" {
		t.Error("WORDPRESS_CONFIG_EXTRA should be set")
	}
}

func TestInjectEndpointEnvVars_UserOverride(t *testing.T) {
	env := map[string]string{
		"CLANK_BASE_PATH":        "/custom",
		"CLANK_BASE_URL":         "https://custom.example.com",
		"WORDPRESS_CONFIG_EXTRA": "custom config",
	}
	endpoints := []EndpointInfo{
		{Hostname: "ffood1.tail3bc261.ts.net", PathPrefix: "/wordpress", TLSMode: "tailscale_https"},
	}
	injectEndpointEnvVars(env, "wordpress:latest", endpoints)

	if env["CLANK_BASE_PATH"] != "/custom" {
		t.Error("user CLANK_BASE_PATH was overwritten")
	}
	if env["CLANK_BASE_URL"] != "https://custom.example.com" {
		t.Error("user CLANK_BASE_URL was overwritten")
	}
	if env["WORDPRESS_CONFIG_EXTRA"] != "custom config" {
		t.Error("user WORDPRESS_CONFIG_EXTRA was overwritten")
	}
}

func TestInjectEndpointEnvVars_NonWordPress(t *testing.T) {
	env := map[string]string{}
	endpoints := []EndpointInfo{
		{Hostname: "ffood1.tail3bc261.ts.net", PathPrefix: "/myapp", TLSMode: "tailscale_https"},
	}
	injectEndpointEnvVars(env, "nginx:latest", endpoints)

	if env["CLANK_BASE_PATH"] != "/myapp" {
		t.Errorf("CLANK_BASE_PATH = %q, want /myapp", env["CLANK_BASE_PATH"])
	}
	if _, ok := env["WORDPRESS_CONFIG_EXTRA"]; ok {
		t.Error("WORDPRESS_CONFIG_EXTRA should not be set for non-WordPress images")
	}
}

func TestInjectEndpointEnvVars_NoEndpoints(t *testing.T) {
	env := map[string]string{}
	injectEndpointEnvVars(env, "nginx:latest", nil)

	if len(env) != 0 {
		t.Errorf("expected empty env for non-matching image, got %v", env)
	}
}

func TestInjectEndpointEnvVars_NoEndpoints_OpenClaw(t *testing.T) {
	env := map[string]string{}
	injectEndpointEnvVars(env, "alpine/openclaw:latest", nil)

	// OpenClaw handler should fire even without endpoints
	if env["OPENCLAW_GATEWAY_TOKEN"] == "" {
		t.Error("OPENCLAW_GATEWAY_TOKEN should be set even without endpoints")
	}
	cmd := env["CLANK_CONTAINER_CMD"]
	// Should write a JSON config file and start the gateway
	if !contains(cmd, "openclaw-clank.json") {
		t.Error("CLANK_CONTAINER_CMD should write a JSON config file")
	}
	if !contains(cmd, "--auth trusted-proxy") {
		t.Error("should use --auth trusted-proxy even without endpoints")
	}
	// JSON config should contain key settings
	if !contains(cmd, `"dangerouslyDisableDeviceAuth": true`) {
		t.Error("should disable device auth even without endpoints")
	}
	if !contains(cmd, `"allowInsecureAuth": true`) {
		t.Error("should allow insecure auth even without endpoints")
	}
	// No endpoint-derived vars
	if _, ok := env["CLANK_BASE_URL"]; ok {
		t.Error("CLANK_BASE_URL should not be set without endpoints")
	}
}

func TestInjectEndpointEnvVars_PrefersPathPrefix(t *testing.T) {
	env := map[string]string{}
	endpoints := []EndpointInfo{
		{Hostname: "myapp.example.com", TLSMode: "lets_encrypt_http01"},
		{Hostname: "ffood1.tail3bc261.ts.net", PathPrefix: "/myapp", TLSMode: "tailscale_https"},
	}
	injectEndpointEnvVars(env, "nginx:latest", endpoints)

	// Should pick the path-prefix endpoint
	if env["CLANK_BASE_PATH"] != "/myapp" {
		t.Errorf("CLANK_BASE_PATH = %q, want /myapp", env["CLANK_BASE_PATH"])
	}
	if env["CLANK_BASE_URL"] != "https://ffood1.tail3bc261.ts.net/myapp" {
		t.Errorf("CLANK_BASE_URL = %q", env["CLANK_BASE_URL"])
	}
}

func TestInjectOpenClawEnvVars(t *testing.T) {
	env := map[string]string{}
	injectOpenClawEnvVars(env, "https://myhost.example.com", "")

	// Token should be auto-generated: 16 bytes = 32 hex chars
	token := env["OPENCLAW_GATEWAY_TOKEN"]
	if token == "" {
		t.Fatal("OPENCLAW_GATEWAY_TOKEN should be set")
	}
	if len(token) != 32 {
		t.Errorf("OPENCLAW_GATEWAY_TOKEN length = %d, want 32", len(token))
	}
	if _, err := hex.DecodeString(token); err != nil {
		t.Errorf("OPENCLAW_GATEWAY_TOKEN is not valid hex: %v", err)
	}

	if env["NODE_OPTIONS"] != "--max-old-space-size=3584" {
		t.Errorf("NODE_OPTIONS = %q, want '--max-old-space-size=3584'", env["NODE_OPTIONS"])
	}

	// CMD should write a nested JSON config file (not dotted config set keys)
	// and start the gateway with --auth trusted-proxy.
	cmd := env["CLANK_CONTAINER_CMD"]
	if cmd == "" {
		t.Fatal("CLANK_CONTAINER_CMD should be set")
	}
	// Should write a JSON config file
	if !contains(cmd, "openclaw-clank.json") {
		t.Error("should write a JSON config file")
	}
	if !contains(cmd, "--config /app/openclaw-clank.json") {
		t.Error("should pass --config flag to gateway")
	}
	if !contains(cmd, "--auth trusted-proxy") {
		t.Error("CLANK_CONTAINER_CMD should use --auth trusted-proxy")
	}
	// JSON config should contain nested structure
	if !contains(cmd, `"dangerouslyAllowHostHeaderOriginFallback": true`) {
		t.Error("JSON config should set controlUi fallback")
	}
	// Should include both http:// and https:// origins in JSON array
	if !contains(cmd, `"https://myhost.example.com"`) {
		t.Error("allowedOrigins should include https:// URL")
	}
	if !contains(cmd, `"http://myhost.example.com"`) {
		t.Error("allowedOrigins should include http:// URL")
	}
	// JSON config should have trustedProxy with userHeader
	if !contains(cmd, `"userHeader"`) {
		t.Error("should set trustedProxy userHeader in JSON config")
	}
	if !contains(cmd, `"X-Openclaw-User"`) {
		t.Error("should configure X-Openclaw-User header")
	}
	if !contains(cmd, `"dangerouslyDisableDeviceAuth": true`) {
		t.Error("should disable device auth")
	}
	if !contains(cmd, `"allowInsecureAuth": true`) {
		t.Error("should allow insecure auth")
	}
	// Should NOT contain dotted config set commands
	if contains(cmd, "config set gateway.") {
		t.Error("should NOT use dotted 'config set' commands — use JSON config file instead")
	}
}

func TestInjectOpenClawEnvVars_HTTP(t *testing.T) {
	env := map[string]string{}
	injectOpenClawEnvVars(env, "http://openclaw.172.30.227.155.sslip.io", "")

	cmd := env["CLANK_CONTAINER_CMD"]
	if cmd == "" {
		t.Fatal("CLANK_CONTAINER_CMD should be set")
	}

	// Should use trusted-proxy auth
	if !contains(cmd, "--auth trusted-proxy") {
		t.Error("should use --auth trusted-proxy")
	}
	if contains(cmd, "--auth token") {
		t.Error("should NOT use --auth token")
	}

	// JSON config should include both http:// and https:// origins
	if !contains(cmd, `"https://openclaw.172.30.227.155.sslip.io"`) {
		t.Error("allowedOrigins should include https:// URL")
	}
	if !contains(cmd, `"http://openclaw.172.30.227.155.sslip.io"`) {
		t.Error("allowedOrigins should include http:// URL")
	}

	// JSON config should contain trusted proxy header
	if !contains(cmd, `"X-Openclaw-User"`) {
		t.Error("should configure X-Openclaw-User as trusted proxy header")
	}

	// JSON config should disable device auth and allow insecure auth
	if !contains(cmd, `"dangerouslyDisableDeviceAuth": true`) {
		t.Error("should disable device auth")
	}
	if !contains(cmd, `"allowInsecureAuth": true`) {
		t.Error("should allow insecure auth")
	}

	// JSON config should have trustedProxies and origin fallback
	if !contains(cmd, `"trustedProxies"`) {
		t.Error("should set trustedProxies")
	}
	if !contains(cmd, `"dangerouslyAllowHostHeaderOriginFallback": true`) {
		t.Error("should set origin fallback")
	}

	// Should write JSON config file, not use dotted config set
	if !contains(cmd, "openclaw-clank.json") {
		t.Error("should write a JSON config file")
	}
	if contains(cmd, "config set gateway.") {
		t.Error("should NOT use dotted 'config set' commands")
	}
}

func TestInjectOpenClawEnvVars_UserOverride(t *testing.T) {
	env := map[string]string{
		"OPENCLAW_GATEWAY_TOKEN": "mytoken",
		"NODE_OPTIONS":           "--max-old-space-size=4096",
		"CLANK_CONTAINER_CMD":    "custom start command",
	}
	injectOpenClawEnvVars(env, "https://myhost.example.com", "")

	if env["OPENCLAW_GATEWAY_TOKEN"] != "mytoken" {
		t.Error("user OPENCLAW_GATEWAY_TOKEN was overwritten")
	}
	if env["NODE_OPTIONS"] != "--max-old-space-size=4096" {
		t.Error("user NODE_OPTIONS was overwritten")
	}
	if env["CLANK_CONTAINER_CMD"] != "custom start command" {
		t.Error("user CLANK_CONTAINER_CMD was overwritten")
	}
}

func TestInjectEndpointEnvVars_OpenClaw(t *testing.T) {
	env := map[string]string{}
	endpoints := []EndpointInfo{
		{Hostname: "myhost.example.com", TLSMode: "lets_encrypt_http01"},
	}
	injectEndpointEnvVars(env, "alpine/openclaw:latest", endpoints)

	// Should have base URL from endpoint
	if env["CLANK_BASE_URL"] != "https://myhost.example.com" {
		t.Errorf("CLANK_BASE_URL = %q", env["CLANK_BASE_URL"])
	}

	// Should have OpenClaw-specific vars injected
	if env["OPENCLAW_GATEWAY_TOKEN"] == "" {
		t.Error("OPENCLAW_GATEWAY_TOKEN should be auto-generated")
	}
	if env["NODE_OPTIONS"] != "--max-old-space-size=3584" {
		t.Error("NODE_OPTIONS should be set")
	}

	// Should use JSON config file and trusted-proxy auth
	cmd := env["CLANK_CONTAINER_CMD"]
	if !contains(cmd, "openclaw-clank.json") || !contains(cmd, "--auth trusted-proxy") || !contains(cmd, `"dangerouslyAllowHostHeaderOriginFallback": true`) {
		t.Errorf("CLANK_CONTAINER_CMD incorrect, got %q", cmd)
	}
	// Should include both http:// and https:// origins in JSON config
	if !contains(cmd, `"https://myhost.example.com"`) || !contains(cmd, `"http://myhost.example.com"`) {
		t.Errorf("allowedOrigins should include both schemes, got %q", cmd)
	}

	// Should NOT have WordPress vars
	if _, ok := env["WORDPRESS_CONFIG_EXTRA"]; ok {
		t.Error("WORDPRESS_CONFIG_EXTRA should not be set for OpenClaw")
	}
}

func TestInjectEndpointEnvVars_OpenClaw_HTTP(t *testing.T) {
	env := map[string]string{}
	endpoints := []EndpointInfo{
		{Hostname: "openclaw.172.30.227.155.sslip.io", TLSMode: "off"},
	}
	injectEndpointEnvVars(env, "alpine/openclaw:latest", endpoints)

	// Should have HTTP base URL
	if env["CLANK_BASE_URL"] != "http://openclaw.172.30.227.155.sslip.io" {
		t.Errorf("CLANK_BASE_URL = %q, want http://...", env["CLANK_BASE_URL"])
	}

	// Should use JSON config file and trusted-proxy auth
	cmd := env["CLANK_CONTAINER_CMD"]
	if !contains(cmd, "--auth trusted-proxy") {
		t.Errorf("OpenClaw should use --auth trusted-proxy, got %q", cmd)
	}
	if !contains(cmd, "openclaw-clank.json") {
		t.Error("OpenClaw should write a JSON config file")
	}
	if !contains(cmd, `"X-Openclaw-User"`) {
		t.Error("OpenClaw should configure trustedProxy userHeader")
	}
	// Should include both http:// and https:// origins in JSON
	if !contains(cmd, `"https://openclaw.172.30.227.155.sslip.io"`) {
		t.Error("allowedOrigins should include https:// URL")
	}
	if !contains(cmd, `"http://openclaw.172.30.227.155.sslip.io"`) {
		t.Error("allowedOrigins should include http:// URL")
	}
}

func TestAddOpenClawProxyAuthLabels(t *testing.T) {
	labels := map[string]string{
		"traefik.http.routers.clank-myservice.rule":        "Host(`myservice.example.com`)",
		"traefik.http.routers.clank-myservice.entrypoints": "web",
		"traefik.http.routers.clank-myservice-ep1.rule":    "Host(`ep1.example.com`)",
	}

	addOpenClawProxyAuthLabels(labels, "myservice")

	// Should define the middleware
	mwKey := "traefik.http.middlewares.clank-myservice-ocauth.headers.customrequestheaders.X-Openclaw-User"
	if labels[mwKey] != "operator" {
		t.Errorf("middleware label = %q, want 'operator'", labels[mwKey])
	}

	// Should add middleware ref to both routers
	mwRef := "clank-myservice-ocauth@docker"
	for _, router := range []string{"clank-myservice", "clank-myservice-ep1"} {
		key := fmt.Sprintf("traefik.http.routers.%s.middlewares", router)
		val, ok := labels[key]
		if !ok {
			t.Errorf("missing middlewares label for router %s", router)
		} else if !contains(val, mwRef) {
			t.Errorf("router %s middlewares = %q, should contain %q", router, val, mwRef)
		}
	}
}

func TestAddOpenClawProxyAuthLabels_AppendToExisting(t *testing.T) {
	labels := map[string]string{
		"traefik.http.routers.clank-svc.rule":        "Host(`svc.example.com`)",
		"traefik.http.routers.clank-svc.middlewares":  "existing-mw@docker",
	}

	addOpenClawProxyAuthLabels(labels, "svc")

	mwKey := "traefik.http.routers.clank-svc.middlewares"
	want := "existing-mw@docker,clank-svc-ocauth@docker"
	if labels[mwKey] != want {
		t.Errorf("middlewares = %q, want %q", labels[mwKey], want)
	}
}

func TestGenerateHexToken(t *testing.T) {
	token := generateHexToken(16)
	if len(token) != 32 {
		t.Errorf("generateHexToken(16) length = %d, want 32", len(token))
	}
	if _, err := hex.DecodeString(token); err != nil {
		t.Errorf("not valid hex: %v", err)
	}

	// Two tokens should be different (probabilistically)
	token2 := generateHexToken(16)
	if token == token2 {
		t.Error("two consecutive tokens should differ")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
