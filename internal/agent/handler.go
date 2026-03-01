package agent

import (
	"context"
	"fmt"
	"log"

	clankv1 "github.com/anaremore/clank/apps/agent/gen/clank/v1"
	"github.com/anaremore/clank/apps/agent/internal/build"
	"github.com/anaremore/clank/apps/agent/internal/deploy"
	"github.com/anaremore/clank/apps/agent/internal/docker"
	"github.com/anaremore/clank/apps/agent/internal/grpcclient"
)

// CommandHandler processes commands received from the control plane.
type CommandHandler struct {
	docker   *docker.Manager
	builder  *build.Builder
	deployer *deploy.Deployer
	cfg      *Config
	cfgDir   string
}

// NewCommandHandler creates a handler with all agent capabilities.
func NewCommandHandler(dm *docker.Manager, b *build.Builder, d *deploy.Deployer, cfg *Config, cfgDir string) *CommandHandler {
	return &CommandHandler{
		docker:   dm,
		builder:  b,
		deployer: d,
		cfg:      cfg,
		cfgDir:   cfgDir,
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

	err := h.deployer.Deploy(ctx, deploy.DeployOpts{
		DeploymentID:    deployID,
		ServiceSlug:     cmd.GetServiceSlug(),
		ImageTag:        imageTag,
		Env:             cmd.GetEnvVars(),
		Port:            int(cmd.GetPort()),
		Domains:         cmd.GetDomains(),
		HealthCheckPath: cmd.GetHealthCheckPath(),
		HealthConfig:    healthConfig,
		CPULimit:        cpuLimit,
		MemoryLimitMB:   memoryLimitMB,
		ProjectNetwork:  cmd.GetProjectNetwork(),
	}, func(status, message, containerID, containerName string) {
		sendProgress(status, message, containerID, containerName, imageTag, gitSHA)
	})

	if err != nil {
		sendProgress("failed", fmt.Sprintf("Deploy failed: %v", err), "", "", imageTag, gitSHA)
	}
}

// HandleContainerCommand processes a ContainerCommand (stop/start/restart).
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
