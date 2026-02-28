package cmd

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/anaremore/clank/apps/agent/internal/agent"
	"github.com/anaremore/clank/apps/agent/internal/docker"
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
	Version        string `json:"version"`
	ServerID       string `json:"server_id"`
	Endpoint       string `json:"grpc_endpoint"`
	ConfigDir      string `json:"config_dir"`
	CertExpiry     string `json:"cert_expiry,omitempty"`
	SystemdState   string `json:"systemd_state,omitempty"`
	ContainerCount int    `json:"managed_containers"`
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

	// Load config
	cfg, err := agent.LoadConfig(configDir)
	if err != nil {
		info.ServerID = "(not enrolled)"
		info.Endpoint = "(not enrolled)"
	} else {
		info.ServerID = cfg.ServerID
		info.Endpoint = cfg.GRPCEndpoint
	}

	// Cert expiry
	info.CertExpiry = getCertExpiry(configDir)

	// Systemd state (Linux only)
	info.SystemdState = getSystemdState()

	// Managed container count
	info.ContainerCount = countManagedContainers()

	if statusJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}

	// Pretty print
	fmt.Printf("clank-agent %s\n\n", info.Version)
	fmt.Printf("  Server ID:       %s\n", info.ServerID)
	fmt.Printf("  gRPC Endpoint:   %s\n", info.Endpoint)
	fmt.Printf("  Config Dir:      %s\n", info.ConfigDir)
	fmt.Printf("  Cert Expiry:     %s\n", info.CertExpiry)
	if runtime.GOOS == "linux" {
		fmt.Printf("  Systemd:         %s\n", info.SystemdState)
	}
	fmt.Printf("  Containers:      %d managed\n", info.ContainerCount)
	return nil
}

func getCertExpiry(configDir string) string {
	certPath := filepath.Join(configDir, "client.crt")
	data, err := os.ReadFile(certPath)
	if err != nil {
		return "no certificate"
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "invalid certificate"
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "invalid certificate"
	}
	remaining := time.Until(cert.NotAfter)
	if remaining <= 0 {
		return fmt.Sprintf("EXPIRED (%s)", cert.NotAfter.Format("2006-01-02"))
	}
	return fmt.Sprintf("%s (%s remaining)", cert.NotAfter.Format("2006-01-02"), remaining.Round(24*time.Hour))
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
