package build

import (
	"context"
	"fmt"
	"log"

	"github.com/anaremore/clank/apps/agent/internal/docker"
)

// Builder orchestrates the clone → auto-Dockerfile → Docker build pipeline.
type Builder struct {
	docker *docker.Manager
}

// NewBuilder creates a Builder with the given Docker manager.
func NewBuilder(dm *docker.Manager) *Builder {
	return &Builder{docker: dm}
}

// BuildResult contains the output of a successful build.
type BuildResult struct {
	ImageTag      string
	GitSHA        string
	EffectivePort int    // 0 = no override (use command port)
	HealthPath    string // "" = no override (use command health path)
}

// ProgressFunc is a callback for reporting build progress.
type ProgressFunc func(status, message string)

// BuildFromSource clones a repo, auto-generates a Dockerfile if needed,
// builds the Docker image, and returns the result.
// onLog streams individual build log lines (may be nil).
func (b *Builder) BuildFromSource(
	ctx context.Context,
	repoURL, branch, gitToken, dockerfilePath, serviceSlug, deploymentID string,
	port int,
	onProgress ProgressFunc,
	onLog func(string),
) (*BuildResult, error) {
	// Step 1: Clone
	cloneMsg := fmt.Sprintf("Cloning %s (branch: %s)...", repoURL, branch)
	onProgress("cloning", cloneMsg)
	if onLog != nil {
		onLog(cloneMsg)
	}
	cloneDir, gitSHA, err := CloneRepo(ctx, repoURL, branch, gitToken)
	if err != nil {
		if onLog != nil {
			onLog(fmt.Sprintf("Clone failed: %v", err))
		}
		return nil, fmt.Errorf("clone failed: %w", err)
	}
	defer CleanupCloneDir(cloneDir)

	buildMsg := fmt.Sprintf("Cloned at %s, starting build...", gitSHA[:8])
	onProgress("building", buildMsg)
	if onLog != nil {
		onLog(buildMsg)
	}

	// Step 2: Auto-generate Dockerfile if missing
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	result := GenerateDockerfileIfMissing(cloneDir, dockerfilePath, port)
	if result.Generated {
		genMsg := fmt.Sprintf("Auto-generated Dockerfile (port=%d, health=%s)", result.EffectivePort, result.HealthPath)
		log.Print(genMsg)
		if onLog != nil {
			onLog(genMsg)
		}
	}

	// Step 3: Build image
	imageTag := fmt.Sprintf("clank-%s:%s", serviceSlug, deploymentID[:12])
	if onLog != nil {
		onLog(fmt.Sprintf("Building image %s...", imageTag))
	}
	err = b.docker.BuildImage(ctx, cloneDir, imageTag, dockerfilePath, func(msg string) {
		log.Printf("  [build] %s", msg)
		if onLog != nil {
			onLog(msg)
		}
	})
	if err != nil {
		if onLog != nil {
			onLog(fmt.Sprintf("Build failed: %v", err))
		}
		return nil, fmt.Errorf("docker build failed: %w", err)
	}

	if onLog != nil {
		onLog("Build complete")
	}

	return &BuildResult{
		ImageTag:      imageTag,
		GitSHA:        gitSHA,
		EffectivePort: result.EffectivePort,
		HealthPath:    result.HealthPath,
	}, nil
}
