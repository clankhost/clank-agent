package deploy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/anaremore/clank/apps/agent/internal/docker"
)

const servicesNetwork = "clank-services"

// ProgressFunc is a callback for reporting deploy progress.
type ProgressFunc func(status, message, containerID, containerName string)

// Deployer handles container deployment on the agent host.
type Deployer struct {
	docker *docker.Manager
}

// NewDeployer creates a Deployer with the given Docker manager.
func NewDeployer(dm *docker.Manager) *Deployer {
	return &Deployer{docker: dm}
}

// EndpointInfo mirrors the proto EndpointInfo for deploy-time label generation.
type EndpointInfo struct {
	EndpointID string
	Provider   string
	Hostname   string
	PathPrefix string
	TLSMode    string
}

// DeployOpts configures a deployment.
type DeployOpts struct {
	DeploymentID    string
	ServiceSlug     string
	ImageTag        string
	Env             map[string]string
	Port            int
	Domains         []string
	Endpoints       []EndpointInfo
	HealthCheckPath string
	HealthConfig    HealthConfig
	CPULimit        float64
	MemoryLimitMB   int
	ProjectNetwork  string
	LANIPs          []string // Agent LAN IPs for sslip.io routing
}

// HealthConfig mirrors the proto HealthCheckConfig.
type HealthConfig struct {
	Path                string
	TimeoutSeconds      int
	Retries             int
	IntervalSeconds     int
	StartupGraceSeconds int
}

var safeDomainRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.\-]*[a-zA-Z0-9])?$`)

// Deploy starts a container with Traefik labels and runs health checks.
func (d *Deployer) Deploy(ctx context.Context, opts DeployOpts, onProgress ProgressFunc) error {
	onProgress("deploying", "Starting deployment...", "", "")

	// Determine which network to start the container on.
	// Prefer per-project network for isolation; fall back to shared for backward compat.
	primaryNetwork := opts.ProjectNetwork
	if primaryNetwork == "" {
		primaryNetwork = servicesNetwork
	}

	if err := d.docker.EnsureNetwork(ctx, primaryNetwork); err != nil {
		return fmt.Errorf("ensuring network: %w", err)
	}

	// Stop old container for this service slug (if any)
	oldID, oldName, err := d.docker.FindContainerByLabel(ctx, "clank.service_slug", opts.ServiceSlug)
	if err != nil {
		log.Printf("Warning: could not search for old container: %v", err)
	}
	if oldID != "" {
		log.Printf("Stopping old container %s (%s)", oldName, oldID[:12])
		if err := d.docker.StopAndRemove(ctx, oldID); err != nil {
			log.Printf("Warning: failed to remove old container: %v", err)
		}
	}

	// Pull image if not locally built
	if opts.ImageTag != "" && !strings.HasPrefix(opts.ImageTag, "clank-") {
		if err := d.docker.PullImage(ctx, opts.ImageTag, func(msg string) {
			log.Printf("  [pull] %s", msg)
		}); err != nil {
			return fmt.Errorf("pulling image: %w", err)
		}
	}

	// Generate Traefik labels (endpoint-aware if endpoints are provided)
	labels := generateTraefikLabels(opts.DeploymentID, opts.ServiceSlug, opts.Domains, opts.Port, opts.Endpoints, opts.LANIPs)

	// Tell Traefik which network to use for reaching this container
	if opts.ProjectNetwork != "" {
		labels["traefik.docker.network"] = opts.ProjectNetwork
	}

	// Container name
	containerName := fmt.Sprintf("clank-%s-%s", opts.ServiceSlug, opts.DeploymentID[:8])

	// Start container on the project network (isolated) with slug alias for DNS
	containerID, err := d.docker.RunContainer(ctx, docker.RunOpts{
		Image:         opts.ImageTag,
		Name:          containerName,
		Env:           opts.Env,
		Port:          opts.Port,
		Labels:        labels,
		Network:       primaryNetwork,
		NetworkAlias:  opts.ServiceSlug,
		CPULimit:      opts.CPULimit,
		MemoryLimitMB: opts.MemoryLimitMB,
	})
	if err != nil {
		return fmt.Errorf("starting container: %w", err)
	}

	log.Printf("Container %s started on network %s (%s)", containerName, primaryNetwork, containerID[:12])

	// Connect Traefik to the project network so it can route to this container
	if opts.ProjectNetwork != "" {
		if traefikID := d.docker.FindTraefikContainer(ctx); traefikID != "" {
			if err := d.docker.ConnectToNetworkIfNeeded(ctx, traefikID, opts.ProjectNetwork); err != nil {
				log.Printf("Warning: failed to connect Traefik to network %s: %v", opts.ProjectNetwork, err)
			} else {
				log.Printf("Traefik connected to project network %s", opts.ProjectNetwork)
			}
		}
	}

	onProgress("health_checking", "Container started, running health checks...", containerID[:12], containerName)

	// Health checks
	hc := opts.HealthConfig
	if hc.Path == "" {
		// Skip health checks
		log.Println("Health check skipped (no path configured)")
		onProgress("active", "Deployment active (health check skipped)", containerID[:12], containerName)
		return nil
	}

	if hc.Retries <= 0 {
		hc.Retries = 3
	}
	if hc.IntervalSeconds <= 0 {
		hc.IntervalSeconds = 10
	}
	if hc.TimeoutSeconds <= 0 {
		hc.TimeoutSeconds = 5
	}

	// Startup grace period
	if hc.StartupGraceSeconds > 0 {
		log.Printf("Waiting %ds startup grace...", hc.StartupGraceSeconds)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(hc.StartupGraceSeconds) * time.Second):
		}
	}

	// Get container IP for health checks (use project network)
	ip, err := d.docker.GetContainerIP(ctx, containerID, primaryNetwork)
	if err != nil {
		return fmt.Errorf("getting container IP: %w", err)
	}

	healthURL := fmt.Sprintf("http://%s:%d%s", ip, opts.Port, hc.Path)

	for attempt := 1; attempt <= hc.Retries; attempt++ {
		healthy := checkHTTPHealth(healthURL, hc.TimeoutSeconds)
		if healthy {
			log.Printf("Health check passed on attempt %d", attempt)
			onProgress("active", fmt.Sprintf("Health check passed (attempt %d)", attempt), containerID[:12], containerName)
			return nil
		}

		log.Printf("Health check failed (attempt %d/%d)", attempt, hc.Retries)
		if attempt < hc.Retries {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(hc.IntervalSeconds) * time.Second):
			}
		}
	}

	// Stop the failed container so Traefik doesn't route traffic to it (CRITICAL-1 fix).
	log.Printf("Stopping failed container %s after health check failure", containerName)
	if stopErr := d.docker.StopAndRemove(ctx, containerID); stopErr != nil {
		log.Printf("Warning: failed to remove unhealthy container: %v", stopErr)
	}

	return fmt.Errorf("health checks failed after %d attempts", hc.Retries)
}

func checkHTTPHealth(url string, timeoutSec int) bool {
	client := &http.Client{Timeout: time.Duration(timeoutSec) * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

func generateTraefikLabels(deploymentID, serviceSlug string, domains []string, port int, endpoints []EndpointInfo, lanIPs []string) map[string]string {
	labels := map[string]string{
		"traefik.enable":      "true",
		"clank.managed":       "true",
		"clank.service_slug":  serviceSlug,
		"clank.deployment_id": deploymentID,
	}

	// Shared service port
	svcName := "clank-" + serviceSlug
	labels[fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", svcName)] = fmt.Sprintf("%d", port)

	// Always generate sslip.io / localhost labels for basic accessibility
	generateLegacyLabels(labels, serviceSlug, domains, lanIPs)

	// Also generate per-endpoint labels (HTTPS with custom domains)
	if len(endpoints) > 0 {
		generateEndpointLabels(labels, serviceSlug, port, endpoints)
	}

	return labels
}

func generateLegacyLabels(labels map[string]string, serviceSlug string, domains []string, lanIPs []string) map[string]string {
	routerName := "clank-" + serviceSlug

	var safeDomains []string
	for _, d := range domains {
		if safeDomainRe.MatchString(d) && len(d) <= 253 {
			safeDomains = append(safeDomains, d)
		} else {
			log.Printf("Rejected unsafe domain for Traefik: %q", d)
		}
	}
	if len(safeDomains) == 0 {
		safeDomains = []string{serviceSlug + ".localhost"}
	}

	// Add sslip.io entries for each LAN IP so services are reachable from the network.
	// e.g. n8n.192.168.1.100.sslip.io resolves to 192.168.1.100, Traefik routes by Host.
	for _, ip := range lanIPs {
		sslipHost := fmt.Sprintf("%s.%s.sslip.io", serviceSlug, ip)
		safeDomains = append(safeDomains, sslipHost)
	}

	var rules []string
	for _, d := range safeDomains {
		rules = append(rules, fmt.Sprintf("Host(`%s`)", d))
	}
	hostRules := strings.Join(rules, " || ")

	labels[fmt.Sprintf("traefik.http.routers.%s.rule", routerName)] = hostRules
	labels[fmt.Sprintf("traefik.http.routers.%s.entrypoints", routerName)] = "web"

	return labels
}

func generateEndpointLabels(labels map[string]string, serviceSlug string, port int, endpoints []EndpointInfo) {
	svcName := "clank-" + serviceSlug

	for i, ep := range endpoints {
		hostname := ep.Hostname
		if hostname == "" {
			continue
		}
		if !safeDomainRe.MatchString(hostname) || len(hostname) > 253 {
			log.Printf("Rejected unsafe hostname for endpoint %s: %q", ep.EndpointID, hostname)
			continue
		}

		// Router name includes index to keep labels unique per endpoint
		routerBase := fmt.Sprintf("clank-%s-ep%d", serviceSlug, i)

		switch ep.Provider {
		case "public_direct":
			// HTTPS with Let's Encrypt certresolver + HTTP redirect
			secureRouter := routerBase + "-secure"
			labels[fmt.Sprintf("traefik.http.routers.%s.rule", secureRouter)] = fmt.Sprintf("Host(`%s`)", hostname)
			labels[fmt.Sprintf("traefik.http.routers.%s.entrypoints", secureRouter)] = "websecure"
			labels[fmt.Sprintf("traefik.http.routers.%s.tls.certresolver", secureRouter)] = "letsencrypt"
			labels[fmt.Sprintf("traefik.http.routers.%s.service", secureRouter)] = svcName

			// HTTP → HTTPS redirect
			httpRouter := routerBase + "-http"
			labels[fmt.Sprintf("traefik.http.routers.%s.rule", httpRouter)] = fmt.Sprintf("Host(`%s`)", hostname)
			labels[fmt.Sprintf("traefik.http.routers.%s.entrypoints", httpRouter)] = "web"
			labels[fmt.Sprintf("traefik.http.routers.%s.middlewares", httpRouter)] = routerBase + "-redirect"
			labels[fmt.Sprintf("traefik.http.middlewares.%s-redirect.redirectscheme.scheme", routerBase)] = "https"

		case "public_tunnel_cloudflare":
			// HTTP only — Cloudflare terminates TLS at edge
			labels[fmt.Sprintf("traefik.http.routers.%s.rule", routerBase)] = fmt.Sprintf("Host(`%s`)", hostname)
			labels[fmt.Sprintf("traefik.http.routers.%s.entrypoints", routerBase)] = "web"
			labels[fmt.Sprintf("traefik.http.routers.%s.service", routerBase)] = svcName

		case "private_tailscale_https":
			// Path-based routing on tailnet hostname with StripPrefix
			rule := fmt.Sprintf("Host(`%s`)", hostname)
			if ep.PathPrefix != "" {
				rule = fmt.Sprintf("Host(`%s`) && PathPrefix(`%s`)", hostname, ep.PathPrefix)
			}
			labels[fmt.Sprintf("traefik.http.routers.%s.rule", routerBase)] = rule
			labels[fmt.Sprintf("traefik.http.routers.%s.entrypoints", routerBase)] = "web"
			labels[fmt.Sprintf("traefik.http.routers.%s.service", routerBase)] = svcName
			if ep.PathPrefix != "" {
				labels[fmt.Sprintf("traefik.http.routers.%s.middlewares", routerBase)] = routerBase + "-strip"
				labels[fmt.Sprintf("traefik.http.middlewares.%s-strip.stripprefix.prefixes", routerBase)] = ep.PathPrefix
			}

		case "lan_only", "byo_proxy":
			// HTTP router only
			labels[fmt.Sprintf("traefik.http.routers.%s.rule", routerBase)] = fmt.Sprintf("Host(`%s`)", hostname)
			labels[fmt.Sprintf("traefik.http.routers.%s.entrypoints", routerBase)] = "web"
			labels[fmt.Sprintf("traefik.http.routers.%s.service", routerBase)] = svcName
		}
	}
}
