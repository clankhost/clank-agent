package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Set at build time via ldflags.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the agent version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("clank-agent %s (commit: %s, built: %s)\n", Version, Commit, Date)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
