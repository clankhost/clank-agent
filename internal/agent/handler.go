package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	clankv1 "github.com/anaremore/clank/apps/agent/gen/clank/v1"
	"github.com/anaremore/clank/apps/agent/internal/backup"
	"github.com/anaremore/clank/apps/agent/internal/build"
	"github.com/anaremore/clank/apps/agent/internal/deploy"
	"github.com/anaremore/clank/apps/agent/internal/docker"
	"github.com/anaremore/clank/apps/agent/internal/endpoint"
	"github.com/anaremore/clank/apps/agent/internal/grpcclient"
	"github.com/anaremore/clank/apps/agent/internal/logs"
	"github.com/anaremore/clank/apps/agent/internal/selfupdate"
	"github.com/anaremore/clank/apps/agent/internal/sysinfo"
)

// Terminal deploy statuses — if Send fails for these, queue for retry.
var terminalDeployStatuses = map[string]bool{
	"active":       true,
	"failed":       true,
	"build_failed": true,
}

// CommandHandler processes commands received from the control plane.
type CommandHandler struct {
	docker         *docker.Manager
	builder        *build.Builder
	deployer       *deploy.Deployer
	endpointMgr    *endpoint.Manager
	cfg            *Config
	cfgDir         string
	currentVersion string
	logCollector   *logs.Collector

	// pendingResults holds deploy progress messages whose Send failed
	// (stream broke mid-deploy). Drained on the next successful connection.
	pendingMu      sync.Mutex
	pendingResults []*clankv1.AgentMessage

	// activeDeploysMu guards activeDeploys — slugs with in-progress or
	// recently completed deploys. REMOVE commands for these slugs are
	// skipped to prevent stale cleanup commands from killing containers.
	activeDeploysMu sync.Mutex
	activeDeploys   map[string]time.Time // value = expiry time
}

// NewCommandHandler creates a handler with all agent capabilities.
func NewCommandHandler(dm *docker.Manager, b *build.Builder, d *deploy.Deployer, cfg *Config, cfgDir string, version string, lc *logs.Collector) *CommandHandler {
	// Initialize endpoint providers
	epMgr := endpoint.NewManager(
		&endpoint.LANProvider{},
		&endpoint.DirectProvider{},
		endpoint.NewCFTunnelProvider(dm),
		&endpoint.TailscaleProvider{},
		&endpoint.BYOProxyProvider{},
	)

	return &CommandHandler{
		docker:         dm,
		builder:        b,
		deployer:       d,
		endpointMgr:    epMgr,
		cfg:            cfg,
		cfgDir:         cfgDir,
		currentVersion: version,
		logCollector:   lc,
	}
}

// queuePendingResult stores a message for retry on the next connection.
func (h *CommandHandler) queuePendingResult(msg *clankv1.AgentMessage) {
	h.pendingMu.Lock()
	defer h.pendingMu.Unlock()
	// Cap at 64 to prevent unbounded growth if agent stays disconnected
	if len(h.pendingResults) < 64 {
		h.pendingResults = append(h.pendingResults, msg)
		log.Printf("Queued pending deploy result (%d in queue)", len(h.pendingResults))
	}
}

// DrainPendingResults sends any queued deploy results on the given stream.
// Called by the agent after establishing a new connection.
func (h *CommandHandler) DrainPendingResults(stream grpcclient.ConnectStream) {
	h.pendingMu.Lock()
	results := h.pendingResults
	h.pendingResults = nil
	h.pendingMu.Unlock()

	if len(results) == 0 {
		return
	}

	log.Printf("Draining %d pending deploy result(s) on new connection", len(results))
	for _, msg := range results {
		if err := stream.Send(msg); err != nil {
			log.Printf("Failed to send pending result on new stream: %v (re-queuing)", err)
			h.queuePendingResult(msg)
		} else {
			// Log what we sent
			if dp := msg.GetDeployProgress(); dp != nil {
				log.Printf("Sent pending deploy result: deployment=%s status=%s", dp.DeploymentId, dp.Status)
			}
		}
	}
}

// deployGracePeriod is how long after a successful deploy the slug stays
// protected from stale REMOVE commands sent by the control plane.
const deployGracePeriod = 5 * time.Minute

func (h *CommandHandler) markDeployActive(slug string) {
	h.activeDeploysMu.Lock()
	defer h.activeDeploysMu.Unlock()
	if h.activeDeploys == nil {
		h.activeDeploys = make(map[string]time.Time)
	}
	// Use a far-future expiry while deploying; replaced by real expiry on completion.
	h.activeDeploys[slug] = time.Now().Add(1 * time.Hour)
}

func (h *CommandHandler) markDeployDone(slug string, success bool) {
	h.activeDeploysMu.Lock()
	defer h.activeDeploysMu.Unlock()
	if success {
		// Keep protected for grace period after successful deploy.
		h.activeDeploys[slug] = time.Now().Add(deployGracePeriod)
		log.Printf("Deploy guard for slug %s extended for %s", slug, deployGracePeriod)
	} else {
		delete(h.activeDeploys, slug)
	}
}

func (h *CommandHandler) isDeployActive(slug string) bool {
	h.activeDeploysMu.Lock()
	defer h.activeDeploysMu.Unlock()
	expiry, ok := h.activeDeploys[slug]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(h.activeDeploys, slug)
		return false
	}
	return true
}

// HandleDeploy processes a DeployCommand — clone+build or image pull, then deploy.
func (h *CommandHandler) HandleDeploy(ctx context.Context, stream grpcclient.ConnectStream, cmd *clankv1.DeployCommand) {
	deployID := cmd.GetDeploymentId()
	slug := cmd.GetServiceSlug()
	log.Printf("Handling deploy command for deployment %s (service: %s)", deployID, slug)

	// Guard: mark this slug as deploying so concurrent REMOVE commands
	// don't kill the container while it's still starting up.
	// On success, the guard extends for deployGracePeriod to absorb
	// stale REMOVE waves from the control plane.
	h.markDeployActive(slug)
	deploySuccess := false
	defer func() { h.markDeployDone(slug, deploySuccess) }()

	// buildEffectivePort is set after the build phase if the auto-generated
	// Dockerfile uses a different port than the command specified.
	var buildEffectivePort int32

	// sendProgress sends intermediate progress updates (no introspection attached).
	sendProgress := func(status, message, containerID, containerName, imageTag, gitSHA string) {
		msg := &clankv1.AgentMessage{
			Payload: &clankv1.AgentMessage_DeployProgress{
				DeployProgress: &clankv1.DeployProgress{
					DeploymentId:  deployID,
					Status:        status,
					Message:       message,
					ContainerId:   containerID,
					ContainerName: containerName,
					ImageTag:      imageTag,
					GitSha:        gitSHA,
					EffectivePort: buildEffectivePort,
				},
			},
		}
		if err := stream.Send(msg); err != nil {
			log.Printf("Failed to send deploy progress: %v", err)
			if terminalDeployStatuses[status] {
				h.queuePendingResult(msg)
			}
		}
	}

	// sendTerminalProgress sends a terminal (active/failed) progress with
	// introspection data and startup logs attached.
	sendTerminalProgress := func(status, message, containerID, containerName, imageTag, gitSHA string, result *deploy.DeployResult) {
		dp := &clankv1.DeployProgress{
			DeploymentId:  deployID,
			Status:        status,
			Message:       message,
			ContainerId:   containerID,
			ContainerName: containerName,
			ImageTag:      imageTag,
			GitSha:        gitSHA,
		}

		if result != nil {
			// Attach startup logs (Phase A)
			if result.StartupLogs != "" {
				// Truncate to 4KB for the proto message
				logs := result.StartupLogs
				if len(logs) > 4096 {
					logs = logs[:4096]
				}
				dp.StartupLogs = logs
			}

			// Attach effective port (Phase D)
			if result.EffectivePort > 0 {
				dp.EffectivePort = int32(result.EffectivePort)
			}

			// Build ContainerIntrospection (Phase B)
			dp.Introspection = buildIntrospectionProto(result)
		}

		msg := &clankv1.AgentMessage{
			Payload: &clankv1.AgentMessage_DeployProgress{
				DeployProgress: dp,
			},
		}
		if err := stream.Send(msg); err != nil {
			log.Printf("Failed to send deploy progress: %v", err)
			if terminalDeployStatuses[status] {
				h.queuePendingResult(msg)
			}
		}
	}

	// buildLog injects a build/deploy log line into the StreamLogs channel
	// so it appears in the UI in real-time.
	buildLog := func(line string) {
		if h.logCollector != nil {
			h.logCollector.Inject(&clankv1.LogEntry{
				DeploymentId: deployID,
				Line:         line,
				TimestampNs:  time.Now().UnixNano(),
				Stream:       "build",
			})
		}
	}

	imageTag := cmd.GetImageTag()
	var gitSHA string

	// Phase 1: Build (if repo_url is set)
	if cmd.GetRepoUrl() != "" {
		sendProgress("cloning", fmt.Sprintf("Cloning %s...", cmd.GetRepoUrl()), "", "", "", "")

		result, err := h.builder.BuildFromSource(
			ctx,
			cmd.GetRepoUrl(),
			cmd.GetBranch(),
			cmd.GetGitToken(),
			cmd.GetDockerfilePath(),
			cmd.GetServiceSlug(),
			deployID,
			int(cmd.GetPort()),
			func(status, message string) {
				sendProgress(status, message, "", "", "", "")
			},
			buildLog,
		)
		if err != nil {
			sendProgress("build_failed", fmt.Sprintf("Build failed: %v", err), "", "", "", "")
			return
		}

		imageTag = result.ImageTag
		gitSHA = result.GitSHA

		// Propagate effective port and health path from build detection.
		// The auto-generated Dockerfile may use a different port than the
		// command specified (e.g., SPA → nginx on port 80 vs default 8080).
		if result.EffectivePort > 0 && result.EffectivePort != int(cmd.GetPort()) {
			log.Printf("[deploy] Build detected effective port %d (was %d)", result.EffectivePort, cmd.GetPort())
			buildEffectivePort = int32(result.EffectivePort)
		}

		sendProgress("built", "Build complete", "", "", imageTag, gitSHA)
	}

	if imageTag == "" {
		sendProgress("failed", "No image to deploy (no repo_url and no image_tag)", "", "", "", "")
		return
	}

	// Determine the deploy port: prefer build-detected port over command port.
	deployPort := int(cmd.GetPort())
	if buildEffectivePort > 0 {
		deployPort = int(buildEffectivePort)
	}

	// Phase 2: Deploy
	hc := cmd.GetHealthConfig()
	var healthConfig deploy.HealthConfig
	if hc != nil {
		healthConfig = deploy.HealthConfig{
			Path:                hc.GetPath(),
			TimeoutSeconds:      int(hc.GetTimeoutSeconds()),
			Retries:             int(hc.GetRetries()),
			IntervalSeconds:     int(hc.GetIntervalSeconds()),
			StartupGraceSeconds: int(hc.GetStartupGraceSeconds()),
		}
	}

	rc := cmd.GetResourceConfig()
	var cpuLimit float64
	var memoryLimitMB int
	if rc != nil {
		cpuLimit = rc.GetCpuLimit()
		memoryLimitMB = int(rc.GetMemoryLimitMb())
	}

	// Map proto EndpointInfo to deploy.EndpointInfo
	var endpoints []deploy.EndpointInfo
	for _, ep := range cmd.GetActiveEndpoints() {
		endpoints = append(endpoints, deploy.EndpointInfo{
			EndpointID: ep.GetEndpointId(),
			Provider:   ep.GetProvider(),
			Hostname:   ep.GetHostname(),
			PathPrefix: ep.GetPathPrefix(),
			TLSMode:    ep.GetTlsMode(),
		})
	}

	// Collect all host IPs for sslip.io routing labels (LAN + public)
	netInfo := sysinfo.CollectNetworkInfo()
	hostIPs := make([]string, 0, len(netInfo.LANIPs)+1)
	hostIPs = append(hostIPs, netInfo.LANIPs...)
	if netInfo.PublicIP != "" {
		hostIPs = append(hostIPs, netInfo.PublicIP)
	}

	// Map proto volume mounts to deploy opts
	var volumes []docker.VolumeMount
	for _, vm := range cmd.GetVolumeMounts() {
		volumes = append(volumes, docker.VolumeMount{
			Name:      vm.GetName(),
			MountPath: vm.GetMountPath(),
		})
	}

	deployResult, err := h.deployer.Deploy(ctx, deploy.DeployOpts{
		DeploymentID:     deployID,
		ServiceSlug:      cmd.GetServiceSlug(),
		ImageTag:         imageTag,
		Env:              cmd.GetEnvVars(),
		Port:             deployPort,
		Domains:          cmd.GetDomains(),
		Endpoints:        endpoints,
		HealthCheckPath:  cmd.GetHealthCheckPath(),
		HealthConfig:     healthConfig,
		CPULimit:         cpuLimit,
		MemoryLimitMB:    memoryLimitMB,
		ProjectNetwork:   cmd.GetProjectNetwork(),
		LANIPs:           hostIPs,
		Volumes:          volumes,
		ContainerCommand: cmd.GetContainerCommand(),
		OnLog:            buildLog,
	}, func(status, message, containerID, containerName string) {
		sendProgress(status, message, containerID, containerName, imageTag, gitSHA)
	})

	if err != nil {
		sendTerminalProgress("failed", fmt.Sprintf("Deploy failed: %v", err), "", "", imageTag, gitSHA, deployResult)
	} else {
		deploySuccess = true
		// The deployer already called onProgress("active", ...) for intermediate
		// progress. Send a terminal message with introspection attached.
		// Note: the deployer's onProgress already reported "active" to the stream,
		// but that message didn't have introspection. We don't re-send "active" here
		// because the first one already transitioned the deployment state.
		// Instead, we only attach introspection to failed deployments.
		// For active deploys, the deployer's onProgress("active",...) suffices,
		// but we want introspection too. Let's send it as part of the active message.
		// We need to check if the deployer already sent the terminal "active".
		// Since it did via onProgress, and we can't un-send, we send a supplementary
		// message. However, the API ignores duplicate "active" transitions.
		// So we send another "active" with introspection attached.
		if deployResult != nil && (deployResult.Inspection != nil || len(deployResult.Ports) > 0 || deployResult.EffectivePort != int(cmd.GetPort())) {
			sendTerminalProgress("active", "Deployment active (introspection attached)", "", "", imageTag, gitSHA, deployResult)
		}
	}
}

// buildIntrospectionProto converts a DeployResult into a proto ContainerIntrospection.
func buildIntrospectionProto(result *deploy.DeployResult) *clankv1.ContainerIntrospection {
	if result == nil {
		return nil
	}

	intro := &clankv1.ContainerIntrospection{}

	// Discovered ports
	for _, p := range result.Ports {
		intro.DiscoveredPorts = append(intro.DiscoveredPorts, &clankv1.DiscoveredPort{
			Port:     int32(p.Port),
			Protocol: p.Protocol,
			Source:   p.Source,
		})
	}

	// Container inspection
	if result.Inspection != nil {
		intro.ContainerIp = result.Inspection.IP
		intro.Networks = result.Inspection.Networks
		intro.ExitCode = int32(result.Inspection.ExitCode)
		intro.OomKilled = result.Inspection.OOMKilled
	}

	// Image metadata
	if result.ImageMeta != nil {
		for _, p := range result.ImageMeta.ExposedPorts {
			intro.ImageExposePorts = append(intro.ImageExposePorts, int32(p))
		}
		intro.ImageCmd = result.ImageMeta.Cmd
		intro.ImageEntrypoint = result.ImageMeta.Entrypoint
		intro.HasImageHealthcheck = result.ImageMeta.Healthcheck != nil
	}

	return intro
}

// HandleContainerCommand processes a ContainerCommand (stop/start/restart/remove).
func (h *CommandHandler) HandleContainerCommand(ctx context.Context, stream grpcclient.ConnectStream, cmd *clankv1.ContainerCommand) {
	commandID := cmd.GetCommandId()
	containerName := cmd.GetContainerName()
	action := cmd.GetAction()

	log.Printf("Handling container command %s: %s on %s", commandID, action, containerName)

	// Find container by name
	containerID, _, err := h.docker.FindContainerByLabel(ctx, "clank.managed", "true")
	if err != nil || containerID == "" {
		// Try by container name directly
		// For simplicity, we use the container name from the command
	}

	// Guard: skip REMOVE for slugs with in-progress deploys to avoid
	// killing a container that's still starting up.
	if action == clankv1.ContainerCommand_REMOVE {
		slug := cmd.GetServiceSlug()
		if slug != "" && h.isDeployActive(slug) {
			log.Printf("Skipping REMOVE for slug %s — deploy in progress", slug)
			msg := &clankv1.AgentMessage{
				Payload: &clankv1.AgentMessage_CommandResult{
					CommandResult: &clankv1.CommandResult{
						CommandId: commandID,
						Success:   true,
						Output:    fmt.Sprintf("Skipped: deploy in progress for %s", slug),
					},
				},
			}
			if err := stream.Send(msg); err != nil {
				log.Printf("Failed to send command result: %v", err)
			}
			return
		}
	}

	var execErr error
	switch action {
	case clankv1.ContainerCommand_STOP:
		execErr = h.docker.StopContainer(ctx, containerName)
	case clankv1.ContainerCommand_START:
		execErr = h.docker.StartContainer(ctx, containerName)
	case clankv1.ContainerCommand_RESTART:
		execErr = h.docker.RestartContainer(ctx, containerName)
	case clankv1.ContainerCommand_REMOVE:
		execErr = h.removeServiceContainers(ctx, cmd)
	default:
		execErr = fmt.Errorf("unknown action: %v", action)
	}

	success := execErr == nil
	output := ""
	if execErr != nil {
		output = execErr.Error()
		log.Printf("Container command %s failed: %v", commandID, execErr)
	} else {
		output = fmt.Sprintf("Container %s %s successful", containerName, action)
		log.Printf("Container command %s completed successfully", commandID)
	}

	// Send result back
	msg := &clankv1.AgentMessage{
		Payload: &clankv1.AgentMessage_CommandResult{
			CommandResult: &clankv1.CommandResult{
				CommandId: commandID,
				Success:   success,
				Output:    output,
			},
		},
	}
	if err := stream.Send(msg); err != nil {
		log.Printf("Failed to send command result: %v", err)
	}
}

// removeServiceContainers stops and removes all containers for a service slug,
// then cleans up build images.
func (h *CommandHandler) removeServiceContainers(ctx context.Context, cmd *clankv1.ContainerCommand) error {
	slug := cmd.GetServiceSlug()
	if slug == "" {
		// Fallback: just stop+remove the named container
		name := cmd.GetContainerName()
		if name == "" {
			return fmt.Errorf("REMOVE requires service_slug or container_name")
		}
		return h.docker.StopAndRemove(ctx, name)
	}

	containers, err := h.docker.ListContainersByLabel(ctx, "clank.service_slug", slug)
	if err != nil {
		return fmt.Errorf("listing containers for slug %s: %w", slug, err)
	}

	var lastErr error
	for _, c := range containers {
		id := c.ContainerID
		if id == "" {
			id = c.Name
		}
		shortID := c.ContainerID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		log.Printf("Removing container %s (%s) for service slug %s", c.Name, shortID, slug)
		if err := h.docker.StopAndRemove(ctx, id); err != nil {
			log.Printf("Warning: failed to remove container %s: %v", c.Name, err)
			lastErr = err
		}
	}

	// Remove build images (best-effort)
	h.docker.RemoveImages(ctx, fmt.Sprintf("clank-%s:", slug))

	// Remove Docker volumes if explicitly requested
	for _, volName := range cmd.GetVolumeNames() {
		if volName == "" {
			continue
		}
		log.Printf("Removing volume %s for service slug %s", volName, slug)
		if err := h.docker.RemoveVolume(ctx, volName); err != nil {
			log.Printf("Warning: failed to remove volume %s: %v", volName, err)
		}
	}

	// Best-effort: remove the project network if now empty.
	// Uses the traefik.docker.network label already set on deployed containers.
	if len(containers) > 0 {
		if netName, ok := containers[0].Labels["traefik.docker.network"]; ok && netName != "" {
			nets, listErr := h.docker.ListClankProjectNetworks(ctx)
			if listErr == nil {
				for _, n := range nets {
					if n.Name == netName {
						removed, rmErr := h.docker.RemoveNetworkIfEmpty(ctx, n.ID)
						if rmErr != nil {
							log.Printf("Warning: failed to check/prune network %s: %v", netName, rmErr)
						} else if removed {
							log.Printf("Pruned empty project network %s after service removal", netName)
						}
						break
					}
				}
			}
		}
	}

	if lastErr != nil {
		return fmt.Errorf("some containers failed to remove: %w", lastErr)
	}
	if len(containers) == 0 {
		log.Printf("No containers found for slug %s (already cleaned up)", slug)
	}
	return nil
}

// HandleUpdate downloads a new agent binary, replaces the current one,
// sends an ACK to the control plane, and exits so systemd can restart
// with the new version. On transient download failures, retries up to 3 times.
func (h *CommandHandler) HandleUpdate(ctx context.Context, stream grpcclient.ConnectStream, cmd *clankv1.UpdateCommand) {
	newVersion := cmd.GetVersion()
	log.Printf("Self-update: %s → %s", h.currentVersion, newVersion)

	// Write state file before attempting update (for crash recovery).
	// Use the binary directory (always writable under systemd sandbox)
	// instead of cfgDir which may be in a read-only home directory.
	binDir := selfupdate.BinDir()
	selfupdate.SaveState(binDir, &selfupdate.UpdateState{
		Status:      "pending",
		FromVersion: h.currentVersion,
		ToVersion:   newVersion,
		Attempts:    0,
	})

	// Retry loop with backoff for transient failures
	backoffs := []time.Duration{10 * time.Second, 30 * time.Second, 60 * time.Second}
	var lastErr error

	for attempt := 0; attempt <= len(backoffs); attempt++ {
		if attempt > 0 {
			wait := backoffs[attempt-1]
			log.Printf("[update] Retry %d/%d in %s...", attempt, len(backoffs), wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}

		lastErr = selfupdate.BackupAndApply(
			cmd.GetDownloadUrl(),
			cmd.GetSha256(),
			cmd.GetSignature(),
			h.currentVersion,
			newVersion,
		)

		if lastErr == nil {
			break
		}

		log.Printf("[update] Attempt %d failed: %v", attempt+1, lastErr)

		// Only retry on transient (download) errors
		if !selfupdate.IsRetryable(lastErr) {
			break
		}
	}

	if lastErr != nil {
		// Send failure ACK
		log.Printf("Self-update failed: %v", lastErr)
		selfupdate.ClearState(binDir)
		h.sendUpdateResult(stream, newVersion, false, lastErr)
		return
	}

	// Send success ACK
	h.sendUpdateResult(stream, newVersion, true, nil)

	// Brief sleep to let the ACK flush on the wire
	time.Sleep(500 * time.Millisecond)

	log.Printf("Self-update to %s complete, exiting for restart...", newVersion)
	os.Exit(0)
}

// sendUpdateResult sends an UpdateResult message back to the control plane.
func (h *CommandHandler) sendUpdateResult(stream grpcclient.ConnectStream, toVersion string, success bool, err error) {
	result := &clankv1.UpdateResult{
		FromVersion: h.currentVersion,
		ToVersion:   toVersion,
		Success:     success,
	}
	if err != nil {
		result.ErrorMessage = err.Error()
		result.FailedPhase = selfupdate.ErrorPhase(err)
	}

	msg := &clankv1.AgentMessage{
		Payload: &clankv1.AgentMessage_UpdateResult{
			UpdateResult: result,
		},
	}
	if sendErr := stream.Send(msg); sendErr != nil {
		log.Printf("Failed to send update result: %v", sendErr)
	}
}

// HandleEndpoint processes an EndpointCommand (ensure/disable/doctor).
func (h *CommandHandler) HandleEndpoint(ctx context.Context, stream grpcclient.ConnectStream, cmd *clankv1.EndpointCommand) {
	cfg := endpoint.ProviderConfig{
		EndpointID:  cmd.GetEndpointId(),
		ServiceSlug: cmd.GetServiceSlug(),
		Hostname:    cmd.GetHostname(),
		PathPrefix:  cmd.GetPathPrefix(),
		Port:        int(cmd.GetPort()),
		TLSMode:     cmd.GetTlsMode(),
		Config:      cmd.GetProviderConfig(),
	}

	providerName := cmd.GetProvider()

	// For public_direct, ensure Traefik has ACME before proceeding
	if providerName == "public_direct" && cmd.GetAction() == clankv1.EndpointCommand_ENSURE {
		if !h.docker.HasACME(ctx) {
			log.Printf("Upgrading Traefik to ACME for public_direct endpoint")
			netInfo := sysinfo.CollectNetworkInfo()
			if err := h.docker.ReconfigureTraefikACME(ctx, netInfo.TraefikBindIP()); err != nil {
				log.Printf("Warning: ACME reconfiguration failed: %v", err)
			}
		}
	}

	var result *endpoint.ProviderStatus
	var err error

	switch cmd.GetAction() {
	case clankv1.EndpointCommand_ENSURE:
		result, err = h.endpointMgr.HandleEnsure(ctx, cfg, providerName)
	case clankv1.EndpointCommand_DISABLE:
		result, err = h.endpointMgr.HandleDisable(ctx, cfg, providerName)
	case clankv1.EndpointCommand_DOCTOR:
		result, err = h.endpointMgr.HandleDoctor(ctx, cfg, providerName)
	default:
		log.Printf("Unknown endpoint action: %v", cmd.GetAction())
		return
	}

	if err != nil {
		result = &endpoint.ProviderStatus{
			Status:  "error",
			Message: fmt.Sprintf("Endpoint operation failed: %v", err),
		}
	}

	// Send EndpointStatus back to control plane
	diagnostics := result.Diagnostics
	if diagnostics == nil {
		diagnostics = map[string]string{}
	}

	msg := &clankv1.AgentMessage{
		Payload: &clankv1.AgentMessage_EndpointStatus{
			EndpointStatus: &clankv1.EndpointStatus{
				CommandId:   cmd.GetCommandId(),
				EndpointId:  cmd.GetEndpointId(),
				Status:      result.Status,
				Message:     result.Message,
				ResolvedUrl: result.ResolvedURL,
				VerifiedBy:  result.VerifiedBy,
				Diagnostics: diagnostics,
			},
		},
	}
	if err := stream.Send(msg); err != nil {
		log.Printf("Failed to send endpoint status: %v", err)
	}
}

// HandleTunnelConfig saves tunnel credentials and starts cloudflared.
func (h *CommandHandler) HandleTunnelConfig(ctx context.Context, cfg *clankv1.TunnelConfig) {
	token := cfg.GetTunnelToken()
	tunnelID := cfg.GetTunnelId()
	log.Printf("Configuring tunnel %s", tunnelID)

	// Persist to agent config so cloudflared auto-starts on restart
	h.cfg.TunnelToken = token
	h.cfg.TunnelID = tunnelID
	if err := SaveConfig(h.cfgDir, h.cfg); err != nil {
		log.Printf("Warning: failed to save tunnel config: %v", err)
	}

	// Start or restart cloudflared
	if err := h.docker.EnsureCloudflared(ctx, token); err != nil {
		log.Printf("Error starting cloudflared: %v", err)
	}
}

// HandleBackup processes a BackupCommand — executes backup and reports result.
func (h *CommandHandler) HandleBackup(ctx context.Context, stream grpcclient.ConnectStream, cmd *clankv1.BackupCommand) {
	log.Printf("Handling backup command %s for service %s (type: %s)",
		cmd.GetCommandId(), cmd.GetServiceSlug(), cmd.GetBackupType())

	executor := backup.NewExecutor(h.docker)
	result := executor.Execute(ctx, cmd)

	if result.Success {
		log.Printf("Backup %s completed: %d bytes, %d files",
			cmd.GetBackupId(), result.SizeBytes, len(result.Files))
	} else {
		log.Printf("Backup %s failed: %s", cmd.GetBackupId(), result.ErrorMessage)
	}

	msg := &clankv1.AgentMessage{
		Payload: &clankv1.AgentMessage_BackupResult{
			BackupResult: result,
		},
	}
	if err := stream.Send(msg); err != nil {
		log.Printf("Failed to send backup result: %v (queuing)", err)
		h.queuePendingResult(msg)
	}
}
