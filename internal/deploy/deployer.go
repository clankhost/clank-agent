package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/clankhost/clank-agent/internal/docker"
	"github.com/docker/docker/api/types/container"
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
		if strings.Contains(tag, "/clank/templates/"+imageHandlers[i].name+":") {
			return &imageHandlers[i]
		}
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

// dbHealthcheck defines a Docker HEALTHCHECK to inject for known database images.
type dbHealthcheck struct {
	prefixes    []string
	test        []string
	interval    time.Duration
	timeout     time.Duration
	retries     int
	startPeriod time.Duration
}

var dbHealthchecks = []dbHealthcheck{
	{
		prefixes:    []string{"postgres:", "library/postgres:", "docker.io/library/postgres:"},
		test:        []string{"CMD-SHELL", "pg_isready -U ${POSTGRES_USER:-postgres}"},
		interval:    3 * time.Second,
		timeout:     3 * time.Second,
		retries:     10,
		startPeriod: 5 * time.Second,
	},
	{
		prefixes:    []string{"mysql:", "library/mysql:", "docker.io/library/mysql:", "mariadb:", "library/mariadb:", "docker.io/library/mariadb:"},
		test:        []string{"CMD-SHELL", "mysqladmin ping -h localhost --silent"},
		interval:    3 * time.Second,
		timeout:     3 * time.Second,
		retries:     10,
		startPeriod: 10 * time.Second,
	},
	{
		prefixes:    []string{"redis:", "library/redis:", "docker.io/library/redis:"},
		test:        []string{"CMD-SHELL", "redis-cli ping | grep -q PONG"},
		interval:    2 * time.Second,
		timeout:     2 * time.Second,
		retries:     5,
		startPeriod: 3 * time.Second,
	},
	{
		prefixes:    []string{"mongo:", "library/mongo:", "docker.io/library/mongo:"},
		test:        []string{"CMD-SHELL", "mongosh --eval 'db.runCommand(\"ping\").ok' --quiet || mongo --eval 'db.runCommand(\"ping\").ok' --quiet"},
		interval:    3 * time.Second,
		timeout:     3 * time.Second,
		retries:     10,
		startPeriod: 10 * time.Second,
	},
}

// matchDBHealthcheck returns the healthcheck config for a known database image, or nil.
func matchDBHealthcheck(imageTag string) *dbHealthcheck {
	tag := strings.ToLower(imageTag)
	for i := range dbHealthchecks {
		for _, prefix := range dbHealthchecks[i].prefixes {
			if strings.HasPrefix(tag, prefix) {
				return &dbHealthchecks[i]
			}
		}
	}
	return nil
}

const servicesNetwork = "clank-services"

// Default port assigned by the platform when user doesn't specify one.
const platformDefaultPort = 8080

// ProgressFunc is a callback for reporting deploy progress.
type ProgressFunc func(status, message, containerID, containerName string)

// DockerClient defines the Docker operations required by Deployer.
type DockerClient interface {
	EnsureNetwork(ctx context.Context, name string) error
	FindContainerByLabel(ctx context.Context, key, value string) (string, string, error)
	FindAllContainersByLabel(ctx context.Context, key, value string) ([]docker.ContainerRef, error)
	FindContainerByName(ctx context.Context, name string) (string, string, error)
	FindTraefikContainer(ctx context.Context) string
	ConnectToNetworkIfNeeded(ctx context.Context, containerID, networkName string) error
	PullImage(ctx context.Context, img string, auth *docker.RegistryAuth, onLog func(string)) error
	InspectImage(ctx context.Context, imageRef string) (*docker.ImageMeta, error)
	GetImageDigest(ctx context.Context, imageRef string) (string, error)
	EnsureVolumeOwnership(ctx context.Context, imageRef string, volumes []docker.VolumeMount) error
	RunContainer(ctx context.Context, opts docker.RunOpts) (string, error)
	InspectContainer(ctx context.Context, containerID string) (*docker.ContainerInspection, error)
	GetContainerIP(ctx context.Context, containerID, networkName string) (string, error)
	GetHealthStatus(ctx context.Context, containerID string) string
	GetStartupLogs(ctx context.Context, containerID string, tailLines int) (string, error)
	StopAndRemove(ctx context.Context, containerID string) error
}

var _ DockerClient = (*docker.Manager)(nil)

// Deployer handles container deployment on the agent host.
type Deployer struct {
	docker DockerClient

	// Traefik-ready cutover probe configuration (zero values use production defaults).
	traefikEndpoint     string
	traefikReadyTimeout time.Duration
	traefikPollInterval time.Duration
	traefikHTTPClient   *http.Client

	// Optional test overrides for startup/cutover waits (zero = 2s default, negative = skip).
	crashCheckDelay time.Duration
	cutoverDelay    time.Duration
}

const (
	defaultTraefikEndpoint     = "http://127.0.0.1:80"
	defaultTraefikReadyTimeout = 10 * time.Second
	defaultTraefikPollInterval = 500 * time.Millisecond
)

// NewDeployer creates a Deployer with the given Docker manager.
func NewDeployer(dm *docker.Manager) *Deployer {
	var dc DockerClient
	if dm != nil {
		dc = dm
	}
	return &Deployer{docker: dc}
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
	DeploymentID     string
	ServiceSlug      string
	ImageTag         string
	Env              map[string]string
	Port             int
	Domains          []string
	Endpoints        []EndpointInfo
	HealthCheckPath  string
	HealthConfig     HealthConfig
	CPULimit         float64
	MemoryLimitMB    int
	ProjectNetwork   string
	LANIPs           []string             // Agent LAN IPs for sslip.io routing
	Volumes          []docker.VolumeMount // Persistent volume mounts
	ContainerCommand []string             // Docker CMD override from the API
	CompanionSlugs   []string             // Companion service slugs to wait for before starting
	RegistryAuth     *docker.RegistryAuth // Auth for registry pulls (ADR-006)
	OnLog            func(string)         // Optional: streams deploy log lines to UI
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
	ImageMeta     *docker.ImageMeta
	Inspection    *docker.ContainerInspection
	StartupLogs   string
	Ports         []docker.DiscoveredPort
	EffectivePort int
	IsHTTP        bool   // whether the primary port speaks HTTP
	ImageDigest   string // sha256 digest of the deployed image
}

var safeDomainRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.\-]*[a-zA-Z0-9])?$`)

// waitForCompanions blocks until all companion containers are running and healthy.
// Polls every 3s, times out after 120s. Returns nil if all companions are ready.
func (d *Deployer) waitForCompanions(ctx context.Context, slugs []string, onProgress ProgressFunc, onLog func(string)) error {
	if len(slugs) == 0 {
		return nil
	}

	const pollInterval = 3 * time.Second
	const timeout = 120 * time.Second
	deadline := time.Now().Add(timeout)

	onProgress("deploying", fmt.Sprintf("Waiting for %d companion service(s) to start...", len(slugs)), "", "")
	if onLog != nil {
		onLog(fmt.Sprintf("Waiting for companion services: %v", slugs))
	}

	for {
		allReady := true
		for _, slug := range slugs {
			id, _, err := d.docker.FindContainerByLabel(ctx, "clank.service_slug", slug)
			if err != nil || id == "" {
				allReady = false
				continue
			}

			// Container is running — check Docker HEALTHCHECK if present
			health := d.docker.GetHealthStatus(ctx, id)
			if health == "starting" || health == "unhealthy" {
				allReady = false
				continue
			}
			// health == "" (no healthcheck) or "healthy" → ready
		}

		if allReady {
			msg := fmt.Sprintf("All %d companion service(s) are ready", len(slugs))
			onProgress("deploying", msg, "", "")
			if onLog != nil {
				onLog(msg)
			}
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("companion services %v did not become ready within %s", slugs, timeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// shouldBlueGreen returns true when a deploy can safely run both old and new
// containers in parallel (explicit health check path, no shared volumes).
func shouldBlueGreen(opts DeployOpts) bool {
	return opts.HealthConfig.Path != "" && len(opts.Volumes) == 0
}

// sweepStaleContainers stops and removes all containers matching
// clank.service_slug=<serviceSlug> except keepContainerID (when non-empty).
// Returns the number of stale containers removed.
func (d *Deployer) sweepStaleContainers(ctx context.Context, serviceSlug, keepContainerID string) int {
	containers, err := d.docker.FindAllContainersByLabel(ctx, "clank.service_slug", serviceSlug)
	if err != nil {
		log.Printf("Warning: could not list containers for stale cleanup (%s): %v", serviceSlug, err)
		return 0
	}

	removed := 0
	for _, c := range containers {
		if keepContainerID != "" && c.ID == keepContainerID {
			continue
		}
		log.Printf("Stopping stale container %s (%s) for service %s", c.Name, shortID(c.ID, 12), serviceSlug)
		if stopErr := d.docker.StopAndRemove(ctx, c.ID); stopErr != nil {
			log.Printf("Warning: failed to remove stale container %s (%s): %v", c.Name, shortID(c.ID, 12), stopErr)
			continue
		}
		removed++
	}
	log.Printf("Cleaned up %d stale container(s) for service %s", removed, serviceSlug)
	return removed
}

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

	// Find all existing containers for this service slug (running and stopped).
	// Blue-green: keep old containers running until new passes health checks.
	// Recreate: stop & remove ALL old containers immediately (databases, volumes, no health path).
	oldContainers, err := d.docker.FindAllContainersByLabel(ctx, "clank.service_slug", opts.ServiceSlug)
	if err != nil {
		log.Printf("Warning: could not search for old containers: %v", err)
	}

	useBlueGreen := shouldBlueGreen(opts) && len(oldContainers) > 0

	if len(oldContainers) > 0 && !useBlueGreen {
		// Recreate strategy: stop and remove ALL existing containers before starting new
		removed := 0
		for _, old := range oldContainers {
			log.Printf("Stopping old container %s (%s) [recreate]", old.Name, shortID(old.ID, 12))
			if err := d.docker.StopAndRemove(ctx, old.ID); err != nil {
				log.Printf("Warning: failed to remove old container %s: %v", old.Name, err)
			} else {
				removed++
			}
		}
		log.Printf("Cleaned up %d stale container(s) for service %s before recreate", removed, opts.ServiceSlug)
		oldContainers = nil // Already cleaned up
	}
	if useBlueGreen {
		log.Printf("Blue-green deploy: %d existing container(s) stay up until new passes health checks", len(oldContainers))
		onProgress("deploying", "Starting new container (current version remains active)", "", "")
	}

	// Pull image if not locally built
	if opts.ImageTag != "" && !strings.HasPrefix(opts.ImageTag, "clank-") {
		if opts.OnLog != nil {
			opts.OnLog(fmt.Sprintf("Pulling image %s...", opts.ImageTag))
		}
		if err := d.docker.PullImage(ctx, opts.ImageTag, opts.RegistryAuth, func(msg string) {
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

	// === Volume auto-detection from image VOLUME directives ===
	// If the image declares VOLUME paths that aren't already covered by
	// explicit volume config from the API, create deterministically-named
	// volumes so data persists across redeploys.
	if imageMeta != nil && len(imageMeta.Volumes) > 0 {
		configuredPaths := make(map[string]bool, len(opts.Volumes))
		for _, v := range opts.Volumes {
			configuredPaths[v.MountPath] = true
		}
		for _, volPath := range imageMeta.Volumes {
			if configuredPaths[volPath] {
				continue
			}
			if !docker.IsValidMountPath(volPath) {
				log.Printf("Skipping auto-detected volume at blocked path: %s", volPath)
				continue
			}
			sanitized := strings.TrimPrefix(volPath, "/")
			sanitized = strings.ReplaceAll(sanitized, "/", "-")
			volName := fmt.Sprintf("clank-%s-vol-%s", opts.ServiceSlug, sanitized)
			opts.Volumes = append(opts.Volumes, docker.VolumeMount{
				Name:      volName,
				MountPath: volPath,
			})
			log.Printf("Auto-detected volume: %s -> %s", volName, volPath)
		}
	}

	// Generate Traefik labels using the effective port.
	// isHTTP defaults to true (assume HTTP until probing proves otherwise).
	isHTTP := true
	labels := generateTraefikLabels(opts.DeploymentID, opts.ServiceSlug, opts.Domains, effectivePort, opts.Endpoints, opts.LANIPs, isHTTP, opts.HealthConfig.Path)

	// Tell Traefik which network to use for reaching this container
	if opts.ProjectNetwork != "" {
		labels["traefik.docker.network"] = opts.ProjectNetwork
	}

	// Container name
	containerName := fmt.Sprintf("clank-%s-%s", opts.ServiceSlug, shortID(opts.DeploymentID, 8))

	// Fast path: if a container with this exact name already exists and is
	// running, this is a retry for the same deployment (stream broke before
	// progress was delivered). Skip all work and report success immediately.
	if existingID, _, findErr := d.docker.FindContainerByName(ctx, containerName); findErr == nil && existingID != "" {
		ci, inspErr := d.docker.InspectContainer(ctx, existingID)
		if inspErr == nil && ci != nil && ci.State == "running" {
			log.Printf("Container %s already running (id: %s) — reporting active immediately", containerName, existingID[:12])
			result.Inspection = ci
			// Get image digest if available
			if digest, err := d.docker.GetImageDigest(ctx, opts.ImageTag); err == nil {
				result.ImageDigest = digest
			}
			onProgress("active", "Container already running (reconnect fast path)", existingID[:12], containerName)
			return result, nil
		}
	}

	// Auto-inject endpoint-aware env vars (base path, image-specific config).
	// Runs BEFORE CMD extraction so image handlers can provide default CMDs.
	if opts.Env == nil {
		opts.Env = make(map[string]string)
	}
	injectEndpointEnvVars(opts.Env, opts.ImageTag, opts.Endpoints)

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

	// Container command override: proto-level takes precedence, then env var fallback.
	var cmdOverride []string
	var entrypointOverride []string
	if len(opts.ContainerCommand) > 0 {
		cmdOverride = opts.ContainerCommand
		entrypointOverride = []string{""} // Clear image ENTRYPOINT so CMD runs standalone
		log.Printf("Using proto command override: %v", cmdOverride)
	} else if cmdStr, ok := opts.Env["CLANK_CONTAINER_CMD"]; ok && cmdStr != "" {
		// Legacy: CLANK_CONTAINER_CMD env var (used by image handlers)
		cmdOverride = []string{"sh", "-c", cmdStr}
		entrypointOverride = []string{""} // Docker convention: clear image ENTRYPOINT
		delete(opts.Env, "CLANK_CONTAINER_CMD")
		log.Printf("Using CMD override from env: %v", cmdOverride)
	}

	// Inject Docker HEALTHCHECK for known database images.
	// Only inject if the image doesn't already define a HEALTHCHECK.
	var injectedHealthcheck *container.HealthConfig
	if dbHC := matchDBHealthcheck(opts.ImageTag); dbHC != nil {
		if imageMeta == nil || imageMeta.Healthcheck == nil {
			injectedHealthcheck = &container.HealthConfig{
				Test:        dbHC.test,
				Interval:    dbHC.interval,
				Timeout:     dbHC.timeout,
				Retries:     dbHC.retries,
				StartPeriod: dbHC.startPeriod,
			}
			log.Printf("Injecting Docker HEALTHCHECK for %s: %v", opts.ImageTag, dbHC.test)
		}
	}

	// Wait for companion services to be running+healthy before starting.
	// This ensures databases/caches are ready when the primary app starts.
	if len(opts.CompanionSlugs) > 0 {
		if err := d.waitForCompanions(ctx, opts.CompanionSlugs, onProgress, opts.OnLog); err != nil {
			return result, fmt.Errorf("waiting for companions: %w", err)
		}
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
		Entrypoint:    entrypointOverride,
		Volumes:       opts.Volumes,
		Healthcheck:   injectedHealthcheck,
	})
	if err != nil {
		// Container name conflict — adopt the existing container if it's from a prior attempt
		// for the same deployment (duplicate dispatch from reconnect).
		if strings.Contains(err.Error(), "is already in use") {
			existingID, _, findErr := d.docker.FindContainerByName(ctx, containerName)
			if findErr == nil && existingID != "" {
				log.Printf("Container %s already exists (id: %s), adopting it", containerName, existingID[:12])
				containerID = existingID
				err = nil
			}
		}
		if err != nil {
			return result, fmt.Errorf("starting container: %w", err)
		}
	}

	if opts.ProjectNetwork != "" {
		log.Printf("Container %s started on isolated project network %s (%s)", containerName, primaryNetwork, containerID[:12])
	} else {
		log.Printf("Container %s started on shared network %s (no project network set) (%s)", containerName, primaryNetwork, containerID[:12])
	}

	// Extract image digest for tracking (ADR-006)
	if digest, err := d.docker.GetImageDigest(ctx, opts.ImageTag); err == nil {
		result.ImageDigest = digest
		log.Printf("Image digest: %s", digest)
	} else {
		log.Printf("Warning: failed to get image digest: %v", err)
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
	crashDelay := d.crashCheckDelay
	if crashDelay == 0 {
		crashDelay = 2 * time.Second
	}
	if crashDelay > 0 {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-time.After(crashDelay):
		}
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

	// Get container IP for health checks and probing.
	// Retry a few times — Docker may briefly report no IP if the container exits
	// quickly or the network attachment hasn't propagated yet.
	var ip string
	for ipAttempt := 0; ipAttempt < 3; ipAttempt++ {
		ip, err = d.docker.GetContainerIP(ctx, containerID, primaryNetwork)
		if err == nil {
			break
		}
		// Check if container has exited/crashed.
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
		state := "unknown"
		if inspErr == nil {
			state = ci2.State
		}
		log.Printf("IP lookup attempt %d failed (container state=%s): %v", ipAttempt+1, state, err)
		time.Sleep(time.Second)
	}
	if err != nil {
		// Collect startup logs for diagnostics even when the error is not a clean exit
		logs, logErr := d.docker.GetStartupLogs(ctx, containerID, 100)
		if logErr == nil {
			result.StartupLogs = logs
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
		newLabels := generateTraefikLabels(opts.DeploymentID, opts.ServiceSlug, opts.Domains, effectivePort, opts.Endpoints, opts.LANIPs, false, opts.HealthConfig.Path)
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

	// If health check path is explicitly set, run health checks
	if hc.Path != "" {
		result, passedAttempt, err := d.runHTTPHealthChecks(ctx, result, containerID, containerName, ip, effectivePort, hc, opts.OnLog)
		if err != nil {
			// Health check failed. In blue-green mode, old containers are still
			// serving traffic — runHTTPHealthChecks already stopped the new one.
			if useBlueGreen {
				log.Printf("Blue-green deploy failed: existing container(s) for %s continue serving", opts.ServiceSlug)
			}
			return result, err
		}
		// Wait for Traefik to pick up the new container's Docker labels and
		// settle routing before removing old containers or reporting active.
		d.waitForTraefikReady(ctx, opts, hc.Path, opts.OnLog)

		// Health check passed — sweep ALL stale containers for this service slug
		// (keeping only the newly deployed containerID).
		if useBlueGreen {
			cutoverDelay := d.cutoverDelay
			if cutoverDelay == 0 {
				cutoverDelay = 2 * time.Second
			}
			if cutoverDelay > 0 {
				// Brief delay for Traefik to fully route to new container
				select {
				case <-ctx.Done():
					return result, ctx.Err()
				case <-time.After(cutoverDelay):
				}
			}
			if cleaned := d.sweepStaleContainers(ctx, opts.ServiceSlug, containerID); cleaned > 0 {
				onProgress("deploying", "Traffic switched to new version", "", "")
			}
		}
		onProgress("active", fmt.Sprintf("Health check passed (attempt %d)", passedAttempt), shortID(containerID, 12), containerName)
		return result, nil
	}

	// No explicit path — use smart detection
	if isHTTP {
		// Auto-probe common health paths
		autoPath := autoDetectHealthPath(ip, effectivePort)
		if autoPath != "" {
			log.Printf("Auto-detected health path: %s", autoPath)
			hc.Path = autoPath
			result, passedAttempt, err := d.runHTTPHealthChecks(ctx, result, containerID, containerName, ip, effectivePort, hc, opts.OnLog)
			if err != nil {
				return result, err
			}
			d.waitForTraefikReady(ctx, opts, autoPath, opts.OnLog)
			onProgress("active", fmt.Sprintf("Health check passed (attempt %d)", passedAttempt), containerID[:12], containerName)
			return result, nil
		}
		// HTTP but no health path found — wait briefly for Traefik then mark active
		d.waitForTraefikReady(ctx, opts, "/", opts.OnLog)
		log.Println("HTTP port responding, no health path found — marking active")
		onProgress("active", "Deployment active (HTTP port responding)", containerID[:12], containerName)
		return result, nil
	}

	if primaryProtocol == "tcp" {
		if injectedHealthcheck != nil {
			// TCP port open, but we injected a HEALTHCHECK — wait for Docker to confirm healthy
			log.Printf("TCP port %d responding, waiting for Docker HEALTHCHECK...", effectivePort)
			if opts.OnLog != nil {
				opts.OnLog("TCP port responding. Waiting for database health check...")
			}
			if err := d.waitForDockerHealthy(ctx, containerID, 60*time.Second, opts.OnLog); err != nil {
				logs, _ := d.docker.GetStartupLogs(ctx, containerID, 100)
				result.StartupLogs = logs
				return result, fmt.Errorf("database health check failed: %w", err)
			}
			log.Println("Docker HEALTHCHECK passed — marking active")
			onProgress("active", "Deployment active (database health check passed)", containerID[:12], containerName)
			return result, nil
		}
		// No HEALTHCHECK — TCP connect = healthy (existing behavior)
		log.Println("TCP-only service — connection successful, marking active")
		onProgress("active", "Deployment active (TCP port responding)", containerID[:12], containerName)
		return result, nil
	}

	// Port is closed — wait and retry (slow startup)
	if primaryProtocol == "closed" {
		log.Printf("Primary port %d is closed, waiting for startup...", effectivePort)
		retries := 6
		interval := 10

		// Use health config retries/interval if higher than defaults.
		// These fields also govern HTTP health checks, but for the port-open
		// case they let users increase the startup timeout.
		if hc.Retries > retries {
			retries = hc.Retries
		}
		if hc.IntervalSeconds > interval {
			interval = hc.IntervalSeconds
		}

		// Image handlers override for known-slow images.
		if handler := matchesImageHandler(opts.ImageTag); handler != nil {
			if handler.startupRetries > 0 {
				retries = handler.startupRetries
			}
			if handler.startupInterval > 0 {
				interval = handler.startupInterval
			}
		}

		// StartupGraceSeconds adds an initial sleep before probing begins.
		if hc.StartupGraceSeconds > 0 {
			log.Printf("Startup grace: waiting %ds before port probing...", hc.StartupGraceSeconds)
			if opts.OnLog != nil {
				opts.OnLog(fmt.Sprintf("Waiting %ds startup grace before probing port %d...", hc.StartupGraceSeconds, effectivePort))
			}
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

			// Check container liveness — fail fast if container crashed
			ci2, inspErr := d.docker.InspectContainer(ctx, containerID)
			if inspErr == nil && (ci2.State == "exited" || ci2.State == "dead") {
				result.Inspection = ci2
				cLogs, logErr := d.docker.GetStartupLogs(ctx, containerID, 100)
				if logErr == nil {
					result.StartupLogs = cLogs
				}
				log.Printf("Container exited during startup wait (state=%s exit=%d oom=%v)", ci2.State, ci2.ExitCode, ci2.OOMKilled)
				if stopErr := d.docker.StopAndRemove(ctx, containerID); stopErr != nil {
					log.Printf("Warning: failed to remove container: %v", stopErr)
				}
				msg := fmt.Sprintf("Container exited during startup (exit code %d)", ci2.ExitCode)
				if ci2.OOMKilled {
					msg = "Container killed: out of memory (OOMKilled)"
				}
				return result, fmt.Errorf("%s", msg)
			}

			proto := docker.ProbePort(ctx, ip, effectivePort)
			if proto == "http" || (proto == "tcp" && injectedHealthcheck == nil) {
				log.Printf("Port %d now responding (%s) on attempt %d", effectivePort, proto, attempt)
				if proto == "http" {
					probePath := hc.Path
					if probePath == "" {
						probePath = "/"
					}
					d.waitForTraefikReady(ctx, opts, probePath, opts.OnLog)
				}
				onProgress("active", fmt.Sprintf("Deployment active (%s port responding, attempt %d)", proto, attempt), containerID[:12], containerName)
				return result, nil
			}
			if proto == "tcp" && injectedHealthcheck != nil {
				// TCP port open but we have an injected HEALTHCHECK — wait for Docker healthy
				log.Printf("Port %d responding (TCP) on attempt %d, waiting for Docker HEALTHCHECK...", effectivePort, attempt)
				if err := d.waitForDockerHealthy(ctx, containerID, 60*time.Second, opts.OnLog); err != nil {
					logs, _ := d.docker.GetStartupLogs(ctx, containerID, 100)
					result.StartupLogs = logs
					return result, fmt.Errorf("database health check failed: %w", err)
				}
				onProgress("active", "Deployment active (database health check passed)", containerID[:12], containerName)
				return result, nil
			}
			log.Printf("Port %d still closed (attempt %d/%d)", effectivePort, attempt, retries)
			if opts.OnLog != nil {
				opts.OnLog(fmt.Sprintf("Port %d still closed, container running (attempt %d/%d)", effectivePort, attempt, retries))
			}
		}

		// Still closed — re-probe common ports to detect if app started on a different port
		commonPorts := []int{effectivePort, 80, 443, 3000, 4000, 5000, 5173, 8000, 8080, 8443, 9000}
		if imageMeta != nil {
			commonPorts = append(commonPorts, imageMeta.ExposedPorts...)
		}
		finalProbe := docker.ProbeAllPorts(ctx, ip, commonPorts)
		result.Ports = finalProbe

		var openPorts []string
		for _, p := range finalProbe {
			if p.Protocol != "closed" && p.Port != effectivePort {
				openPorts = append(openPorts, fmt.Sprintf("%d (%s)", p.Port, p.Protocol))
			}
		}

		// Capture logs and fail
		logs, logErr := d.docker.GetStartupLogs(ctx, containerID, 100)
		if logErr == nil {
			result.StartupLogs = logs
		}
		log.Printf("Stopping failed container %s — port never opened", containerName)
		if stopErr := d.docker.StopAndRemove(ctx, containerID); stopErr != nil {
			log.Printf("Warning: failed to remove container: %v", stopErr)
		}

		errMsg := fmt.Sprintf("port %d never opened after %d attempts (~%ds total)", effectivePort, retries, retries*interval)
		if len(openPorts) > 0 {
			errMsg += fmt.Sprintf(". Detected open ports: %s — update your port setting?", strings.Join(openPorts, ", "))
		}
		return result, fmt.Errorf("%s", errMsg)
	}

	// Fallback: skip health checks
	log.Println("Health check skipped (no path configured, port status unknown)")
	onProgress("active", "Deployment active (health check skipped)", containerID[:12], containerName)
	return result, nil
}

// waitForDockerHealthy polls Docker's health status until the container reports "healthy".
func (d *Deployer) waitForDockerHealthy(ctx context.Context, containerID string, timeout time.Duration, onLog func(string)) error {
	deadline := time.Now().Add(timeout)
	attempt := 0
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		attempt++

		ci, err := d.docker.InspectContainer(ctx, containerID)
		if err == nil && (ci.State == "exited" || ci.State == "dead") {
			return fmt.Errorf("container exited (exit code %d)", ci.ExitCode)
		}

		health := d.docker.GetHealthStatus(ctx, containerID)
		if health == "healthy" {
			return nil
		}
		if health == "unhealthy" {
			return fmt.Errorf("Docker HEALTHCHECK reports unhealthy")
		}
		if onLog != nil && attempt%3 == 0 {
			onLog(fmt.Sprintf("Waiting for database health check (attempt %d, status: %s)...", attempt, health))
		}
	}
	return fmt.Errorf("timed out waiting for Docker HEALTHCHECK to pass after %v", timeout)
}

// runHTTPHealthChecks performs traditional HTTP health checks and handles failure cleanup.
// Returns the DeployResult, the 1-based attempt number that succeeded, and any error.
func (d *Deployer) runHTTPHealthChecks(
	ctx context.Context,
	result *DeployResult,
	containerID, containerName, ip string,
	port int,
	hc HealthConfig,
	onLog func(string),
) (*DeployResult, int, error) {
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
			return result, 0, ctx.Err()
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
			return result, attempt, nil
		}

		log.Printf("Health check failed (attempt %d/%d)", attempt, hc.Retries)
		if onLog != nil {
			onLog(fmt.Sprintf("Health check failed (attempt %d/%d)", attempt, hc.Retries))
		}
		if attempt < hc.Retries {
			select {
			case <-ctx.Done():
				return result, 0, ctx.Err()
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

	return result, 0, fmt.Errorf("health checks failed after %d attempts", hc.Retries)
}

// selectTraefikProbeTarget chooses the primary hostname and request path to
// probe through local Traefik (:80). Returns ("", "") if no public hostname or
// domain is configured.
func selectTraefikProbeTarget(opts DeployOpts, healthPath string) (string, string) {
	if healthPath == "" {
		healthPath = "/"
	} else if !strings.HasPrefix(healthPath, "/") {
		healthPath = "/" + healthPath
	}

	// Prefer endpoints whose :80 router proxies directly to the backend container
	// (so a 200-399 proves end-to-end Traefik → container routing).
	var fallbackHost string
	for _, ep := range opts.Endpoints {
		h := strings.TrimSpace(ep.Hostname)
		if h == "" || !safeDomainRe.MatchString(h) || len(h) > 253 {
			continue
		}
		switch ep.Provider {
		case "public_tunnel_cloudflare", "lan_only", "byo_proxy":
			return h, healthPath
		case "private_tailscale_https":
			path := healthPath
			if ep.PathPrefix != "" {
				path = strings.TrimSuffix(ep.PathPrefix, "/") + healthPath
			}
			return h, path
		default:
			if fallbackHost == "" {
				fallbackHost = h
			}
		}
	}
	if fallbackHost != "" {
		return fallbackHost, healthPath
	}

	for _, dom := range opts.Domains {
		h := strings.TrimSpace(dom)
		if h != "" && safeDomainRe.MatchString(h) && len(h) <= 253 {
			return h, healthPath
		}
	}

	return "", ""
}

// waitForTraefikReady probes the local Traefik HTTP endpoint with the service's
// primary Host header so Traefik finishes picking up Docker labels before we
// stop the old container or report "active". Always falls back gracefully if no
// hostname is configured, Traefik isn't listening on port 80, or the bounded
// timeout elapses.
func (d *Deployer) waitForTraefikReady(ctx context.Context, opts DeployOpts, healthPath string, onLog func(string)) {
	host, path := selectTraefikProbeTarget(opts, healthPath)
	if host == "" {
		return
	}

	endpoint := d.traefikEndpoint
	if endpoint == "" {
		endpoint = defaultTraefikEndpoint
	}
	timeout := d.traefikReadyTimeout
	if timeout <= 0 {
		timeout = defaultTraefikReadyTimeout
	}
	pollInterval := d.traefikPollInterval
	if pollInterval <= 0 {
		pollInterval = defaultTraefikPollInterval
	}
	client := d.traefikHTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: 2 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	probeURL := strings.TrimRight(endpoint, "/") + path
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log.Printf("Waiting for Traefik route readiness: %s (Host: %s, timeout: %s)", probeURL, host, timeout)
	if onLog != nil {
		onLog(fmt.Sprintf("Waiting for Traefik route to settle for %s%s...", host, path))
	}

	consecutiveDialErrors := 0
	lastStatus := 0
	var lastErr error

	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, nil)
		if err != nil {
			log.Printf("Traefik probe request creation failed: %v — skipping", err)
			return
		}
		req.Host = host

		resp, err := client.Do(req)
		if err == nil {
			consecutiveDialErrors = 0
			lastStatus = resp.StatusCode
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				log.Printf("Traefik route ready for %s%s (HTTP %d, attempt %d)", host, path, resp.StatusCode, attempt)
				if onLog != nil {
					onLog(fmt.Sprintf("Traefik route ready for %s%s (HTTP %d)", host, path, resp.StatusCode))
				}
				return
			}
		} else {
			lastErr = err
			// If Traefik isn't listening on port 80 (connection refused / dial error),
			// don't burn the entire timeout waiting.
			if isNetworkDialError(err) {
				consecutiveDialErrors++
				if consecutiveDialErrors >= 2 {
					log.Printf("Traefik not reachable at %s (%v) — skipping Traefik cutover wait", endpoint, err)
					return
				}
			}
		}

		select {
		case <-probeCtx.Done():
			if ctx.Err() != nil {
				return
			}
			log.Printf("Traefik route probe timed out after %s for %s%s (last_status=%d last_err=%v) — proceeding", timeout, host, path, lastStatus, lastErr)
			if onLog != nil {
				onLog(fmt.Sprintf("Traefik route probe timed out after %s — proceeding with deployment", timeout))
			}
			return
		case <-time.After(pollInterval):
		}
	}
}

func isNetworkDialError(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}

// autoDetectHealthPath tries common health check paths and returns the first
// one that returns 200-399, or empty string if none work.
func autoDetectHealthPath(ip string, port int) string {
	paths := []string{"/health", "/healthz", "/"}
	client := &http.Client{
		Timeout: 3 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
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
	client := &http.Client{
		Timeout: time.Duration(timeoutSec) * time.Second,
		// Don't follow redirects — a 302 proves the server is running.
		// Following redirects can fail when the target is HTTPS but the
		// container only speaks HTTP (TLS handshake → 400 Bad Request).
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

func generateTraefikLabels(deploymentID, serviceSlug string, domains []string, port int, endpoints []EndpointInfo, lanIPs []string, isHTTP bool, healthCheckPath ...string) map[string]string {
	labels := map[string]string{
		"clank.managed":           "true",
		"clank.ownership_version": "1",
		"clank.artifact_type":     "service_container",
		"clank.service_slug":      serviceSlug,
		"clank.deployment_id":     deploymentID,
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

	// Traefik-level health check: prevents routing to unready containers
	// during blue-green deploys (Traefik only routes after its own probe passes).
	hcPath := ""
	if len(healthCheckPath) > 0 {
		hcPath = healthCheckPath[0]
	}
	if hcPath != "" {
		labels[fmt.Sprintf("traefik.http.services.%s.loadbalancer.healthcheck.path", svcName)] = hcPath
		labels[fmt.Sprintf("traefik.http.services.%s.loadbalancer.healthcheck.interval", svcName)] = "5s"
		labels[fmt.Sprintf("traefik.http.services.%s.loadbalancer.healthcheck.timeout", svcName)] = "3s"
		labels[fmt.Sprintf("traefik.http.services.%s.loadbalancer.healthcheck.followredirects", svcName)] = "false"
	}

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

	// HTTPS → HTTP redirect for legacy/sslip.io domains.
	// If Traefik is also serving port 443 (for endpoints with TLS), browsers
	// may auto-upgrade these HTTP-only hostnames to HTTPS and get a 404.
	// This low-priority HTTPS router catches those requests and redirects
	// them back to HTTP. Endpoint-specific HTTPS routers (Let's Encrypt,
	// Tailscale) have higher priority and take precedence.
	httpsRouter := routerName + "-https-redir"
	mwName := routerName + "-to-http"
	labels[fmt.Sprintf("traefik.http.routers.%s.rule", httpsRouter)] = hostRules
	labels[fmt.Sprintf("traefik.http.routers.%s.entrypoints", httpsRouter)] = "websecure"
	labels[fmt.Sprintf("traefik.http.routers.%s.tls", httpsRouter)] = "true"
	labels[fmt.Sprintf("traefik.http.routers.%s.middlewares", httpsRouter)] = mwName
	labels[fmt.Sprintf("traefik.http.routers.%s.priority", httpsRouter)] = "1"
	labels[fmt.Sprintf("traefik.http.middlewares.%s.redirectscheme.scheme", mwName)] = "http"
	labels[fmt.Sprintf("traefik.http.middlewares.%s.redirectscheme.permanent", mwName)] = "false"

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

	if resolvedURL != "" {
		for key, value := range env {
			env[key] = strings.ReplaceAll(value, "{{CLANK_PUBLIC_URL}}", resolvedURL)
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
// Without --bind lan the gateway binds to loopback only (unreachable from
// outside the container in Docker bridge networking).
//
// The gateway uses token authentication and an explicit origin allowlist.
// Both http:// and https:// origins are allowed so the UI works while a
// certificate is being provisioned, without trusting arbitrary Host headers.
func injectOpenClawEnvVars(env map[string]string, resolvedURL, pathPrefix string) {
	if _, ok := env["OPENCLAW_GATEWAY_TOKEN"]; !ok {
		env["OPENCLAW_GATEWAY_TOKEN"] = generateHexToken(16)
	}
	if _, ok := env["NODE_OPTIONS"]; !ok {
		env["NODE_OPTIONS"] = "--max-old-space-size=3584"
	}
	if _, ok := env["CLANK_CONTAINER_CMD"]; !ok {
		// Build a proper nested JSON config file instead of using dotted
		// "config set" keys — dotted keys may not be parsed as nested config
		// by OpenClaw, causing allowedOrigins and other settings to silently
		// fail.
		origins := []string{}
		if resolvedURL != "" {
			hostname := resolvedURL
			for _, prefix := range []string{"https://", "http://"} {
				hostname = strings.TrimPrefix(hostname, prefix)
			}
			if idx := strings.Index(hostname, "/"); idx > 0 {
				hostname = hostname[:idx]
			}
			// Strip any port for domain validation, or validate host[:port] safely
			hostOnly := hostname
			if colonIdx := strings.LastIndex(hostname, ":"); colonIdx > 0 {
				hostOnly = hostname[:colonIdx]
			}
			if safeDomainRe.MatchString(hostOnly) && !strings.ContainsAny(hostname, "'\"\\\r\n\x00") {
				origins = append(origins, fmt.Sprintf("https://%s", hostname), fmt.Sprintf("http://%s", hostname))
			} else {
				log.Printf("Rejected unsafe OpenClaw origin hostname: %q", hostname)
			}
		}

		type openClawAuth struct {
			Mode string `json:"mode"`
		}
		type openClawControlUI struct {
			AllowedOrigins []string `json:"allowedOrigins"`
		}
		type openClawGateway struct {
			Mode           string            `json:"mode"`
			Bind           string            `json:"bind"`
			TrustedProxies []string          `json:"trustedProxies"`
			Auth           openClawAuth      `json:"auth"`
			ControlUI      openClawControlUI `json:"controlUi"`
		}
		type openClawConfig struct {
			Gateway openClawGateway `json:"gateway"`
		}

		cfgBytes, err := json.Marshal(openClawConfig{
			Gateway: openClawGateway{
				Mode:           "local",
				Bind:           "lan",
				TrustedProxies: []string{"172.16.0.0/12", "192.168.0.0/16", "10.0.0.0/8"},
				Auth:           openClawAuth{Mode: "token"},
				ControlUI:      openClawControlUI{AllowedOrigins: origins},
			},
		})
		if err != nil {
			log.Printf("Failed to marshal OpenClaw config: %v", err)
			return
		}

		// Escape any single quotes for POSIX single-quoted shell literal safety.
		escapedJSON := escapePOSIXSingleQuotes(string(cfgBytes))
		writeConfig := fmt.Sprintf(`mkdir -p ~/.openclaw && printf '%%s' '%s' > ~/.openclaw/openclaw.json`, escapedJSON)

		env["CLANK_CONTAINER_CMD"] = writeConfig + " && exec node dist/index.js gateway --allow-unconfigured --auth token"
	}
}

// escapePOSIXSingleQuotes safely escapes single quotes for embedding inside '...' in POSIX sh.
func escapePOSIXSingleQuotes(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

// escapePHPSingleQuoted safely escapes backslashes and single quotes for PHP single-quoted strings.
func escapePHPSingleQuoted(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return s
}

func shortID(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}

// buildWordPressConfig generates the PHP snippet for WORDPRESS_CONFIG_EXTRA.
// If pathPrefix is set, it includes $_SERVER fixups for path-prefix routing.
// Otherwise it just sets WP_HOME/WP_SITEURL to the resolved URL.
func buildWordPressConfig(resolvedURL, pathPrefix string) string {
	if resolvedURL == "" {
		return ""
	}
	if strings.ContainsAny(resolvedURL, "\r\n\x00") || strings.ContainsAny(pathPrefix, "\r\n\x00") {
		log.Printf("Rejected unsafe control characters in WordPress config inputs")
		return ""
	}
	if !strings.HasPrefix(resolvedURL, "http://") && !strings.HasPrefix(resolvedURL, "https://") {
		log.Printf("Rejected unsafe WordPress URL scheme: %q", resolvedURL)
		return ""
	}

	if pathPrefix != "" {
		// Split URL into host-part and path-part for the PHP variables.
		hostURL := strings.TrimSuffix(resolvedURL, pathPrefix)
		config := fmt.Sprintf(
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
			escapePHPSingleQuoted(pathPrefix), escapePHPSingleQuoted(hostURL),
		)
		if strings.HasPrefix(resolvedURL, "https://") {
			config += "\n$_SERVER['HTTPS'] = 'on';"
		}
		return config
	}

	// No path prefix — just set canonical URL.
	safeURL := escapePHPSingleQuoted(resolvedURL)
	config := fmt.Sprintf(
		"define('WP_HOME', '%s');\ndefine('WP_SITEURL', '%s');",
		safeURL, safeURL,
	)
	// When behind a TLS-terminating proxy, tell WordPress the connection is
	// secure. WordPress checks X-Forwarded-Proto by default but this is a
	// belt-and-suspenders measure for proxy configs that don't forward it.
	if strings.HasPrefix(resolvedURL, "https://") {
		config += "\n$_SERVER['HTTPS'] = 'on';"
	}
	return config
}

// resolveTailscaleHostname discovers the machine's tailnet DNS name.
func resolveTailscaleHostname() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
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
