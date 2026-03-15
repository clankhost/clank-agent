package cmd

import (
	"fmt"
	"os"

	"github.com/clankhost/clank-agent/internal/agent"
	"github.com/clankhost/clank-agent/internal/certs"
	"github.com/clankhost/clank-agent/internal/grpcclient"
	"github.com/clankhost/clank-agent/internal/sysinfo"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/grpclog"
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
	enrollMode          string
)

func runEnroll(cmd *cobra.Command, args []string) error {
	if enrollToken == "" {
		return fmt.Errorf("--token is required")
	}
	if enrollServer == "" {
		return fmt.Errorf("--server is required")
	}

	isTunnel := enrollMode == "tunnel"

	if !isTunnel && enrollCAFingerprint == "" {
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

	// Enable verbose gRPC logging for direct-mode debugging
	if !isTunnel && os.Getenv("GRPC_DEBUG") == "1" {
		grpclog.SetLoggerV2(grpclog.NewLoggerV2WithVerbosity(os.Stderr, os.Stderr, os.Stderr, 99))
	}

	if isTunnel {
		fmt.Printf("Enrolling via tunnel with %s...\n", enrollServer)
	} else {
		fmt.Printf("Enrolling with %s...\n", enrollServer)
	}

	// Collect system information
	info := sysinfo.Collect()
	info.AgentVersion = Version

	// Tunnel mode: REST over HTTPS (Cloudflare gRPC proxy drops HTTP/2 trailers)
	// Direct mode: gRPC over mTLS
	var resp *grpcclient.EnrollResponse
	var err error
	if isTunnel {
		resp, err = grpcclient.EnrollTunnel(enrollServer, enrollToken, info)
	} else {
		resp, err = grpcclient.Enroll(enrollServer, enrollToken, enrollCAFingerprint, info)
	}
	if err != nil {
		return fmt.Errorf("enrollment failed: %w", err)
	}

	// Save certificates (useful for both modes — direct mode needs them,
	// tunnel mode stores them as backup)
	store := certs.NewStore(configDir)
	if err := store.Save(resp.ClientCert, resp.ClientKey, resp.CaCert); err != nil {
		return fmt.Errorf("saving certificates: %w", err)
	}

	// Determine auth mode and endpoint for the agent config
	authMode := "mtls"
	authToken := ""
	grpcEndpoint := resp.GrpcEndpoint
	if isTunnel {
		authMode = "token"
		authToken = resp.AuthToken
		if resp.TunnelEndpoint != "" {
			grpcEndpoint = resp.TunnelEndpoint
		}
	}

	cfg := &agent.Config{
		ServerID:         resp.ServerId,
		GRPCEndpoint:     grpcEndpoint,
		CertDir:          configDir,
		AuthMode:         authMode,
		AuthToken:        authToken,
		RegistryURL:      resp.RegistryUrl,
		RegistryUsername: resp.RegistryUsername,
		RegistryPassword: resp.RegistryPassword,
	}
	if err := agent.SaveConfig(configDir, cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	fmt.Printf("Enrolled successfully! Server ID: %s\n", resp.ServerId)
	fmt.Printf("Auth mode: %s\n", authMode)
	fmt.Printf("Config saved to %s\n", configDir)
	fmt.Println("")
	fmt.Println("The agent is running as a systemd service.")
	fmt.Println("Restart it to pick up the new config:")
	fmt.Println("  sudo systemctl restart clank-agent")
	fmt.Println("")
	fmt.Println("To watch logs:")
	fmt.Println("  journalctl -u clank-agent -f")
	return nil
}

func init() {
	enrollCmd.Flags().StringVar(&enrollToken, "token", "", "enrollment token (required)")
	enrollCmd.Flags().StringVar(&enrollServer, "server", "", "gRPC endpoint host:port (required)")
	enrollCmd.Flags().StringVar(&enrollCAFingerprint, "ca-fingerprint", "",
		"SHA-256 fingerprint of the control plane CA cert (format: sha256:<hex>)")
	enrollCmd.Flags().StringVar(&enrollMode, "mode", "direct",
		"connection mode: 'direct' for mTLS or 'tunnel' for Cloudflare Tunnel (JWT auth)")
	rootCmd.AddCommand(enrollCmd)
}
