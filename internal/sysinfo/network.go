package sysinfo

import (
	"encoding/json"
	"net"
	"os/exec"
	"strings"
)

// NetworkInfo holds discovered network addresses.
type NetworkInfo struct {
	LANIPs            []string
	TailscaleIP       string
	TailscaleHostname string
}

// CollectNetworkInfo enumerates LAN IPs and detects Tailscale.
func CollectNetworkInfo() NetworkInfo {
	info := NetworkInfo{}
	info.LANIPs = collectLANIPs()
	info.TailscaleIP, info.TailscaleHostname = detectTailscale()
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

// tailscaleStatus is the minimal subset of `tailscale status --json`.
type tailscaleStatus struct {
	Self struct {
		TailscaleIPs []string `json:"TailscaleIPs"`
		DNSName      string   `json:"DNSName"`
	} `json:"Self"`
}

// detectTailscale runs `tailscale status --json` and extracts the
// node's first IP and DNS name. Returns empty strings if Tailscale
// is not installed or not connected.
func detectTailscale() (ip, hostname string) {
	out, err := exec.Command("tailscale", "status", "--json").Output()
	if err != nil {
		return "", ""
	}

	var status tailscaleStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return "", ""
	}

	if len(status.Self.TailscaleIPs) > 0 {
		ip = status.Self.TailscaleIPs[0]
	}
	hostname = strings.TrimSuffix(status.Self.DNSName, ".")
	return ip, hostname
}
