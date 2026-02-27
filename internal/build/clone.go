package build

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CloneRepo clones a git repository to a temporary directory.
// Returns the clone directory and HEAD commit SHA.
func CloneRepo(ctx context.Context, repoURL, branch, gitToken string) (cloneDir string, gitSHA string, err error) {
	// Create temp dir for the clone
	cloneDir, err = os.MkdirTemp("", "clank-build-*")
	if err != nil {
		return "", "", fmt.Errorf("creating temp dir: %w", err)
	}

	// Inject token into HTTPS URL for private repos
	cloneURL := repoURL
	if gitToken != "" && strings.Contains(repoURL, "github.com") {
		parsed, parseErr := url.Parse(repoURL)
		if parseErr == nil && parsed.Scheme == "https" {
			parsed.User = url.UserPassword("x-access-token", gitToken)
			cloneURL = parsed.String()
		}
	}

	// Clone with depth 1 for speed
	args := []string{"clone", "--depth", "1"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, cloneURL, cloneDir)

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = os.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(cloneDir)
		return "", "", fmt.Errorf("git clone failed: %s\n%s", err, string(output))
	}

	// Get HEAD SHA
	shaCmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	shaCmd.Dir = cloneDir
	shaBytes, err := shaCmd.Output()
	if err != nil {
		// Non-fatal — continue without SHA
		gitSHA = "unknown"
	} else {
		gitSHA = strings.TrimSpace(string(shaBytes))
	}

	return cloneDir, gitSHA, nil
}

// CleanupCloneDir removes a clone directory (best-effort).
func CleanupCloneDir(dir string) {
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// ResolveDockerfilePath returns the absolute Dockerfile path within the clone dir.
func ResolveDockerfilePath(cloneDir, dockerfilePath string) string {
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	return filepath.Join(cloneDir, dockerfilePath)
}
