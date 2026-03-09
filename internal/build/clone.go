package build

import (
	"context"
	"fmt"
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

	// Clone with depth 1 for speed
	args := []string{"clone", "--depth", "1"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, repoURL, cloneDir)

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	// Use GIT_ASKPASS to supply credentials without exposing the token
	// in the process argument list (/proc/<pid>/cmdline).
	if gitToken != "" {
		askpassScript, cleanupAskpass, askErr := writeAskpassHelper(gitToken)
		if askErr != nil {
			os.RemoveAll(cloneDir)
			return "", "", fmt.Errorf("creating askpass helper: %w", askErr)
		}
		defer cleanupAskpass()

		// Provider-specific git username
		gitUsername := "x-access-token" // GitHub default
		if strings.Contains(repoURL, "gitlab.com") {
			gitUsername = "oauth2"
		} else if strings.Contains(repoURL, "bitbucket.org") {
			gitUsername = "x-token-auth"
		}

		cmd.Env = append(cmd.Env,
			"GIT_ASKPASS="+askpassScript,
			"GIT_USERNAME="+gitUsername,
		)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(cloneDir)
		// Scrub any token that might appear in error output
		sanitized := scrubToken(string(output), gitToken)
		return "", "", fmt.Errorf("git clone failed: %s\n%s", err, sanitized)
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

// writeAskpassHelper creates a temporary shell script that outputs the git
// token when invoked by git. The script is only readable by the current user.
// Returns the script path and a cleanup function.
func writeAskpassHelper(token string) (scriptPath string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "clank-askpass-*.sh")
	if err != nil {
		return "", nil, err
	}
	scriptPath = f.Name()

	// The script checks if git is asking for a password or username.
	// GIT_ASKPASS is called with a prompt like "Password for ..." or "Username for ...".
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  *assword*) echo '%s' ;;
  *sername*) echo "${GIT_USERNAME:-x-access-token}" ;;
esac
`, token)

	if _, err := f.WriteString(script); err != nil {
		f.Close()
		os.Remove(scriptPath)
		return "", nil, err
	}
	f.Close()

	// Make executable, owner-only permissions (0700)
	if err := os.Chmod(scriptPath, 0700); err != nil {
		os.Remove(scriptPath)
		return "", nil, err
	}

	cleanup = func() { os.Remove(scriptPath) }
	return scriptPath, cleanup, nil
}

// scrubToken replaces occurrences of the token in output to prevent leaking
// credentials in error messages or logs.
func scrubToken(output, token string) string {
	if token == "" {
		return output
	}
	return strings.ReplaceAll(output, token, "***")
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
