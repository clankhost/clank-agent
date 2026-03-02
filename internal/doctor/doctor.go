package doctor

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/anaremore/clank/apps/agent/internal/docker"
	"github.com/shirou/gopsutil/v4/disk"
)

// Severity indicates the outcome of a diagnostic check.
type Severity int

const (
	OK Severity = iota
	Warn
	Error
)

func (s Severity) String() string {
	switch s {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Error:
		return "error"
	default:
		return "unknown"
	}
}

// CheckResult holds the outcome of a single diagnostic check.
type CheckResult struct {
	Name    string   `json:"name"`
	Status  Severity `json:"status"`
	Message string   `json:"message"`
	Fix     string   `json:"fix,omitempty"`
}

// CheckFunc is the signature for a diagnostic check.
type CheckFunc func() CheckResult

// Runner collects and executes checks in order.
type Runner struct {
	checks []namedCheck
}

type namedCheck struct {
	name string
	fn   CheckFunc
}

// NewRunner creates a new check runner.
func NewRunner() *Runner {
	return &Runner{}
}

// Add registers a check.
func (r *Runner) Add(name string, fn CheckFunc) {
	r.checks = append(r.checks, namedCheck{name: name, fn: fn})
}

// Run executes all registered checks and returns results.
func (r *Runner) Run() []CheckResult {
	results := make([]CheckResult, 0, len(r.checks))
	for _, c := range r.checks {
		result := c.fn()
		result.Name = c.name
		results = append(results, result)
	}
	return results
}

// HasErrors returns true if any check resulted in an Error severity.
func HasErrors(results []CheckResult) bool {
	for _, r := range results {
		if r.Status == Error {
			return true
		}
	}
	return false
}

// --- Built-in checks ---

// CheckDockerAvailable verifies the Docker daemon is reachable.
func CheckDockerAvailable() CheckResult {
	ok, detail := docker.IsDockerAvailable()
	if !ok {
		return CheckResult{
			Status:  Error,
			Message: detail,
			Fix:     "Install Docker Engine: https://docs.docker.com/engine/install/",
		}
	}
	return CheckResult{Status: OK, Message: fmt.Sprintf("Docker %s", detail)}
}

// CheckDockerVersion warns if Docker is below the minimum supported version.
func CheckDockerVersion() CheckResult {
	ok, detail := docker.IsDockerAvailable()
	if !ok {
		return CheckResult{Status: Error, Message: "cannot check version: Docker not available"}
	}
	// detail is the version string like "24.0.7"
	parts := strings.SplitN(detail, ".", 2)
	if len(parts) < 1 {
		return CheckResult{Status: Warn, Message: fmt.Sprintf("cannot parse Docker version: %s", detail)}
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return CheckResult{Status: Warn, Message: fmt.Sprintf("cannot parse Docker major version: %s", detail)}
	}
	if major < 24 {
		return CheckResult{
			Status:  Warn,
			Message: fmt.Sprintf("Docker %s (minimum recommended: 24.x)", detail),
			Fix:     "Upgrade Docker Engine: https://docs.docker.com/engine/install/",
		}
	}
	return CheckResult{Status: OK, Message: fmt.Sprintf("Docker %s", detail)}
}

// CheckDockerSocket verifies the Docker socket is accessible.
func CheckDockerSocket() CheckResult {
	sock := docker.DetectDockerSocket()
	// Strip the scheme for stat-checking unix sockets
	path := sock
	if strings.HasPrefix(path, "unix://") {
		path = strings.TrimPrefix(path, "unix://")
	}
	if strings.HasPrefix(sock, "npipe:") {
		// Windows named pipe — can't stat, just report detection
		return CheckResult{Status: OK, Message: fmt.Sprintf("Docker socket: %s", sock)}
	}
	if _, err := os.Stat(path); err != nil {
		return CheckResult{
			Status:  Error,
			Message: fmt.Sprintf("Docker socket not found: %s", path),
			Fix:     "Ensure Docker is running: sudo systemctl start docker",
		}
	}
	return CheckResult{Status: OK, Message: fmt.Sprintf("Docker socket: %s", sock)}
}

// CheckGRPCConnectivity tests TCP connectivity to the gRPC endpoint.
func CheckGRPCConnectivity(endpoint string) CheckResult {
	if endpoint == "" {
		return CheckResult{Status: Warn, Message: "no gRPC endpoint configured (not enrolled yet)"}
	}
	conn, err := net.DialTimeout("tcp", endpoint, 5*time.Second)
	if err != nil {
		return CheckResult{
			Status:  Error,
			Message: fmt.Sprintf("cannot reach %s: %v", endpoint, err),
			Fix:     "Check network/firewall — agent needs outbound TCP to the control plane",
		}
	}
	conn.Close()
	return CheckResult{Status: OK, Message: fmt.Sprintf("reachable: %s", endpoint)}
}

// CheckDiskSpace warns if free disk space is below 2GB.
func CheckDiskSpace() CheckResult {
	root := "/"
	if runtime.GOOS == "windows" {
		root = "C:\\"
	}
	usage, err := disk.Usage(root)
	if err != nil {
		return CheckResult{Status: Warn, Message: fmt.Sprintf("cannot check disk: %v", err)}
	}
	freeGB := float64(usage.Free) / (1024 * 1024 * 1024)
	if freeGB < 2.0 {
		return CheckResult{
			Status:  Warn,
			Message: fmt.Sprintf("%.1f GB free (recommended: >= 2 GB)", freeGB),
			Fix:     "Free up disk space — Docker images and builds require storage",
		}
	}
	return CheckResult{Status: OK, Message: fmt.Sprintf("%.1f GB free", freeGB)}
}

// CheckConfigExists verifies the agent config file is present.
func CheckConfigExists(configDir string) CheckResult {
	path := filepath.Join(configDir, "config.yaml")
	if _, err := os.Stat(path); err != nil {
		return CheckResult{
			Status:  Error,
			Message: fmt.Sprintf("config not found: %s", path),
			Fix:     "Run 'clank-agent enroll' to register with the control plane",
		}
	}
	return CheckResult{Status: OK, Message: fmt.Sprintf("config: %s", path)}
}

// CheckCertsValid verifies the client certificate exists and is not expired/expiring.
func CheckCertsValid(configDir string) CheckResult {
	certPath := filepath.Join(configDir, "client.crt")
	data, err := os.ReadFile(certPath)
	if err != nil {
		return CheckResult{
			Status:  Error,
			Message: "client certificate not found",
			Fix:     "Run 'clank-agent enroll' to obtain certificates",
		}
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return CheckResult{Status: Error, Message: "cannot parse client certificate PEM"}
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return CheckResult{Status: Error, Message: fmt.Sprintf("invalid certificate: %v", err)}
	}
	remaining := time.Until(cert.NotAfter)
	if remaining <= 0 {
		return CheckResult{
			Status:  Error,
			Message: fmt.Sprintf("certificate expired %s ago", (-remaining).Round(time.Hour)),
			Fix:     "Re-enroll or request certificate rotation from the control plane",
		}
	}
	if remaining < 7*24*time.Hour {
		return CheckResult{
			Status:  Warn,
			Message: fmt.Sprintf("certificate expires in %s", remaining.Round(time.Hour)),
			Fix:     "Certificate will be rotated automatically, or re-enroll manually",
		}
	}
	return CheckResult{
		Status:  OK,
		Message: fmt.Sprintf("certificate valid until %s (%s remaining)", cert.NotAfter.Format("2006-01-02"), remaining.Round(24*time.Hour)),
	}
}

// CheckSystemdService verifies the clank-agent systemd service is active (Linux only).
func CheckSystemdService() CheckResult {
	if runtime.GOOS != "linux" {
		return CheckResult{Status: OK, Message: "skipped (not Linux)"}
	}
	out, err := exec.Command("systemctl", "is-active", "clank-agent").Output()
	state := strings.TrimSpace(string(out))
	if err != nil || state != "active" {
		if state == "" {
			state = "not found"
		}
		return CheckResult{
			Status:  Warn,
			Message: fmt.Sprintf("systemd service: %s", state),
			Fix:     "Enable the service: sudo systemctl enable --now clank-agent",
		}
	}
	return CheckResult{Status: OK, Message: "systemd service: active"}
}

// CheckDockerGroup verifies the current user is in the docker group (Linux only).
func CheckDockerGroup() CheckResult {
	if runtime.GOOS != "linux" {
		return CheckResult{Status: OK, Message: "skipped (not Linux)"}
	}
	u, err := user.Current()
	if err != nil {
		return CheckResult{Status: Warn, Message: fmt.Sprintf("cannot determine user: %v", err)}
	}
	groups, err := u.GroupIds()
	if err != nil {
		return CheckResult{Status: Warn, Message: fmt.Sprintf("cannot read groups: %v", err)}
	}
	dockerGroup, err := user.LookupGroup("docker")
	if err != nil {
		return CheckResult{
			Status:  Warn,
			Message: "docker group does not exist",
			Fix:     "Create docker group: sudo groupadd docker",
		}
	}
	for _, gid := range groups {
		if gid == dockerGroup.Gid {
			return CheckResult{Status: OK, Message: fmt.Sprintf("user %s in docker group", u.Username)}
		}
	}
	return CheckResult{
		Status:  Warn,
		Message: fmt.Sprintf("user %s not in docker group", u.Username),
		Fix:     fmt.Sprintf("sudo usermod -aG docker %s && newgrp docker", u.Username),
	}
}

// CheckTailscale verifies Tailscale installation and connectivity.
// Tailscale is optional, so "not installed" is reported as OK.
func CheckTailscale() CheckResult {
	if _, err := exec.LookPath("tailscale"); err != nil {
		return CheckResult{
			Status:  OK,
			Message: "Tailscale not installed (optional)",
			Fix:     "Install for private HTTPS access: https://tailscale.com/download",
		}
	}

	out, err := exec.Command("tailscale", "status", "--json").Output()
	if err != nil {
		return CheckResult{
			Status:  Warn,
			Message: "Tailscale installed but not running or not connected",
			Fix:     "Run: sudo tailscale up",
		}
	}

	var status struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return CheckResult{
			Status:  Warn,
			Message: fmt.Sprintf("cannot parse Tailscale status: %v", err),
		}
	}

	hostname := strings.TrimSuffix(status.Self.DNSName, ".")
	if hostname == "" {
		return CheckResult{
			Status:  Warn,
			Message: "Tailscale connected but no DNS name assigned",
			Fix:     "Enable MagicDNS in Tailscale admin console",
		}
	}

	serveStatus := "not configured"
	serveOut, serveErr := exec.Command("tailscale", "serve", "status").Output()
	if serveErr == nil {
		s := strings.TrimSpace(string(serveOut))
		if s != "" {
			serveStatus = s
		}
	}

	return CheckResult{
		Status:  OK,
		Message: fmt.Sprintf("connected as %s (serve: %s)", hostname, serveStatus),
	}
}
