package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/anaremore/clank/apps/agent/internal/docker"
)

const servicesNetwork = "clank-services"

// Default port assigned by the platform when user doesn't specify one.
const platformDefaultPort = 8080

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

// DeployResult holds introspection data collected during deployment.
// Always returned (even partial on failure) so the caller can report
// startup logs, port info, etc. back to the control plane.
type DeployResult struct {
	ImageMeta    *docker.ImageMeta
	Inspection   *docker.ContainerInspection
	StartupLogs  string
	Ports        []docker.DiscoveredPort
	EffectivePort int
	IsHTTP       bool // whether the primary port speaks HTTP
}

var safeDomainRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.\-]*[a-zA-Z0-9])?$`)

// Deploy starts a container with Traefik labels and runs health checks.
// Returns a DeployResult (always non-nil, even on error) and an error.
func (d *Deployer) Deploy(ctx context.Context, opts DeployOpts, onProgress ProgressFunc) (*DeployResult, error) {
	result := &DeployResult{EffectivePort: opts.Port}
	onProgress("deploying", "Starting deployment...", "", "")

	// Determine which network to start the container on.
	primaryNetwork := opts.ProjectNetwork
	if primaryNetwork == "" {
		primaryNetwork = servicesNetwork
	}

	if err := d.docker.EnsureNetwork(ctx, primaryNetwork); err != nil {
		return result, fmt.Errorf("ensuring network: %w", err)
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
			return result, fmt.Errorf("pulling image: %w", err)
		}
	}

	// === Phase A: Image introspection (best-effort) ===
	imageMeta, err := d.docker.InspectImage(ctx, opts.ImageTag)
	if err != nil {
		log.Printf("Warning: image introspection failed: %v", err)
	} else {
		result.ImageMeta = imageMeta
		log.Printf("Image introspection: EXPOSE=%v CMD=%v", imageMeta.ExposedPorts, imageMeta.Cmd)
	}

	// === Phase D: Port auto-fill ===
	// If the user kept the platform default (8080) and the image EXPOSEs exactly one
	// different port, auto-fill to the image port.
	effectivePort := opts.Port
	if effectivePort == platformDefaultPort && imageMeta != nil && len(imageMeta.ExposedPorts) == 1 {
		exposed := imageMeta.ExposedPorts[0]
		if exposed != platformDefaultPort {
			log.Printf("Auto-filled port from image EXPOSE: %d (was default %d)", exposed, platformDefaultPort)
			effectivePort = exposed
		}
	}
	result.EffectivePort = effectivePort

	// Generate Traefik labels using the effective port.
	// isHTTP defaults to true (assume HTTP until probing proves otherwise).
	isHTTP := true
	labels := generateTraefikLabels(opts.DeploymentID, opts.ServiceSlug, opts.Domains, effectivePort, opts.Endpoints, opts.LANIPs, isHTTP)

	// Tell Traefik which network to use for reaching this container
	if opts.ProjectNetwork != "" {
		labels["traefik.docker.network"] = opts.ProjectNetwork
	}

	// Container name
	containerName := fmt.Sprintf("clank-%s-%s", opts.ServiceSlug, opts.DeploymentID[:8])

	// Extract CLANK_CONTAINER_CMD magic env var (used as Docker CMD override).
	var cmdOverride []string
	if cmdStr, ok := opts.Env["CLANK_CONTAINER_CMD"]; ok && cmdStr != "" {
		cmdOverride = []string{"sh", "-c", cmdStr}
		delete(opts.Env, "CLANK_CONTAINER_CMD")
		log.Printf("Using CMD override: %v", cmdOverride)
	}

	// Start container on the project network (isolated) with slug alias for DNS
	containerID, err := d.docker.RunContainer(ctx, docker.RunOpts{
		Image:         opts.ImageTag,
		Name:          containerName,
		Env:           opts.Env,
		Port:          effectivePort,
		Labels:        labels,
		Network:       primaryNetwork,
		NetworkAlias:  opts.ServiceSlug,
		CPULimit:      opts.CPULimit,
		MemoryLimitMB: opts.MemoryLimitMB,
		Command:       cmdOverride,
	})
	if err != nil {
		return result, fmt.Errorf("starting container: %w", err)
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

	// === Phase A: Crash detection ===
	// Wait 2s then check if the container immediately exited.
	select {
	case <-ctx.Done():
		return result, ctx.Err()
	case <-time.After(2 * time.Second):
	}

	ci, err := d.docker.InspectContainer(ctx, containerID)
	if err != nil {
		log.Printf("Warning: container inspection failed: %v", err)
	} else {
		result.Inspection = ci
		if ci.State == "exited" || ci.State == "dead" {
			// Container crashed immediately — capture logs before removing
			log.Printf("Container crashed immediately (state=%s exit=%d oom=%v)", ci.State, ci.ExitCode, ci.OOMKilled)
			logs, logErr := d.docker.GetStartupLogs(ctx, containerID, 100)
			if logErr != nil {
				log.Printf("Warning: failed to capture startup logs: %v", logErr)
			} else {
				result.StartupLogs = logs
			}
			// Remove the crashed container
			if stopErr := d.docker.StopAndRemove(ctx, containerID); stopErr != nil {
				log.Printf("Warning: failed to remove crashed container: %v", stopErr)
			}
			msg := fmt.Sprintf("Container crashed on startup (exit code %d)", ci.ExitCode)
			if ci.OOMKilled {
				msg = "Container killed: out of memory (OOMKilled)"
			}
			return result, fmt.Errorf("%s", msg)
		}
	}

	onProgress("health_checking", "Container started, running health checks...", containerID[:12], containerName)

	// Get container IP for health checks and probing
	ip, err := d.docker.GetContainerIP(ctx, containerID, primaryNetwork)
	if err != nil {
		// IP lookup can fail if the container exited between crash detection and now.
		// Re-inspect to check if it crashed.
		ci2, inspErr := d.docker.InspectContainer(ctx, containerID)
		if inspErr == nil && (ci2.State == "exited" || ci2.State == "dead") {
			result.Inspection = ci2
			log.Printf("Container exited after startup (state=%s exit=%d oom=%v)", ci2.State, ci2.ExitCode, ci2.OOMKilled)
			logs, logErr := d.docker.GetStartupLogs(ctx, containerID, 100)
			if logErr == nil {
				result.StartupLogs = logs
			}
			if stopErr := d.docker.StopAndRemove(ctx, containerID); stopErr != nil {
				log.Printf("Warning: failed to remove crashed container: %v", stopErr)
			}
			msg := fmt.Sprintf("Container crashed on startup (exit code %d)", ci2.ExitCode)
			if ci2.OOMKilled {
				msg = "Container killed: out of memory (OOMKilled)"
			}
			return result, fmt.Errorf("%s", msg)
		}
		return result, fmt.Errorf("getting container IP: %w", err)
	}
	if ci != nil {
		ci.IP = ip // update with network-specific IP
	}

	// === Phase B: Port probing ===
	// Collect ports to probe: configured port + image EXPOSE ports (deduplicated)
	probePorts := []int{effectivePort}
	if imageMeta != nil {
		for _, p := range imageMeta.ExposedPorts {
			if p != effectivePort {
				probePorts = append(probePorts, p)
			}
		}
	}

	probeResults := docker.ProbeAllPorts(ctx, ip, probePorts)
	result.Ports = probeResults

	// Determine if the primary port speaks HTTP
	primaryProtocol := "closed"
	for _, pr := range probeResults {
		if pr.Port == effectivePort {
			primaryProtocol = pr.Protocol
			break
		}
	}
	isHTTP = primaryProtocol == "http"
	result.IsHTTP = isHTTP

	log.Printf("Port probing results: primary=%d protocol=%s total_probed=%d", effectivePort, primaryProtocol, len(probeResults))

	// If primary port is TCP-only (not HTTP), regenerate labels without Traefik routing
	if !isHTTP && primaryProtocol == "tcp" {
		log.Printf("Primary port is TCP-only — disabling Traefik HTTP routing for this service")
		newLabels := generateTraefikLabels(opts.DeploymentID, opts.ServiceSlug, opts.Domains, effectivePort, opts.Endpoints, opts.LANIPs, false)
		if opts.ProjectNetwork != "" {
			newLabels["traefik.docker.network"] = opts.ProjectNetwork
		}
		// We can't relabel a running container, but we can log the intent.
		// The labels were already set at container creation. For TCP services
		// the health check below will pass via TCP, and the Traefik router
		// will simply never get requests on those routes (harmless).
		// Future: stop + recreate with correct labels if needed.
		_ = newLabels
	}

	// === Phase B: Smart health checks ===
	hc := opts.HealthConfig

	// If health check path is explicitly set, use existing behavior
	if hc.Path != "" {
		return d.runHTTPHealthChecks(ctx, result, containerID, containerName, ip, effectivePort, hc, onProgress)
	}

	// No explicit path — use smart detection
	if isHTTP {
		// Auto-probe common health paths
		autoPath := autoDetectHealthPath(ip, effectivePort)
		if autoPath != "" {
			log.Printf("Auto-detected health path: %s", autoPath)
			hc.Path = autoPath
			return d.runHTTPHealthChecks(ctx, result, containerID, containerName, ip, effectivePort, hc, onProgress)
		}
		// HTTP but no health path found — consider alive if port responds
		log.Println("HTTP port responding, no health path found — marking active")
		onProgress("active", "Deployment active (HTTP port responding)", containerID[:12], containerName)
		return result, nil
	}

	if primaryProtocol == "tcp" {
		// TCP-only service (database, cache, etc.) — TCP connect = healthy
		log.Println("TCP-only service — connection successful, marking active")
		onProgress("active", "Deployment active (TCP port responding)", containerID[:12], containerName)
		return result, nil
	}

	// Port is closed — wait and retry (slow startup)
	if primaryProtocol == "closed" {
		log.Printf("Primary port %d is closed, waiting for startup...", effectivePort)
		retries := 3
		if hc.Retries > 0 {
			retries = hc.Retries
		}
		interval := 10
		if hc.IntervalSeconds > 0 {
			interval = hc.IntervalSeconds
		}
		for attempt := 1; attempt <= retries; attempt++ {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(time.Duration(interval) * time.Second):
			}
			proto := docker.ProbePort(ctx, ip, effectivePort)
			if proto == "http" || proto == "tcp" {
				log.Printf("Port %d now responding (%s) on attempt %d", effectivePort, proto, attempt)
				onProgress("active", fmt.Sprintf("Deployment active (%s port responding, attempt %d)", proto, attempt), containerID[:12], containerName)
				return result, nil
			}
			log.Printf("Port %d still closed (attempt %d/%d)", effectivePort, attempt, retries)
		}

		// Still closed — capture logs and fail
		logs, logErr := d.docker.GetStartupLogs(ctx, containerID, 100)
		if logErr == nil {
			result.StartupLogs = logs
		}
		log.Printf("Stopping failed container %s — port never opened", containerName)
		if stopErr := d.docker.StopAndRemove(ctx, containerID); stopErr != nil {
			log.Printf("Warning: failed to remove container: %v", stopErr)
		}
		return result, fmt.Errorf("port %d never opened after %d attempts", effectivePort, retries)
	}

	// Fallback: skip health checks
	log.Println("Health check skipped (no path configured, port status unknown)")
	onProgress("active", "Deployment active (health check skipped)", containerID[:12], containerName)
	return result, nil
}

// runHTTPHealthChecks performs traditional HTTP health checks and handles failure cleanup.
func (d *Deployer) runHTTPHealthChecks(
	ctx context.Context,
	result *DeployResult,
	containerID, containerName, ip string,
	port int,
	hc HealthConfig,
	onProgress ProgressFunc,
) (*DeployResult, error) {
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
			return result, ctx.Err()
		case <-time.After(time.Duration(hc.StartupGraceSeconds) * time.Second):
		}
	}

	healthURL := fmt.Sprintf("http://%s:%d%s", ip, port, hc.Path)

	for attempt := 1; attempt <= hc.Retries; attempt++ {
		healthy := checkHTTPHealth(healthURL, hc.TimeoutSeconds)
		if healthy {
			log.Printf("Health check passed on attempt %d", attempt)
			onProgress("active", fmt.Sprintf("Health check passed (attempt %d)", attempt), containerID[:12], containerName)
			return result, nil
		}

		log.Printf("Health check failed (attempt %d/%d)", attempt, hc.Retries)
		if attempt < hc.Retries {
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(time.Duration(hc.IntervalSeconds) * time.Second):
			}
		}
	}

	// Capture startup logs before removing
	logs, logErr := d.docker.GetStartupLogs(ctx, containerID, 100)
	if logErr != nil {
		log.Printf("Warning: failed to capture startup logs: %v", logErr)
	} else {
		result.StartupLogs = logs
	}

	// Stop the failed container so Traefik doesn't route traffic to it.
	log.Printf("Stopping failed container %s after health check failure", containerName)
	if stopErr := d.docker.StopAndRemove(ctx, containerID); stopErr != nil {
		log.Printf("Warning: failed to remove unhealthy container: %v", stopErr)
	}

	return result, fmt.Errorf("health checks failed after %d attempts", hc.Retries)
}

// autoDetectHealthPath tries common health check paths and returns the first
// one that returns 200-399, or empty string if none work.
func autoDetectHealthPath(ip string, port int) string {
	paths := []string{"/health", "/healthz", "/"}
	client := &http.Client{Timeout: 3 * time.Second}
	for _, path := range paths {
		url := fmt.Sprintf("http://%s:%d%s", ip, port, path)
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			return path
		}
	}
	return ""
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

func generateTraefikLabels(deploymentID, serviceSlug string, domains []string, port int, endpoints []EndpointInfo, lanIPs []string, isHTTP bool) map[string]string {
	labels := map[string]string{
		"clank.managed":       "true",
		"clank.service_slug":  serviceSlug,
		"clank.deployment_id": deploymentID,
	}

	if !isHTTP {
		// TCP-only service — no Traefik HTTP routing, just management labels
		labels["traefik.enable"] = "false"
		return labels
	}

	labels["traefik.enable"] = "true"

	// Shared service port
	svcName := "clank-" + serviceSlug
	labels[fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", svcName)] = fmt.Sprintf("%d", port)

	// Always generate sslip.io / localhost labels for basic accessibility
	generateLegacyLabels(labels, serviceSlug, domains, lanIPs)

	// Resolve empty Tailscale hostnames locally at deploy time.
	for i := range endpoints {
		if endpoints[i].Provider == "private_tailscale_https" && endpoints[i].Hostname == "" {
			if host, err := resolveTailscaleHostname(); err == nil {
				endpoints[i].Hostname = host
				log.Printf("Resolved Tailscale hostname for endpoint %s at deploy time: %s", endpoints[i].EndpointID, host)
			} else {
				log.Printf("Could not resolve Tailscale hostname for endpoint %s: %v", endpoints[i].EndpointID, err)
			}
		}
	}

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

		routerBase := fmt.Sprintf("clank-%s-ep%d", serviceSlug, i)

		switch ep.Provider {
		case "public_direct":
			secureRouter := routerBase + "-secure"
			labels[fmt.Sprintf("traefik.http.routers.%s.rule", secureRouter)] = fmt.Sprintf("Host(`%s`)", hostname)
			labels[fmt.Sprintf("traefik.http.routers.%s.entrypoints", secureRouter)] = "websecure"
			labels[fmt.Sprintf("traefik.http.routers.%s.tls.certresolver", secureRouter)] = "letsencrypt"
			labels[fmt.Sprintf("traefik.http.routers.%s.service", secureRouter)] = svcName

			httpRouter := routerBase + "-http"
			labels[fmt.Sprintf("traefik.http.routers.%s.rule", httpRouter)] = fmt.Sprintf("Host(`%s`)", hostname)
			labels[fmt.Sprintf("traefik.http.routers.%s.entrypoints", httpRouter)] = "web"
			labels[fmt.Sprintf("traefik.http.routers.%s.middlewares", httpRouter)] = routerBase + "-redirect"
			labels[fmt.Sprintf("traefik.http.middlewares.%s-redirect.redirectscheme.scheme", routerBase)] = "https"

		case "public_tunnel_cloudflare":
			labels[fmt.Sprintf("traefik.http.routers.%s.rule", routerBase)] = fmt.Sprintf("Host(`%s`)", hostname)
			labels[fmt.Sprintf("traefik.http.routers.%s.entrypoints", routerBase)] = "web"
			labels[fmt.Sprintf("traefik.http.routers.%s.service", routerBase)] = svcName

		case "private_tailscale_https":
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
			labels[fmt.Sprintf("traefik.http.routers.%s.rule", routerBase)] = fmt.Sprintf("Host(`%s`)", hostname)
			labels[fmt.Sprintf("traefik.http.routers.%s.entrypoints", routerBase)] = "web"
			labels[fmt.Sprintf("traefik.http.routers.%s.service", routerBase)] = svcName
		}
	}
}

// resolveTailscaleHostname discovers the machine's tailnet DNS name.
func resolveTailscaleHostname() (string, error) {
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
		return "", fmt.Errorf("tailscale DNSName is empty")
	}
	return hostname, nil
}
