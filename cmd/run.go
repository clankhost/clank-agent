package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	agentpkg "github.com/clankhost/clank-agent/internal/agent"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Start the agent and connect to the control plane",
	RunE:  runAgent,
}

func runAgent(cmd *cobra.Command, args []string) error {
	configDir := agentpkg.DefaultConfigDir()
	if cfgFile != "" {
		configDir = cfgFile
	}

	cfg, err := agentpkg.LoadConfig(configDir)
	if err != nil {
		return fmt.Errorf("loading config: %w\nRun 'clank-agent enroll' first", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle SIGINT/SIGTERM for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Printf("\nReceived %s, shutting down...\n", sig)
		cancel()
	}()

	a, err := agentpkg.New(cfg, Version, configDir)
	if err != nil {
		return fmt.Errorf("initializing agent: %w", err)
	}

	fmt.Printf("Agent started (server: %s, endpoint: %s)\n", cfg.ServerID, cfg.GRPCEndpoint)
	return a.Run(ctx)
}

func init() {
	rootCmd.AddCommand(runCmd)
}
