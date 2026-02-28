package cmd

import (
	"fmt"

	"github.com/anaremore/clank/apps/agent/internal/agent"
	"github.com/anaremore/clank/apps/agent/internal/certs"
	"github.com/anaremore/clank/apps/agent/internal/grpcclient"
	"github.com/anaremore/clank/apps/agent/internal/sysinfo"
	"github.com/spf13/cobra"
)

var enrollCmd = &cobra.Command{
	Use:   "enroll",
	Short: "Enroll this server with the Clank control plane",
	RunE:  runEnroll,
}

var (
	enrollToken         string
	enrollServer        string
	enrollCAFingerprint string
)

func runEnroll(cmd *cobra.Command, args []string) error {
	if enrollToken == "" {
		return fmt.Errorf("--token is required")
	}
	if enrollServer == "" {
		return fmt.Errorf("--server is required")
	}

	if enrollCAFingerprint == "" {
		fmt.Println("WARNING: --ca-fingerprint not provided. The server certificate will NOT be")
		fmt.Println("verified during enrollment. This is vulnerable to man-in-the-middle attacks.")
		fmt.Println("For production use, always supply the CA fingerprint shown in the Clank UI.")
		fmt.Println()
	}

	// Determine config/cert directory
	configDir := agent.DefaultConfigDir()
	if cfgFile != "" {
		configDir = cfgFile
	}

	fmt.Printf("Enrolling with %s...\n", enrollServer)

	// Collect system information
	info := sysinfo.Collect()
	info.AgentVersion = Version

	// Call the enrollment RPC (verifies CA fingerprint if provided)
	resp, err := grpcclient.Enroll(enrollServer, enrollToken, enrollCAFingerprint, info)
	if err != nil {
		return fmt.Errorf("enrollment failed: %w", err)
	}

	// Save certificates
	store := certs.NewStore(configDir)
	if err := store.Save(resp.ClientCert, resp.ClientKey, resp.CaCert); err != nil {
		return fmt.Errorf("saving certificates: %w", err)
	}

	// Write agent config
	cfg := &agent.Config{
		ServerID:     resp.ServerId,
		GRPCEndpoint: resp.GrpcEndpoint,
		CertDir:      configDir,
	}
	if err := agent.SaveConfig(configDir, cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	fmt.Printf("Enrolled successfully! Server ID: %s\n", resp.ServerId)
	fmt.Printf("Config saved to %s\n", configDir)
	fmt.Println("Run 'clank-agent run' to start the agent.")
	return nil
}

func init() {
	enrollCmd.Flags().StringVar(&enrollToken, "token", "", "enrollment token (required)")
	enrollCmd.Flags().StringVar(&enrollServer, "server", "", "gRPC endpoint host:port (required)")
	enrollCmd.Flags().StringVar(&enrollCAFingerprint, "ca-fingerprint", "",
		"SHA-256 fingerprint of the control plane CA cert (format: sha256:<hex>)")
	rootCmd.AddCommand(enrollCmd)
}
