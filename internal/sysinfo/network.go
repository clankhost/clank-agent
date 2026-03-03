package sysinfo

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// NetworkInfo holds discovered network addresses.
type NetworkInfo struct {
	LANIPs                []string
	PublicIP              string
	TailscaleIP           string
	TailscaleHostname     string
	TailscaleCLIAvailable bool
}

// CollectNetworkInfo enumerates LAN IPs, detects public IP, and detects Tailscale.
func CollectNetworkInfo() NetworkInfo {
	info := NetworkInfo{}
	info.LANIPs = collectLANIPs()
	info.PublicIP = detectPublicIP()
	info.TailscaleIP, info.TailscaleHostname, info.TailscaleCLIAvailable = detectTailscale()
	return info
}

// collectLANIPs returns private IPv4 addresses, excluding loopback
// and Docker/veth interfaces.
func collectLANIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var ips []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if isDockerInterface(iface.Name) {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil {
				continue // skip IPv6
			}
			if isPrivateIP(ip) {
				ips = append(ips, ip.String())
			}
		}
	}
	return ips
}

// isDockerInterface returns true for Docker bridge and veth interfaces.
func isDockerInterface(name string) bool {
	if name == "docker0" {
		return true
	}
	if strings.HasPrefix(name, "br-") {
		return true
	}
	if strings.HasPrefix(name, "veth") {
		return true
	}
	return false
}

// isPrivateIP checks if an IPv4 address is in RFC 1918 private ranges.
func isPrivateIP(ip net.IP) bool {
	private := []net.IPNet{
		{IP: net.IP{10, 0, 0, 0}, Mask: net.CIDRMask(8, 32)},
		{IP: net.IP{172, 16, 0, 0}, Mask: net.CIDRMask(12, 32)},
		{IP: net.IP{192, 168, 0, 0}, Mask: net.CIDRMask(16, 32)},
	}
	for _, n := range private {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// detectPublicIP queries an external service to discover the host's
// public IPv4 address. Returns "" on any failure (timeout, no internet, etc).
// Uses a short timeout to avoid blocking heartbeats on slow networks.
func detectPublicIP() string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://api.ipify.org")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(body))
	// Basic validation: must parse as an IP
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}

// tailscaleStatus is the minimal subset of `tailscale status --json`.
type tailscaleStatus struct {
	Self struct {
		TailscaleIPs []string `json:"TailscaleIPs"`
		DNSName      string   `json:"DNSName"`
	} `json:"Self"`
}

// detectTailscale checks whether the tailscale CLI is on the PATH,
// and if so runs `tailscale status --json` to extract the node's
// first IP and DNS name. The cliAvailable flag distinguishes "not
// installed" from "installed but disconnected".
func detectTailscale() (ip, hostname string, cliAvailable bool) {
	if _, err := exec.LookPath("tailscale"); err != nil {
		return "", "", false
	}
	cliAvailable = true

	out, err := exec.Command("tailscale", "status", "--json").Output()
	if err != nil {
		return "", "", true // CLI exists but tailscale not running/connected
	}

	var status tailscaleStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return "", "", true
	}

	if len(status.Self.TailscaleIPs) > 0 {
		ip = status.Self.TailscaleIPs[0]
	}
	hostname = strings.TrimSuffix(status.Self.DNSName, ".")
	return ip, hostname, true
}
