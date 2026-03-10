package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// imageHandler defines auto-injection behavior for a known Docker image.
type imageHandler struct {
	name            string
	prefixes        []string
	inject          func(env map[string]string, resolvedURL, pathPrefix string)
	startupRetries  int     // closed-port retries (0 = use default)
	startupInterval int     // seconds between retries (0 = use default)
	minCPU          float64 // minimum CPU cores (0 = no override)
	minMemoryMB     int     // minimum memory in MB (0 = no override)
}

var imageHandlers = []imageHandler{
	{
		name:     "wordpress",
		prefixes: []string{"wordpress:", "library/wordpress:", "docker.io/library/wordpress:"},
		inject:   injectWordPressEnvVars,
	},
	{
		name:            "openclaw",
		prefixes:        []string{"alpine/openclaw:", "openclaw/openclaw:", "ghcr.io/openclaw/openclaw:", "coollabsio/openclaw:"},
		inject:          injectOpenClawEnvVars,
		startupRetries:  12,
		startupInterval: 15, // ~180s total — gateway needs 90-180s depending on CPU
		minCPU:          2.0,
		minMemoryMB:     4096,
	},
}

// matchesImageHandler returns the first handler whose prefix matches imageTag, or nil.
func matchesImageHandler(imageTag string) *imageHandler {
	tag := strings.ToLower(imageTag)
	for i := range imageHandlers {
		for _, prefix := range imageHandlers[i].prefixes {
			if strings.HasPrefix(tag, prefix) {
				return &imageHandlers[i]
			}
		}
	}
	return nil
}

// generateHexToken returns a random hex string of numBytes*2 characters.
func generateHexToken(numBytes int) string {
	b := make([]byte, numBytes)
	rand.Read(b)
	return hex.EncodeToString(b)
}

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
	LANIPs          []string             // Agent LAN IPs for sslip.io routing
	Volumes         []docker.VolumeMount // Persistent volume mounts
	OnLog           func(string)         // Optional: streams deploy log lines to UI
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
		if opts.OnLog != nil {
			opts.OnLog(fmt.Sprintf("Pulling image %s...", opts.ImageTag))
		}
		if err := d.docker.PullImage(ctx, opts.ImageTag, func(msg string) {
			log.Printf("  [pull] %s", msg)
			if opts.OnLog != nil {
				opts.OnLog(msg)
			}
		}); err != nil {
			return result, fmt.Errorf("pulling image: %w", err)
		}
		if opts.OnLog != nil {
			opts.OnLog("Image pulled successfully")
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

	// Auto-inject endpoint-aware env vars (base path, image-specific config).
	// Runs BEFORE CMD extraction so image handlers can provide default CMDs.
	if opts.Env == nil {
		opts.Env = make(map[string]string)
	}
	injectEndpointEnvVars(opts.Env, opts.ImageTag, opts.Endpoints)

	// OpenClaw on HTTP: add Traefik middleware to inject trusted-proxy auth header.
	// This makes the gateway treat every request as pre-authenticated, bypassing
	// the device identity check that fails without a browser secure context.
	if handler := matchesImageHandler(opts.ImageTag); handler != nil && handler.name == "openclaw" {
		for _, ep := range opts.Endpoints {
			u := resolveEndpointURL(ep)
			if u != "" && !strings.HasPrefix(u, "https://") {
				addOpenClawProxyAuthLabels(labels, opts.ServiceSlug)
				break
			}
		}
	}

	// Image handlers can specify minimum resource requirements. Upgrade
	// opts when the handler needs more than the API-supplied defaults.
	cpuLimit := opts.CPULimit
	memoryLimitMB := opts.MemoryLimitMB
	if handler := matchesImageHandler(opts.ImageTag); handler != nil {
		if handler.minCPU > 0 && handler.minCPU > cpuLimit {
			log.Printf("Upgrading CPU limit from %.1f to %.1f for %s handler", cpuLimit, handler.minCPU, handler.name)
			cpuLimit = handler.minCPU
		}
		if handler.minMemoryMB > 0 && handler.minMemoryMB > memoryLimitMB {
			log.Printf("Upgrading memory limit from %dMB to %dMB for %s handler", memoryLimitMB, handler.minMemoryMB, handler.name)
			memoryLimitMB = handler.minMemoryMB
		}
	}

	// Extract CLANK_CONTAINER_CMD magic env var (used as Docker CMD override).
	var cmdOverride []string
	if cmdStr, ok := opts.Env["CLANK_CONTAINER_CMD"]; ok && cmdStr != "" {
		cmdOverride = []string{"sh", "-c", cmdStr}
		delete(opts.Env, "CLANK_CONTAINER_CMD")
		log.Printf("Using CMD override: %v", cmdOverride)
	}

	// Fix volume ownership for non-root images before starting the container
	if len(opts.Volumes) > 0 {
		log.Printf("Ensuring volume ownership for %d mount(s)", len(opts.Volumes))
		if err := d.docker.EnsureVolumeOwnership(ctx, opts.ImageTag, opts.Volumes); err != nil {
			log.Printf("Warning: volume ownership fix failed: %v", err)
		}
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
		CPULimit:      cpuLimit,
		MemoryLimitMB: memoryLimitMB,
		Command:       cmdOverride,
		Volumes:       opts.Volumes,
	})
	if err != nil {
		return result, fmt.Errorf("starting container: %w", err)
	}

	if opts.ProjectNetwork != "" {
		log.Printf("Container %s started on isolated project network %s (%s)", containerName, primaryNetwork, containerID[:12])
	} else {
		log.Printf("Container %s started on shared network %s (no project network set) (%s)", containerName, primaryNetwork, containerID[:12])
	}

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
	if opts.OnLog != nil {
		opts.OnLog(fmt.Sprintf("Container %s started, running health checks...", containerName))
	}

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
		return d.runHTTPHealthChecks(ctx, result, containerID, containerName, ip, effectivePort, hc, onProgress, opts.OnLog)
	}

	// No explicit path — use smart detection
	if isHTTP {
		// Auto-probe common health paths
		autoPath := autoDetectHealthPath(ip, effectivePort)
		if autoPath != "" {
			log.Printf("Auto-detected health path: %s", autoPath)
			hc.Path = autoPath
			return d.runHTTPHealthChecks(ctx, result, containerID, containerName, ip, effectivePort, hc, onProgress, opts.OnLog)
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
		interval := 10

		// Image handlers know what their image needs for startup and
		// take priority over API health-check defaults (which govern
		// HTTP health checks, not port-open probing).
		if handler := matchesImageHandler(opts.ImageTag); handler != nil {
			if handler.startupRetries > 0 {
				retries = handler.startupRetries
			}
			if handler.startupInterval > 0 {
				interval = handler.startupInterval
			}
		}

		// StartupGraceSeconds is the only user-facing knob for startup wait.
		// If set, add it as initial sleep before probing begins.
		if hc.StartupGraceSeconds > 0 {
			log.Printf("Startup grace: waiting %ds before port probing...", hc.StartupGraceSeconds)
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(time.Duration(hc.StartupGraceSeconds) * time.Second):
			}
		}

		log.Printf("Port probe config: retries=%d interval=%ds (total ~%ds)", retries, interval, retries*interval)
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
	onLog func(string),
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
		if onLog != nil {
			onLog(fmt.Sprintf("Health check %s attempt %d/%d...", hc.Path, attempt, hc.Retries))
		}
		healthy := checkHTTPHealth(healthURL, hc.TimeoutSeconds)
		if healthy {
			log.Printf("Health check passed on attempt %d", attempt)
			if onLog != nil {
				onLog(fmt.Sprintf("Health check passed (attempt %d)", attempt))
			}
			onProgress("active", fmt.Sprintf("Health check passed (attempt %d)", attempt), containerID[:12], containerName)
			return result, nil
		}

		log.Printf("Health check failed (attempt %d/%d)", attempt, hc.Retries)
		if onLog != nil {
			onLog(fmt.Sprintf("Health check failed (attempt %d/%d)", attempt, hc.Retries))
		}
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

// addOpenClawProxyAuthLabels adds a Traefik middleware that injects an
// X-Openclaw-User header on every request. This makes the gateway treat the
// connection as trusted-proxy-authenticated, bypassing the device identity
// check that would otherwise fail on plain HTTP (no Web Crypto API).
func addOpenClawProxyAuthLabels(labels map[string]string, serviceSlug string) {
	mwName := fmt.Sprintf("clank-%s-ocauth", serviceSlug)

	// Define the middleware: inject identity header for trusted-proxy auth.
	labels[fmt.Sprintf("traefik.http.middlewares.%s.headers.customrequestheaders.X-Openclaw-User", mwName)] = "operator"

	// Append this middleware to every router already in the label set.
	routerRuleKey := regexp.MustCompile(`^traefik\.http\.routers\.(.+)\.rule$`)

	// Collect router names that exist (from .rule keys).
	routers := map[string]bool{}
	for k := range labels {
		if m := routerRuleKey.FindStringSubmatch(k); m != nil {
			routers[m[1]] = true
		}
	}

	mwRef := mwName + "@docker"
	for router := range routers {
		key := fmt.Sprintf("traefik.http.routers.%s.middlewares", router)
		if existing, ok := labels[key]; ok {
			labels[key] = existing + "," + mwRef
		} else {
			labels[key] = mwRef
		}
	}
}

// injectEndpointEnvVars examines the image tag and active endpoints, then
// auto-injects env vars (CLANK_BASE_PATH, CLANK_BASE_URL, image-specific config)
// into the env map. User-set values are never overwritten.
func injectEndpointEnvVars(env map[string]string, imageTag string, endpoints []EndpointInfo) {
	var resolvedURL, pathPrefix string

	// Resolve endpoint URL if endpoints exist.
	if len(endpoints) > 0 {
		// Find the best endpoint: prefer one with a PathPrefix, otherwise first with a hostname.
		var best *EndpointInfo
		for i := range endpoints {
			if endpoints[i].Hostname == "" {
				continue
			}
			if best == nil {
				best = &endpoints[i]
			}
			if endpoints[i].PathPrefix != "" {
				best = &endpoints[i]
				break
			}
		}
		if best != nil {
			resolvedURL = resolveEndpointURL(*best)
			pathPrefix = best.PathPrefix

			if pathPrefix != "" {
				if _, ok := env["CLANK_BASE_PATH"]; !ok {
					env["CLANK_BASE_PATH"] = pathPrefix
					log.Printf("Auto-injected CLANK_BASE_PATH=%s", pathPrefix)
				}
			}
			if resolvedURL != "" {
				if _, ok := env["CLANK_BASE_URL"]; !ok {
					env["CLANK_BASE_URL"] = resolvedURL
					log.Printf("Auto-injected CLANK_BASE_URL=%s", resolvedURL)
				}
			}
		}
	}

	// App-specific env var injection via image handler registry.
	// Runs regardless of endpoints — some images (e.g. OpenClaw) need
	// env vars and CMD overrides even without an endpoint configured.
	if handler := matchesImageHandler(imageTag); handler != nil {
		handler.inject(env, resolvedURL, pathPrefix)
		log.Printf("Applied %s image handler for %s", handler.name, imageTag)
	}
}

// resolveEndpointURL derives a full URL (scheme + host + path) from endpoint metadata.
func resolveEndpointURL(ep EndpointInfo) string {
	if ep.Hostname == "" {
		return ""
	}

	scheme := "http://"
	switch ep.TLSMode {
	case "lets_encrypt_http01", "cloudflare_edge", "tailscale_https":
		scheme = "https://"
	}

	return scheme + ep.Hostname + ep.PathPrefix
}

// injectWordPressEnvVars sets WORDPRESS_CONFIG_EXTRA for WordPress images.
func injectWordPressEnvVars(env map[string]string, resolvedURL, pathPrefix string) {
	if _, ok := env["WORDPRESS_CONFIG_EXTRA"]; ok {
		return
	}
	wpConfig := buildWordPressConfig(resolvedURL, pathPrefix)
	if wpConfig != "" {
		env["WORDPRESS_CONFIG_EXTRA"] = wpConfig
	}
}

// injectOpenClawEnvVars sets env vars required for OpenClaw gateway startup.
// Without OPENCLAW_GATEWAY_TOKEN the gateway never opens its HTTP listener.
// Without --bind lan --auth token the gateway binds to loopback only (unreachable
// from outside the container in Docker bridge networking).
func injectOpenClawEnvVars(env map[string]string, resolvedURL, pathPrefix string) {
	if _, ok := env["OPENCLAW_GATEWAY_TOKEN"]; !ok {
		env["OPENCLAW_GATEWAY_TOKEN"] = generateHexToken(16)
	}
	if _, ok := env["NODE_OPTIONS"]; !ok {
		env["NODE_OPTIONS"] = "--max-old-space-size=3584"
	}
	// Override CMD to configure and start the gateway:
	//  1. Allow host-header origin fallback (required for non-loopback binding)
	//  2. Add endpoint URL to allowedOrigins so the UI doesn't get CORS-blocked
	//  3. Trust RFC1918 subnets as proxies (Traefik forwards from Docker network)
	//  4. For HTTP: use trusted-proxy auth so Traefik's X-Openclaw-User header
	//     bypasses device identity (browser has no Web Crypto on plain HTTP)
	if _, ok := env["CLANK_CONTAINER_CMD"]; !ok {
		configCmds := "node openclaw.mjs config set gateway.controlUi.dangerouslyAllowHostHeaderOriginFallback true"
		if resolvedURL != "" {
			configCmds += fmt.Sprintf(` && node openclaw.mjs config set gateway.controlUi.allowedOrigins '["%s"]'`, resolvedURL)
		}
		// Always trust Docker-network proxies so Traefik headers are accepted.
		configCmds += ` && node openclaw.mjs config set gateway.trustedProxies '["172.16.0.0/12", "192.168.0.0/16", "10.0.0.0/8"]'`

		isHTTP := resolvedURL != "" && !strings.HasPrefix(resolvedURL, "https://")
		if isHTTP {
			// HTTP endpoints lack a secure context — browser Web Crypto API is
			// unavailable, so device identity will always fail.
			configCmds += " && node openclaw.mjs config set gateway.controlUi.dangerouslyDisableDeviceAuth true"
			configCmds += " && node openclaw.mjs config set gateway.controlUi.allowInsecureAuth true"
			// Tell the gateway to accept Traefik's X-Openclaw-User header as
			// proof of identity. Traefik injects this header via middleware
			// labels on the container, so every request arrives pre-authenticated.
			configCmds += ` && node openclaw.mjs config set gateway.auth.trustedProxy '{"userHeader":"X-Openclaw-User"}'`
		}

		// Auth mode: trusted-proxy for HTTP (Traefik injects identity header),
		//            token for HTTPS (browser has secure context for device identity).
		authMode := "token"
		if isHTTP {
			authMode = "trusted-proxy"
		}
		env["CLANK_CONTAINER_CMD"] = configCmds + " && exec node openclaw.mjs gateway --allow-unconfigured --bind lan --auth " + authMode
	}
}

// buildWordPressConfig generates the PHP snippet for WORDPRESS_CONFIG_EXTRA.
// If pathPrefix is set, it includes $_SERVER fixups for path-prefix routing.
// Otherwise it just sets WP_HOME/WP_SITEURL to the resolved URL.
func buildWordPressConfig(resolvedURL, pathPrefix string) string {
	if resolvedURL == "" {
		return ""
	}

	if pathPrefix != "" {
		// Split URL into host-part and path-part for the PHP variables.
		hostURL := strings.TrimSuffix(resolvedURL, pathPrefix)
		return fmt.Sprintf(
			"$clank_prefix = '%s';\n"+
				"$clank_host = '%s';\n"+
				"define('WP_HOME', $clank_host . $clank_prefix);\n"+
				"define('WP_SITEURL', $clank_host . $clank_prefix);\n"+
				"if (strpos($_SERVER['REQUEST_URI'], $clank_prefix) !== 0) {\n"+
				"    $_SERVER['REQUEST_URI'] = $clank_prefix . $_SERVER['REQUEST_URI'];\n"+
				"}\n"+
				"if (isset($_SERVER['SCRIPT_NAME']) && strpos($_SERVER['SCRIPT_NAME'], $clank_prefix) !== 0) {\n"+
				"    $_SERVER['SCRIPT_NAME'] = $clank_prefix . $_SERVER['SCRIPT_NAME'];\n"+
				"}\n"+
				"if (isset($_SERVER['PHP_SELF']) && strpos($_SERVER['PHP_SELF'], $clank_prefix) !== 0) {\n"+
				"    $_SERVER['PHP_SELF'] = $clank_prefix . $_SERVER['PHP_SELF'];\n"+
				"}",
			pathPrefix, hostURL,
		)
	}

	// No path prefix — just set canonical URL.
	return fmt.Sprintf(
		"define('WP_HOME', '%s');\ndefine('WP_SITEURL', '%s');",
		resolvedURL, resolvedURL,
	)
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
