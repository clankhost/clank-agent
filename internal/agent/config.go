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
}

// DefaultConfigDir returns the platform-appropriate config directory.
func DefaultConfigDir() string {
	if runtime.GOOS == "linux" {
		// Prefer /etc for system-wide install, fall back to home dir
		if os.Getuid() == 0 {
			return "/etc/clank-agent"
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".clank-agent"
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
