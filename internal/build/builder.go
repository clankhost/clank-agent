package build

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/clankhost/clank-agent/internal/docker"
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

// BuildOpts contains options for BuildFromSource.
type BuildOpts struct {
	RepoURL             string
	Branch              string
	GitToken            string
	DockerfilePath      string
	ServiceSlug         string
	DeploymentID        string
	Port                int
	BuildTimeoutSeconds int
	GeneratedDockerfile string // Platform-generated Dockerfile content (empty = use repo's own)
	OnProgress          ProgressFunc
	OnLog               func(string)
}

// BuildFromSource clones a repo, writes the platform-generated Dockerfile
// (or uses the repo's own), builds the Docker image, and returns the result.
func (b *Builder) BuildFromSource(ctx context.Context, opts BuildOpts) (*BuildResult, error) {
	onLog := opts.OnLog
	logLine := func(msg string) {
		if onLog != nil {
			onLog(msg)
		}
	}

	// Apply build timeout
	timeout := time.Duration(opts.BuildTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 600 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Step 1: Clone
	cloneMsg := fmt.Sprintf("Cloning %s (branch: %s)...", opts.RepoURL, opts.Branch)
	opts.OnProgress("cloning", cloneMsg)
	logLine(cloneMsg)

	cloneDir, gitSHA, err := CloneRepo(ctx, opts.RepoURL, opts.Branch, opts.GitToken)
	if err != nil {
		logLine(fmt.Sprintf("Clone failed: %v", err))
		return nil, fmt.Errorf("clone failed: %w", err)
	}
	defer CleanupCloneDir(cloneDir)

	buildMsg := fmt.Sprintf("Cloned at %s, starting build...", gitSHA[:8])
	opts.OnProgress("building", buildMsg)
	logLine(buildMsg)

	// Step 2: Write Dockerfile
	dockerfilePath := opts.DockerfilePath
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	fullPath := filepath.Join(cloneDir, dockerfilePath)

	if _, err := os.Stat(fullPath); err == nil {
		// Repo has its own Dockerfile — use it (always takes priority)
		logLine("Using Dockerfile from repository")
	} else if opts.GeneratedDockerfile != "" {
		// Platform sent a generated Dockerfile via proto
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return nil, fmt.Errorf("creating Dockerfile directory: %w", err)
		}
		if err := os.WriteFile(fullPath, []byte(opts.GeneratedDockerfile), 0o644); err != nil {
			return nil, fmt.Errorf("writing generated Dockerfile: %w", err)
		}
		logLine("Using platform-generated Dockerfile")
	} else {
		logLine("No Dockerfile found. Add a Dockerfile to your repo or re-inspect your service.")
		return nil, fmt.Errorf("no Dockerfile found in repository and no platform-generated Dockerfile provided")
	}

	// Step 3: Build image
	imageTag := fmt.Sprintf("clank-%s:%s", opts.ServiceSlug, opts.DeploymentID[:12])
	logLine(fmt.Sprintf("Building image %s...", imageTag))

	err = b.docker.BuildImage(ctx, cloneDir, imageTag, dockerfilePath, func(msg string) {
		log.Printf("  [build] %s", msg)
		logLine(msg)
	})
	if err != nil {
		logLine(fmt.Sprintf("Build failed: %v", err))
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("build timed out after %v", timeout)
		}
		return nil, fmt.Errorf("docker build failed: %w", err)
	}

	logLine("Build complete")

	return &BuildResult{
		ImageTag: imageTag,
		GitSHA:   gitSHA,
	}, nil
}
