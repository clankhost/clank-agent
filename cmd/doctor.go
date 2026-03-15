package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/clankhost/clank-agent/internal/agent"
	"github.com/clankhost/clank-agent/internal/doctor"
	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Run diagnostic checks on this server",
	Long:  "Checks Docker availability, certificates, connectivity, disk space, and system configuration.",
	RunE:  runDoctor,
}

var (
	doctorQuiet bool
	doctorJSON  bool
)

func runDoctor(cmd *cobra.Command, args []string) error {
	configDir := agent.DefaultConfigDir()
	if cfgFile != "" {
		configDir = cfgFile
	}

	// Load config for connectivity checks (may not exist yet)
	cfg, _ := agent.LoadConfig(configDir)
	grpcEndpoint := ""
	if cfg != nil {
		grpcEndpoint = cfg.GRPCEndpoint
	}

	runner := doctor.NewRunner()
	runner.Add("docker", doctor.CheckDocker)
	runner.Add("docker_socket", doctor.CheckDockerSocket)
	runner.Add("docker_group", doctor.CheckDockerGroup)
	runner.Add("disk_space", doctor.CheckDiskSpace)
	runner.Add("config", func() doctor.CheckResult { return doctor.CheckConfigExists(configDir) })
	runner.Add("certificates", func() doctor.CheckResult { return doctor.CheckCertsValid(configDir) })
	runner.Add("grpc", func() doctor.CheckResult { return doctor.CheckGRPCConnectivity(grpcEndpoint) })
	runner.Add("systemd", doctor.CheckSystemdService)
	runner.Add("tailscale", doctor.CheckTailscale)

	results := runner.Run()

	if doctorJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}

	if doctorQuiet {
		if doctor.HasErrors(results) {
			os.Exit(1)
		}
		return nil
	}

	// Pretty output
	hasIssues := false
	for _, r := range results {
		icon := "\033[32m[OK]\033[0m  "
		switch r.Status {
		case doctor.Warn:
			icon = "\033[33m[WARN]\033[0m"
			hasIssues = true
		case doctor.Error:
			icon = "\033[31m[FAIL]\033[0m"
			hasIssues = true
		}
		fmt.Printf("%s %-16s %s\n", icon, r.Name, r.Message)
		if r.Fix != "" && r.Status != doctor.OK {
			fmt.Printf("       %-16s Fix: %s\n", "", r.Fix)
		}
	}

	fmt.Println()
	if hasIssues {
		fmt.Println("Some checks need attention. See above for details.")
	} else {
		fmt.Println("All checks passed.")
	}

	if doctor.HasErrors(results) {
		os.Exit(1)
	}
	return nil
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorQuiet, "quiet", false, "exit code only (0=ok, 1=errors)")
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "output as JSON")
	rootCmd.AddCommand(doctorCmd)
}
