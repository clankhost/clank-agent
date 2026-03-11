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
	ServerID     string `yaml:"server_id"`
	GRPCEndpoint string `yaml:"grpc_endpoint"`
	CertDir      string `yaml:"cert_dir"`
	AuthMode     string `yaml:"auth_mode,omitempty"`  // "mtls" (default) or "token"
	AuthToken    string `yaml:"auth_token,omitempty"` // JWT for tunnel mode
	TunnelToken  string `yaml:"tunnel_token,omitempty"`
	TunnelID     string `yaml:"tunnel_id,omitempty"`

	// Registry credentials for pulling Clank-hosted images (ADR-006).
	RegistryURL      string `yaml:"registry_url,omitempty"`
	RegistryUsername string `yaml:"registry_username,omitempty"`
	RegistryPassword string `yaml:"registry_password,omitempty"`
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

// SaveConfig writes the agent config to the given directory.
func SaveConfig(dir string, cfg *Config) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0600)
}
