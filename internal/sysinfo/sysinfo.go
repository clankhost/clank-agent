package sysinfo

import (
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/shirou/gopsutil/v4/mem"
)

// Info holds collected system information.
// This is a local struct that gets mapped to the proto SystemInfo
// to avoid leaking proto types through the codebase.
type Info struct {
	Hostname          string
	OS                string
	Arch              string
	CPUCores          int64
	MemoryBytes       int64
	DockerVersion     string
	AgentVersion      string
	LANIPs            []string
	TailscaleIP       string
	TailscaleHostname string
}

// ContainerStatus describes a managed container (for heartbeat reporting).
type ContainerStatus struct {
	ContainerID string
	Name        string
	State       string
	Image       string
}

// Collect gathers system information from the host.
func Collect() *Info {
	hostname, _ := os.Hostname()

	var memBytes int64
	if v, err := mem.VirtualMemory(); err == nil {
		memBytes = int64(v.Total)
	}

	netInfo := CollectNetworkInfo()

	return &Info{
		Hostname:          hostname,
		OS:                runtime.GOOS,
		Arch:              runtime.GOARCH,
		CPUCores:          int64(runtime.NumCPU()),
		MemoryBytes:       memBytes,
		DockerVersion:     detectDocker(),
		LANIPs:            netInfo.LANIPs,
		TailscaleIP:       netInfo.TailscaleIP,
		TailscaleHostname: netInfo.TailscaleHostname,
	}
}

func detectDocker() string {
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
