package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/anaremore/clank/apps/agent/internal/agent"
	"github.com/anaremore/clank/apps/agent/internal/docker"
	"github.com/spf13/cobra"
)

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the Clank agent from this server",
	Long:  "Stops the agent service, removes binaries, config, and optionally managed containers.",
	RunE:  runUninstall,
}

var (
	uninstallForce          bool
	uninstallKeepContainers bool
)

const (
	serviceName = "clank-agent"
	serviceFile = "/etc/systemd/system/clank-agent.service"
	installDir  = "/opt/clank"
	symlinkPath = "/usr/local/bin/clank-agent"
	systemUser  = "clank"
)

func runUninstall(cmd *cobra.Command, args []string) error {
	if runtime.GOOS == "linux" && os.Getuid() != 0 {
		return fmt.Errorf("uninstall must be run as root (use sudo)")
	}

	configDir := agent.DefaultConfigDir()
	if cfgFile != "" {
		configDir = cfgFile
	}

	// Show what will be removed
	fmt.Println("This will remove the Clank agent from this server:")
	fmt.Println()

	if runtime.GOOS == "linux" {
		fmt.Printf("  • Stop and remove systemd service (%s)\n", serviceName)
	}
	fmt.Printf("  • Remove binary (%s)\n", installDir)
	fmt.Printf("  • Remove symlink (%s)\n", symlinkPath)
	fmt.Printf("  • Remove config and certificates (%s)\n", configDir)

	if !uninstallKeepContainers {
		count := countManagedContainers()
		if count > 0 {
			fmt.Printf("  • Stop and remove %d managed container(s)\n", count)
		}
	}

	fmt.Println()

	if !uninstallForce {
		fmt.Print("Continue? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
		fmt.Println()
	}

	// 1. Stop and remove managed containers
	if !uninstallKeepContainers {
		removeAllManagedContainers()
	}

	// 2. Stop and remove systemd service (Linux)
	if runtime.GOOS == "linux" {
		removeSystemdService()
	}

	// 3. Remove binary and install directory
	if err := os.RemoveAll(installDir); err != nil {
		fmt.Printf("  ⚠ Could not remove %s: %v\n", installDir, err)
	} else {
		fmt.Printf("  ✓ Removed %s\n", installDir)
	}

	// 4. Remove symlink
	if err := os.Remove(symlinkPath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("  ⚠ Could not remove %s: %v\n", symlinkPath, err)
	} else if !os.IsNotExist(err) {
		fmt.Printf("  ✓ Removed %s\n", symlinkPath)
	}

	// 5. Remove config directory
	if err := os.RemoveAll(configDir); err != nil {
		fmt.Printf("  ⚠ Could not remove %s: %v\n", configDir, err)
	} else {
		fmt.Printf("  ✓ Removed %s\n", configDir)
	}

	fmt.Println()
	fmt.Println("Clank agent has been removed.")
	fmt.Println("You can also delete this server from the Clank dashboard.")
	return nil
}

func removeSystemdService() {
	// Stop
	_ = exec.Command("systemctl", "stop", serviceName).Run()
	fmt.Printf("  ✓ Stopped %s\n", serviceName)

	// Disable
	_ = exec.Command("systemctl", "disable", serviceName).Run()

	// Remove unit file
	if err := os.Remove(serviceFile); err != nil && !os.IsNotExist(err) {
		fmt.Printf("  ⚠ Could not remove %s: %v\n", serviceFile, err)
	} else {
		fmt.Printf("  ✓ Removed %s\n", serviceFile)
	}

	// Reload
	_ = exec.Command("systemctl", "daemon-reload").Run()
}

func removeAllManagedContainers() {
	mgr, err := docker.NewManager()
	if err != nil {
		return
	}
	containers, err := mgr.ListManagedContainers(context.Background())
	if err != nil || len(containers) == 0 {
		return
	}

	for _, c := range containers {
		name := c.Name
		if name == "" {
			name = c.ContainerID[:12]
		}
		_ = mgr.StopAndRemove(context.Background(), c.ContainerID)
		fmt.Printf("  ✓ Removed container %s\n", name)
	}
}

func init() {
	uninstallCmd.Flags().BoolVarP(&uninstallForce, "force", "f", false, "skip confirmation prompt")
	uninstallCmd.Flags().BoolVar(&uninstallKeepContainers, "keep-containers", false, "don't remove managed containers")
	rootCmd.AddCommand(uninstallCmd)
}
