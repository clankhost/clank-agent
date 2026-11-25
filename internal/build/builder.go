package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/clankhost/clank-agent/internal/docker"
)

type buildBackend int

const (
	buildBackendLegacy buildBackend = iota
	buildBackendBuildKitCLI
	buildBackendBuildx
)

func (b buildBackend) String() string {
	switch b {
	case buildBackendBuildx:
		return "docker buildx (BuildKit)"
	case buildBackendBuildKitCLI:
		return "docker build (DOCKER_BUILDKIT=1)"
	default:
		return "legacy Docker API ImageBuild"
	}
}

type buildRequest struct {
	ContextDir     string
	DockerfilePath string // relative slash path for legacy API
	FullDockerfile string // absolute path inside ContextDir for CLI
	ImageTag       string
	CacheFrom      []string
	Labels         map[string]string
	BuildArgs      map[string]string
	Target         string
	NoCache        bool
}

// Builder orchestrates the clone → auto-Dockerfile → Docker build pipeline.
type Builder struct {
	docker *docker.Manager

	// Optional hooks for testing; nil uses production defaults.
	detectBuildKitFn func(ctx context.Context) (buildBackend, string)
	runBuildKitFn    func(ctx context.Context, backend buildBackend, req buildRequest, onLog func(string)) error
	legacyBuildFn    func(ctx context.Context, req buildRequest, onLog func(string)) error
	latestImageFn    func(ctx context.Context, slug string) string
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
	GeneratedDockerfile string            // Platform-generated Dockerfile content (empty = use repo's own)
	BuildArgs           map[string]string // Optional Docker build args
	Target              string            // Optional multi-stage build target
	NoCache             bool              // Disable build cache when true
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

	buildMsg := fmt.Sprintf("Cloned at %s, starting build...", shortID(gitSHA, 8))
	opts.OnProgress("building", buildMsg)
	logLine(buildMsg)

	// Step 2: Write Dockerfile
	dockerfilePath := opts.DockerfilePath
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	fullPath, cleanRelDockerfile, err := resolveSafeDockerfilePath(cloneDir, dockerfilePath)
	if err != nil {
		return nil, err
	}
	dockerfilePath = cleanRelDockerfile

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

	// Step 3: Build image (with BuildKit persistent layer cache, falling back to legacy ImageBuild)
	imageTag := fmt.Sprintf("clank-%s:%s", opts.ServiceSlug, shortID(opts.DeploymentID, 12))

	var cacheFrom []string
	if !opts.NoCache {
		if prev := b.latestImageForSlug(ctx, opts.ServiceSlug); prev != "" {
			cacheFrom = []string{prev}
			logLine(fmt.Sprintf("Using layer cache from %s", prev))
		}
	}

	logLine(fmt.Sprintf("Building image %s...", imageTag))

	buildLog := func(msg string) {
		log.Printf("  [build] %s", msg)
		logLine(msg)
	}
	req := buildRequest{
		ContextDir:     cloneDir,
		DockerfilePath: dockerfilePath,
		FullDockerfile: fullPath,
		ImageTag:       imageTag,
		CacheFrom:      cacheFrom,
		Labels:         imageOwnershipLabels(opts.DeploymentID, opts.ServiceSlug),
		BuildArgs:      opts.BuildArgs,
		Target:         opts.Target,
		NoCache:        opts.NoCache,
	}

	err = b.executeBuild(ctx, req, buildLog)
	if err != nil {
		logLine(fmt.Sprintf("Build failed: %v", err))
		if isTimeout(ctx) {
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

func (b *Builder) latestImageForSlug(ctx context.Context, slug string) string {
	if b.latestImageFn != nil {
		return b.latestImageFn(ctx, slug)
	}
	if b.docker == nil {
		return ""
	}
	return b.docker.LatestImageForSlug(ctx, slug)
}

func (b *Builder) executeBuild(ctx context.Context, req buildRequest, onLog func(string)) error {
	backend, reason := b.detectBuildKit(ctx)
	if backend != buildBackendLegacy {
		onLog(fmt.Sprintf("Using %s with persistent layer cache", backend))
		err := b.runBuildKit(ctx, backend, req, onLog)
		if err == nil {
			return nil
		}

		// If BuildKit itself isn't supported by the daemon/CLI, fall back cleanly to legacy ImageBuild.
		if isBuildKitUnavailableError(err) && !isTimeout(ctx) && ctx.Err() == nil {
			onLog(fmt.Sprintf("BuildKit unavailable (%v), falling back to legacy Docker builder...", err))
			return b.runLegacyBuildWithCacheRetry(ctx, req, onLog)
		}

		// Cache-related build failures (e.g. corrupt/missing cache source) should retry without cache-from.
		if len(req.CacheFrom) > 0 && !req.NoCache && !isTimeout(ctx) && ctx.Err() == nil {
			onLog(fmt.Sprintf("Build with cache-from failed: %v", err))
			onLog("Retrying BuildKit build without --cache-from...")
			noCacheFromReq := req
			noCacheFromReq.CacheFrom = nil
			return b.runBuildKit(ctx, backend, noCacheFromReq, onLog)
		}
		return err
	}

	if reason != "" {
		onLog(fmt.Sprintf("BuildKit unavailable (%s), using legacy Docker builder", reason))
	}
	return b.runLegacyBuildWithCacheRetry(ctx, req, onLog)
}

func (b *Builder) runLegacyBuildWithCacheRetry(ctx context.Context, req buildRequest, onLog func(string)) error {
	err := b.runLegacyBuild(ctx, req, onLog)
	if err != nil && len(req.CacheFrom) > 0 && !req.NoCache && !isTimeout(ctx) && ctx.Err() == nil {
		// Cache-related build failures (moby-dangling corruption, missing layers)
		// should not be fatal — retry without cache.
		onLog(fmt.Sprintf("Build with cache failed: %v", err))
		onLog("Retrying build without cache...")
		retryReq := req
		retryReq.CacheFrom = nil
		err = b.runLegacyBuild(ctx, retryReq, onLog)
	}
	return err
}

func (b *Builder) detectBuildKit(ctx context.Context) (buildBackend, string) {
	if b.detectBuildKitFn != nil {
		return b.detectBuildKitFn(ctx)
	}
	return defaultDetectBuildKit(ctx)
}

func defaultDetectBuildKit(ctx context.Context) (buildBackend, string) {
	if _, err := exec.LookPath("docker"); err != nil {
		return buildBackendLegacy, "docker CLI not found in PATH"
	}

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Prefer `docker buildx build --load` (standard BuildKit frontend with persistent cache).
	if err := exec.CommandContext(probeCtx, "docker", "buildx", "version").Run(); err == nil {
		return buildBackendBuildx, ""
	}

	// Fallback to `DOCKER_BUILDKIT=1 docker build` if docker CLI is functional.
	if err := exec.CommandContext(probeCtx, "docker", "version", "--format", "{{.Server.Version}}").Run(); err == nil {
		return buildBackendBuildKitCLI, ""
	}

	return buildBackendLegacy, "docker CLI probe failed"
}

func (b *Builder) runBuildKit(ctx context.Context, backend buildBackend, req buildRequest, onLog func(string)) error {
	if b.runBuildKitFn != nil {
		return b.runBuildKitFn(ctx, backend, req, onLog)
	}
	return defaultRunBuildKit(ctx, backend, req, onLog)
}

func buildKitCommandArgs(backend buildBackend, req buildRequest) []string {
	var args []string
	if backend == buildBackendBuildx {
		args = append(args, "buildx", "build", "--load")
	} else {
		args = append(args, "build")
	}

	args = append(args, "--progress=plain", "-t", req.ImageTag)

	dockerfile := req.FullDockerfile
	if dockerfile == "" {
		dockerfile = req.DockerfilePath
	}
	if dockerfile != "" {
		args = append(args, "-f", dockerfile)
	}

	if req.Target != "" {
		args = append(args, "--target", req.Target)
	}

	if req.NoCache {
		args = append(args, "--no-cache")
	} else {
		for _, ref := range req.CacheFrom {
			if ref != "" {
				args = append(args, "--cache-from", ref)
			}
		}
	}

	if len(req.BuildArgs) > 0 {
		keys := make([]string, 0, len(req.BuildArgs))
		for k := range req.BuildArgs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--build-arg", fmt.Sprintf("%s=%s", k, req.BuildArgs[k]))
		}
	}

	if len(req.Labels) > 0 {
		keys := make([]string, 0, len(req.Labels))
		for k := range req.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--label", fmt.Sprintf("%s=%s", k, req.Labels[k]))
		}
	}

	args = append(args, req.ContextDir)
	return args
}

func defaultRunBuildKit(ctx context.Context, backend buildBackend, req buildRequest, onLog func(string)) error {
	args := buildKitCommandArgs(backend, req)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")

	logWriter := newLineLogWriter(onLog)
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter

	err := cmd.Run()
	logWriter.Flush()
	if err != nil {
		if tail := logWriter.LastLine(); tail != "" {
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	onLog(fmt.Sprintf("Image %s built", req.ImageTag))
	return nil
}

func (b *Builder) runLegacyBuild(ctx context.Context, req buildRequest, onLog func(string)) error {
	if b.legacyBuildFn != nil {
		return b.legacyBuildFn(ctx, req, onLog)
	}
	return b.docker.BuildImageWithOpts(ctx, docker.ImageBuildOpts{
		ContextPath: req.ContextDir,
		Tag:         req.ImageTag,
		Dockerfile:  req.DockerfilePath,
		CacheFrom:   req.CacheFrom,
		Labels:      req.Labels,
		BuildArgs:   req.BuildArgs,
		Target:      req.Target,
		NoCache:     req.NoCache,
	}, onLog)
}

func isBuildKitUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "buildkit is not supported") ||
		strings.Contains(msg, "buildkit not supported") ||
		strings.Contains(msg, "requires buildkit") ||
		strings.Contains(msg, "unknown flag: --progress") ||
		strings.Contains(msg, "unknown flag: --load") ||
		strings.Contains(msg, "is not a docker command")
}

// lineLogWriter streams line-delimited output from stdout/stderr to onLog
// without bufio.Scanner's 64KB token limit.
type lineLogWriter struct {
	mu       sync.Mutex
	onLog    func(string)
	buf      bytes.Buffer
	lastLine string
}

func newLineLogWriter(onLog func(string)) *lineLogWriter {
	return &lineLogWriter{onLog: onLog}
}

func (w *lineLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf.Write(p)
	for {
		data := w.buf.Bytes()
		idx := bytes.IndexAny(data, "\r\n")
		if idx < 0 {
			break
		}
		line := strings.TrimSpace(string(data[:idx]))
		w.buf.Next(idx + 1)
		if line != "" {
			w.lastLine = line
			if w.onLog != nil {
				w.onLog(line)
			}
		}
	}
	return len(p), nil
}

func (w *lineLogWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()

	line := strings.TrimSpace(w.buf.String())
	w.buf.Reset()
	if line != "" {
		w.lastLine = line
		if w.onLog != nil {
			w.onLog(line)
		}
	}
}

func (w *lineLogWriter) LastLine() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastLine
}

func imageOwnershipLabels(deploymentID, serviceSlug string) map[string]string {
	return map[string]string{
		"clank.managed":           "true",
		"clank.ownership_version": "1",
		"clank.artifact_type":     "service_image",
		"clank.deployment_id":     deploymentID,
		"clank.service_slug":      serviceSlug,
	}
}

func isTimeout(ctx context.Context) bool {
	return ctx.Err() == context.DeadlineExceeded
}

func shortID(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}

// resolveSafeDockerfilePath validates that dockerfilePath resolves strictly inside cloneDir,
// preventing path traversal (e.g. "../../etc/passwd") or absolute paths.
func resolveSafeDockerfilePath(cloneDir, dockerfilePath string) (fullPath string, relPath string, err error) {
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	if filepath.IsAbs(dockerfilePath) {
		return "", "", fmt.Errorf("invalid dockerfile path %q: must be relative to repository root", dockerfilePath)
	}
	absClone, err := filepath.Abs(cloneDir)
	if err != nil {
		return "", "", fmt.Errorf("resolving clone directory: %w", err)
	}
	absTarget, err := filepath.Abs(filepath.Join(absClone, dockerfilePath))
	if err != nil {
		return "", "", fmt.Errorf("resolving dockerfile path: %w", err)
	}
	rel, err := filepath.Rel(absClone, absTarget)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("invalid dockerfile path %q: escapes repository root", dockerfilePath)
	}
	return absTarget, filepath.ToSlash(rel), nil
}
