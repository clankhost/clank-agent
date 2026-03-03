package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	clankv1 "github.com/anaremore/clank/apps/agent/gen/clank/v1"
	"github.com/anaremore/clank/apps/agent/internal/build"
	"github.com/anaremore/clank/apps/agent/internal/deploy"
	"github.com/anaremore/clank/apps/agent/internal/docker"
	"github.com/anaremore/clank/apps/agent/internal/endpoint"
	"github.com/anaremore/clank/apps/agent/internal/grpcclient"
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

	// pendingResults holds deploy progress messages whose Send failed
	// (stream broke mid-deploy). Drained on the next successful connection.
	pendingMu      sync.Mutex
	pendingResults []*clankv1.AgentMessage
}

// NewCommandHandler creates a handler with all agent capabilities.
func NewCommandHandler(dm *docker.Manager, b *build.Builder, d *deploy.Deployer, cfg *Config, cfgDir string, version string) *CommandHandler {
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

// HandleDeploy processes a DeployCommand — clone+build or image pull, then deploy.
func (h *CommandHandler) HandleDeploy(ctx context.Context, stream grpcclient.ConnectStream, cmd *clankv1.DeployCommand) {
	deployID := cmd.GetDeploymentId()
	log.Printf("Handling deploy command for deployment %s (service: %s)", deployID, cmd.GetServiceSlug())

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
				},
			},
		}
		if err := stream.Send(msg); err != nil {
			log.Printf("Failed to send deploy progress: %v", err)
			// Queue terminal statuses for retry on next connection.
			// These are the final results the API needs to mark the
			// deployment as active or failed.
			if terminalDeployStatuses[status] {
				h.queuePendingResult(msg)
			}
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
		)
		if err != nil {
			sendProgress("build_failed", fmt.Sprintf("Build failed: %v", err), "", "", "", "")
			return
		}

		imageTag = result.ImageTag
		gitSHA = result.GitSHA
		sendProgress("built", "Build complete", "", "", imageTag, gitSHA)
	}

	if imageTag == "" {
		sendProgress("failed", "No image to deploy (no repo_url and no image_tag)", "", "", "", "")
		return
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

	err := h.deployer.Deploy(ctx, deploy.DeployOpts{
		DeploymentID:    deployID,
		ServiceSlug:     cmd.GetServiceSlug(),
		ImageTag:        imageTag,
		Env:             cmd.GetEnvVars(),
		Port:            int(cmd.GetPort()),
		Domains:         cmd.GetDomains(),
		Endpoints:       endpoints,
		HealthCheckPath: cmd.GetHealthCheckPath(),
		HealthConfig:    healthConfig,
		CPULimit:        cpuLimit,
		MemoryLimitMB:   memoryLimitMB,
		ProjectNetwork:  cmd.GetProjectNetwork(),
		LANIPs:          hostIPs,
	}, func(status, message, containerID, containerName string) {
		sendProgress(status, message, containerID, containerName, imageTag, gitSHA)
	})

	if err != nil {
		sendProgress("failed", fmt.Sprintf("Deploy failed: %v", err), "", "", imageTag, gitSHA)
	}
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

	// Write state file before attempting update (for crash recovery)
	selfupdate.SaveState(h.cfgDir, &selfupdate.UpdateState{
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
			h.cfgDir,
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
		selfupdate.ClearState(h.cfgDir)
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
			if err := h.docker.ReconfigureTraefikACME(ctx); err != nil {
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
