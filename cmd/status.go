package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/clankhost/clank-agent/internal/agent"
	"github.com/clankhost/clank-agent/internal/certs"
	"github.com/clankhost/clank-agent/internal/credentials"
	"github.com/clankhost/clank-agent/internal/docker"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show agent status and runtime information",
	RunE:  runStatus,
}

var statusJSON bool

// statusInfo holds the collected status for display or JSON output.
type statusInfo struct {
	Version          string `json:"version"`
	ServerID         string `json:"server_id"`
	Endpoint         string `json:"grpc_endpoint"`
	ConfigDir        string `json:"config_dir"`
	CertExpiry       string `json:"cert_expiry,omitempty"`
	AuthMode         string `json:"auth_mode,omitempty"`
	RenewalStatus    string `json:"renewal_status,omitempty"`
	RenewalError     string `json:"renewal_error,omitempty"`
	RenewalNextRetry string `json:"renewal_next_retry,omitempty"`
	SystemdState     string `json:"systemd_state,omitempty"`
	ContainerCount   int    `json:"managed_containers"`
}

func runStatus(cmd *cobra.Command, args []string) error {
	configDir := agent.DefaultConfigDir()
	if cfgFile != "" {
		configDir = cfgFile
	}

	info := statusInfo{
		Version:   Version,
		ConfigDir: configDir,
	}

	credentialUnhealthy := false
	// Load config
	cfg, err := agent.LoadConfig(configDir)
	if err != nil {
		info.ServerID = "(not enrolled)"
		info.Endpoint = "(not enrolled)"
	} else {
		info.ServerID = cfg.ServerID
		info.Endpoint = cfg.GRPCEndpoint
		info.AuthMode = cfg.AuthMode
		if info.AuthMode == "" {
			info.AuthMode = "mtls"
		}
		info.RenewalStatus = cfg.RenewalStatus
		info.RenewalError = cfg.RenewalLastError
		if cfg.RenewalNextRetryUnix > 0 {
			info.RenewalNextRetry = time.Unix(cfg.RenewalNextRetryUnix, 0).Format(time.RFC3339)
		}
		certDir := cfg.CertDir
		if certDir == "" {
			certDir = configDir
		}
		expiresAt, expiryErr := credentials.Expiry(cfg.AuthMode, cfg.AuthToken, certs.NewStore(certDir), cfg.CredentialExpiresUnix)
		if expiryErr != nil {
			info.CertExpiry = "unavailable"
			credentialUnhealthy = true
		} else {
			info.CertExpiry = formatCredentialExpiry(expiresAt)
			credentialUnhealthy = time.Until(expiresAt) <= 0 || (time.Until(expiresAt) <= credentials.RenewalWindow && cfg.RenewalStatus == "failed")
		}
	}

	// Systemd state (Linux only)
	info.SystemdState = getSystemdState()

	// Managed container count
	info.ContainerCount = countManagedContainers()

	if statusJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(info); err != nil {
			return err
		}
		if credentialUnhealthy {
			return fmt.Errorf("control credential requires attention")
		}
		return nil
	}

	// Pretty print
	fmt.Printf("clank-agent %s\n\n", info.Version)
	fmt.Printf("  Server ID:       %s\n", info.ServerID)
	fmt.Printf("  gRPC Endpoint:   %s\n", info.Endpoint)
	fmt.Printf("  Config Dir:      %s\n", info.ConfigDir)
	fmt.Printf("  Auth Mode:       %s\n", info.AuthMode)
	fmt.Printf("  Credential:      %s\n", info.CertExpiry)
	fmt.Printf("  Renewal:         %s\n", info.RenewalStatus)
	if info.RenewalError != "" {
		fmt.Printf("  Renewal Error:   %s\n", info.RenewalError)
	}
	if info.RenewalNextRetry != "" {
		fmt.Printf("  Next Retry:      %s\n", info.RenewalNextRetry)
	}
	if runtime.GOOS == "linux" {
		fmt.Printf("  Systemd:         %s\n", info.SystemdState)
	}
	fmt.Printf("  Containers:      %d managed\n", info.ContainerCount)
	if credentialUnhealthy {
		return fmt.Errorf("control credential requires attention")
	}
	return nil
}

func formatCredentialExpiry(expiresAt time.Time) string {
	remaining := time.Until(expiresAt)
	if remaining <= 0 {
		return fmt.Sprintf("EXPIRED (%s)", expiresAt.Format("2006-01-02"))
	}
	return fmt.Sprintf("%s (%s remaining)", expiresAt.Format("2006-01-02"), remaining.Round(24*time.Hour))
}

func getSystemdState() string {
	if runtime.GOOS != "linux" {
		return "n/a"
	}
	out, err := exec.Command("systemctl", "is-active", "clank-agent").Output()
	if err != nil {
		state := strings.TrimSpace(string(out))
		if state != "" {
			return state
		}
		return "not installed"
	}
	return strings.TrimSpace(string(out))
}

func countManagedContainers() int {
	mgr, err := docker.NewManager()
	if err != nil {
		return 0
	}
	containers, err := mgr.ListManagedContainers(context.Background())
	if err != nil {
		return 0
	}
	return len(containers)
}

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "output as JSON")
	rootCmd.AddCommand(statusCmd)
}
