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

// DeployOpts configures a deployment.
type DeployOpts struct {
	DeploymentID    string
	ServiceSlug     string
	ImageTag        string
	Env             map[string]string
	Port            int
	Domains         []string
	HealthCheckPath string
	HealthConfig    HealthConfig
	CPULimit        float64
	MemoryLimitMB   int
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

	// Ensure services network exists
	if err := d.docker.EnsureNetwork(ctx, servicesNetwork); err != nil {
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

	// Generate Traefik labels
	labels := generateTraefikLabels(opts.ServiceSlug, opts.Domains, opts.Port)

	// Container name
	containerName := fmt.Sprintf("clank-%s-%s", opts.ServiceSlug, opts.DeploymentID[:8])

	// Start container
	containerID, err := d.docker.RunContainer(ctx, docker.RunOpts{
		Image:         opts.ImageTag,
		Name:          containerName,
		Env:           opts.Env,
		Port:          opts.Port,
		Labels:        labels,
		Network:       servicesNetwork,
		CPULimit:      opts.CPULimit,
		MemoryLimitMB: opts.MemoryLimitMB,
	})
	if err != nil {
		return fmt.Errorf("starting container: %w", err)
	}

	log.Printf("Container %s started (%s)", containerName, containerID[:12])
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

	// Get container IP for health checks
	ip, err := d.docker.GetContainerIP(ctx, containerID, servicesNetwork)
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

func generateTraefikLabels(serviceSlug string, domains []string, port int) map[string]string {
	routerName := "clank-" + serviceSlug

	// Filter domains through sanitizer
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

	// Build Host() rules
	var rules []string
	for _, d := range safeDomains {
		rules = append(rules, fmt.Sprintf("Host(`%s`)", d))
	}
	hostRules := strings.Join(rules, " || ")

	labels := map[string]string{
		"traefik.enable": "true",
		fmt.Sprintf("traefik.http.routers.%s.rule", routerName):                         hostRules,
		fmt.Sprintf("traefik.http.routers.%s.entrypoints", routerName):                  "web",
		fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", routerName):     fmt.Sprintf("%d", port),
		"clank.managed":      "true",
		"clank.service_slug": serviceSlug,
	}

	return labels
}
