package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// Config holds the agent's persistent configuration.
type Config struct {
	ServerID               string `yaml:"server_id"`
	GRPCEndpoint           string `yaml:"grpc_endpoint"`
	CertDir                string `yaml:"cert_dir"`
	AuthMode               string `yaml:"auth_mode,omitempty"`  // "mtls" (default) or "token"
	AuthToken              string `yaml:"auth_token,omitempty"` // JWT for tunnel mode
	RenewalToken           string `yaml:"renewal_token,omitempty"`
	RenewalEndpoint        string `yaml:"renewal_endpoint,omitempty"`
	CredentialExpiresUnix  int64  `yaml:"credential_expires_unix,omitempty"`
	AuthGeneration         int64  `yaml:"auth_generation,omitempty"`
	RenewalStatus          string `yaml:"renewal_status,omitempty"`
	RenewalAttempts        int    `yaml:"renewal_attempts,omitempty"`
	RenewalLastError       string `yaml:"renewal_last_error,omitempty"`
	RenewalLastSuccessUnix int64  `yaml:"renewal_last_success_unix,omitempty"`
	RenewalNextRetryUnix   int64  `yaml:"renewal_next_retry_unix,omitempty"`
	PendingRotationID      string `yaml:"pending_rotation_id,omitempty"`
	PendingAuthToken       string `yaml:"pending_auth_token,omitempty"`
	PendingRenewalToken    string `yaml:"pending_renewal_token,omitempty"`
	PendingExpiresUnix     int64  `yaml:"pending_expires_unix,omitempty"`
	PendingAuthGeneration  int64  `yaml:"pending_auth_generation,omitempty"`
	PendingPreviousBundle  string `yaml:"pending_previous_bundle,omitempty"`
	PendingHadPointer      bool   `yaml:"pending_had_pointer,omitempty"`
	TunnelToken            string `yaml:"tunnel_token,omitempty"`
	TunnelID               string `yaml:"tunnel_id,omitempty"`

	// Registry credentials for pulling Clank-hosted images (ADR-006).
	RegistryURL      string `yaml:"registry_url,omitempty"`
	RegistryUsername string `yaml:"registry_username,omitempty"`
	RegistryPassword string `yaml:"registry_password,omitempty"`

	// Resource limits (optional, defaults applied in code).
	MaxConcurrentBuilds int `yaml:"max_concurrent_builds,omitempty"`
}

// DefaultConfigDir returns the platform-appropriate config directory.
// On Linux, checks these locations in order:
//  1. /etc/clank-agent/config.yaml exists → use /etc/clank-agent
//  2. ~/.clank-agent/config.yaml exists → use ~/.clank-agent (backward compat)
//  3. /etc/clank-agent/ dir exists (no config yet) → use it for enrollment
//  4. Running as root → use /etc/clank-agent
//  5. Fallback → ~/.clank-agent
//
// Step 3 ensures new enrollments write to /etc/clank-agent (which is in the
// systemd ReadWritePaths sandbox) rather than ~/.clank-agent (which is
// read-only under ProtectHome=read-only). Step 2 preserves backward
// compatibility with servers that already have config in the home dir.
func DefaultConfigDir() string {
	if runtime.GOOS == "linux" {
		sysDir := "/etc/clank-agent"
		// If config exists in /etc, use it (preferred location)
		if _, err := os.Stat(filepath.Join(sysDir, "config.yaml")); err == nil {
			return sysDir
		}
		// If config exists in home dir, use it (backward compat for existing installs)
		home, homeErr := os.UserHomeDir()
		if homeErr == nil {
			homeDir := filepath.Join(home, ".clank-agent")
			if _, err := os.Stat(filepath.Join(homeDir, "config.yaml")); err == nil {
				return homeDir
			}
		}
		// No config anywhere yet — if /etc/clank-agent dir exists (created by
		// install script), prefer it for enrollment even as non-root user.
		if info, err := os.Stat(sysDir); err == nil && info.IsDir() {
			return sysDir
		}
		// Root always uses /etc even if dir doesn't exist yet
		if os.Getuid() == 0 {
			return sysDir
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "clank-agent")
	}
	return filepath.Join(home, ".clank-agent")
}

// LoadConfig reads the agent config from the given directory.
func LoadConfig(dir string) (*Config, error) {
	path := filepath.Join(dir, "config.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if cfg.ServerID == "" || cfg.GRPCEndpoint == "" {
		return nil, fmt.Errorf("config missing server_id or grpc_endpoint")
	}
	return &cfg, nil
}

// SaveConfig writes the complete config through a same-directory temporary
// file, preventing a crash or full disk from leaving truncated credentials.
func SaveConfig(dir string, cfg *Config) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.Rename(tmpPath, path); err != nil {
		// Windows cannot replace an existing file. Production agents run on
		// Unix; this fallback keeps local CLI use and tests functional.
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			return err
		}
		if retryErr := os.Rename(tmpPath, path); retryErr != nil {
			return retryErr
		}
	}
	ok = true
	return nil
}
