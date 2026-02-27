package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "clank-agent",
	Short: "Clank agent — runs on managed servers",
	Long:  "The Clank agent enrolls with the control plane and maintains a persistent gRPC connection for receiving deploy commands and sending heartbeats.",
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command.
func Execute() error {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return err
	}
	return nil
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default ~/.clank-agent/config.yaml)")
}
