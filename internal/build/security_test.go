package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAskpassHelperUsesEnvVar(t *testing.T) {
	scriptPath, cleanup, err := writeAskpassHelper()
	if err != nil {
		t.Fatalf("writeAskpassHelper() error = %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("reading askpass script: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, `printf '%s\n' "$CLANK_GIT_TOKEN"`) {
		t.Fatalf("askpass script does not use printf with CLANK_GIT_TOKEN: %q", content)
	}
	if strings.Contains(content, "echo '") {
		t.Fatalf("askpass script should not use single-quoted echo interpolation: %q", content)
	}
}

func TestResolveSafeDockerfilePath(t *testing.T) {
	cloneDir := t.TempDir()

	valid := []struct {
		input   string
		wantRel string
	}{
		{"", "Dockerfile"},
		{"Dockerfile", "Dockerfile"},
		{"docker/Dockerfile.prod", "docker/Dockerfile.prod"},
		{"./sub/Dockerfile", "sub/Dockerfile"},
	}
	for _, tc := range valid {
		full, rel, err := resolveSafeDockerfilePath(cloneDir, tc.input)
		if err != nil {
			t.Errorf("resolveSafeDockerfilePath(%q) unexpected error: %v", tc.input, err)
			continue
		}
		if rel != tc.wantRel {
			t.Errorf("resolveSafeDockerfilePath(%q) rel = %q, want %q", tc.input, rel, tc.wantRel)
		}
		if !strings.HasPrefix(full, cloneDir) {
			t.Errorf("fullPath %q is not inside cloneDir %q", full, cloneDir)
		}
	}

	absEscape := filepath.Join(filepath.Dir(cloneDir), "outside")
	invalid := []string{
		"../Dockerfile",
		"../../etc/passwd",
		"sub/../../escape",
		".",
		absEscape,
	}
	for _, bad := range invalid {
		if _, _, err := resolveSafeDockerfilePath(cloneDir, bad); err == nil {
			t.Errorf("resolveSafeDockerfilePath(%q) succeeded, want traversal error", bad)
		}
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("unknown", 8); got != "unknown" {
		t.Errorf("shortID(unknown, 8) = %q, want unknown", got)
	}
	if got := shortID("1234567890abcdef", 12); got != "1234567890ab" {
		t.Errorf("shortID(..., 12) = %q", got)
	}
	if got := shortID("", 8); got != "" {
		t.Errorf("shortID('', 8) = %q", got)
	}
}
