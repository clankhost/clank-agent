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
}

// DefaultConfigDir returns the platform-appropriate config directory.
// On Linux, prefers /etc/clank-agent if:
//  1. A config already exists there (running agent), OR
//  2. The directory exists (created by install script for enrollment), OR
//  3. Running as root (enrollment / initial setup)
//
// This ensures enrollment writes config to /etc/clank-agent (which is in the
// systemd ReadWritePaths sandbox) rather than ~/.clank-agent (which is
// read-only under ProtectHome=read-only).
func DefaultConfigDir() string {
	if runtime.GOOS == "linux" {
		sysDir := "/etc/clank-agent"
		// If config exists here, use it regardless of UID (systemd runs as clank user)
		if _, err := os.Stat(filepath.Join(sysDir, "config.yaml")); err == nil {
			return sysDir
		}
		// If the directory exists (created by install script), prefer it for
		// enrollment even when running as non-root user.
		if info, err := os.Stat(sysDir); err == nil && info.IsDir() {
			return sysDir
		}
		// Root always uses /etc even if config doesn't exist yet (enrollment)
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
