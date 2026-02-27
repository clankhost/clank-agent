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
	ImageTag string
	GitSHA   string
}

// ProgressFunc is a callback for reporting build progress.
type ProgressFunc func(status, message string)

// BuildFromSource clones a repo, auto-generates a Dockerfile if needed,
// builds the Docker image, and returns the result.
func (b *Builder) BuildFromSource(
	ctx context.Context,
	repoURL, branch, gitToken, dockerfilePath, serviceSlug, deploymentID string,
	port int,
	onProgress ProgressFunc,
) (*BuildResult, error) {
	// Step 1: Clone
	onProgress("cloning", fmt.Sprintf("Cloning %s (branch: %s)...", repoURL, branch))
	cloneDir, gitSHA, err := CloneRepo(ctx, repoURL, branch, gitToken)
	if err != nil {
		return nil, fmt.Errorf("clone failed: %w", err)
	}
	defer CleanupCloneDir(cloneDir)

	onProgress("building", fmt.Sprintf("Cloned at %s, starting build...", gitSHA[:8]))

	// Step 2: Auto-generate Dockerfile if missing
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	result := GenerateDockerfileIfMissing(cloneDir, dockerfilePath, port)
	if result.Generated {
		log.Printf("Auto-generated Dockerfile (port=%d, health=%s)", result.EffectivePort, result.HealthPath)
	}

	// Step 3: Build image
	imageTag := fmt.Sprintf("clank-%s:%s", serviceSlug, deploymentID[:12])
	err = b.docker.BuildImage(ctx, cloneDir, imageTag, dockerfilePath, func(msg string) {
		log.Printf("  [build] %s", msg)
	})
	if err != nil {
		return nil, fmt.Errorf("docker build failed: %w", err)
	}

	return &BuildResult{
		ImageTag: imageTag,
		GitSHA:   gitSHA,
	}, nil
}
