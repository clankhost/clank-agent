package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

const launchdLabel = "com.clank.agent"

var serviceCmd = &cobra.Command{
	Use:   "service",
	Short: "Manage the agent background service",
}

var serviceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install and start the agent as a background service",
	Long: `On macOS: creates a launchd plist and loads it (auto-starts on boot).
On Linux: the install script already creates a systemd service.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		switch runtime.GOOS {
		case "darwin":
			return installLaunchd()
		case "linux":
			fmt.Println("On Linux, the agent service is managed by systemd.")
			fmt.Println("Check status: sudo systemctl status clank-agent")
			return nil
		default:
			return fmt.Errorf("service install is not supported on %s", runtime.GOOS)
		}
	},
}

var serviceUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop and remove the agent background service",
	RunE: func(cmd *cobra.Command, args []string) error {
		switch runtime.GOOS {
		case "darwin":
			return uninstallLaunchd()
		default:
			return fmt.Errorf("service uninstall is not supported on %s", runtime.GOOS)
		}
	},
}

var serviceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show agent service status",
	RunE: func(cmd *cobra.Command, args []string) error {
		switch runtime.GOOS {
		case "darwin":
			return statusLaunchd()
		default:
			return fmt.Errorf("service status is not supported on %s", runtime.GOOS)
		}
	},
}

func installLaunchd() error {
	// Resolve paths
	binPath, err := filepath.Abs(os.Args[0])
	if err != nil {
		binPath = "/opt/clank/bin/clank-agent"
	}

	configDir := cfgFile
	if configDir == "" {
		configDir = "/etc/clank-agent"
	}

	plistPath := fmt.Sprintf("/Library/LaunchDaemons/%s.plist", launchdLabel)
	logPath := "/var/log/clank-agent.log"

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>run</string>
        <string>--config=%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>`, launchdLabel, binPath, configDir, logPath, logPath)

	// Write plist
	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		return fmt.Errorf("writing plist: %w (try running with sudo)", err)
	}

	// Unload first if already loaded (ignore errors)
	exec.Command("launchctl", "bootout", "system/"+launchdLabel).Run()

	// Load the service
	out, err := exec.Command("launchctl", "bootstrap", "system", plistPath).CombinedOutput()
	if err != nil {
		// Fallback to legacy load command
		out, err = exec.Command("launchctl", "load", "-w", plistPath).CombinedOutput()
		if err != nil {
			return fmt.Errorf("loading service: %s", strings.TrimSpace(string(out)))
		}
	}

	fmt.Println("Agent service installed and started.")
	fmt.Printf("  Logs: tail -f %s\n", logPath)
	fmt.Printf("  Stop: sudo launchctl bootout system/%s\n", launchdLabel)
	fmt.Printf("  Remove: sudo clank-agent service uninstall\n")
	return nil
}

func uninstallLaunchd() error {
	plistPath := fmt.Sprintf("/Library/LaunchDaemons/%s.plist", launchdLabel)

	// Stop the service
	exec.Command("launchctl", "bootout", "system/"+launchdLabel).Run()

	// Remove plist
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing plist: %w", err)
	}

	fmt.Println("Agent service stopped and removed.")
	return nil
}

func statusLaunchd() error {
	out, err := exec.Command("launchctl", "print", "system/"+launchdLabel).CombinedOutput()
	if err != nil {
		fmt.Println("Agent service is not running.")
		return nil
	}
	fmt.Println(strings.TrimSpace(string(out)))
	return nil
}

func init() {
	serviceCmd.AddCommand(serviceInstallCmd)
	serviceCmd.AddCommand(serviceUninstallCmd)
	serviceCmd.AddCommand(serviceStatusCmd)
	rootCmd.AddCommand(serviceCmd)
}
