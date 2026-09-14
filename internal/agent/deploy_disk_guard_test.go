package agent

import (
	"testing"

	"github.com/clankhost/clank-agent/internal/docker"
)

func TestRemoteDiskGuardUsesAgentReportedNonDefaultDockerRoot(t *testing.T) {
	result := buildDeployDiskGuardResult(
		&docker.DockerRootUsage{DockerRootDir: "/srv/docker-data", TotalBytes: 100 << 30, FreeBytes: 64 << 30},
		2<<30,
		&docker.CleanupSummary{},
		false,
	)
	if result["docker_root_dir"] != "/srv/docker-data" {
		t.Fatalf("wrong root: %v", result["docker_root_dir"])
	}
	if result["telemetry_source"] != "agent_live" {
		t.Fatalf("wrong source: %v", result["telemetry_source"])
	}
	if result["blocked"].(bool) {
		t.Fatal("healthy remote storage was blocked")
	}
}

func TestRemoteDiskGuardBlocksGenuineLowDiskAndForcePreservesWarning(t *testing.T) {
	usage := &docker.DockerRootUsage{DockerRootDir: "/var/lib/docker", TotalBytes: 100 << 30, FreeBytes: 2 << 30}
	blocked := buildDeployDiskGuardResult(usage, 0, &docker.CleanupSummary{}, false)
	if !blocked["blocked"].(bool) || !blocked["would_block"].(bool) {
		t.Fatal("low disk was not blocked")
	}
	forced := buildDeployDiskGuardResult(usage, 0, &docker.CleanupSummary{}, true)
	if forced["blocked"].(bool) || !forced["would_block"].(bool) || !forced["forced"].(bool) {
		t.Fatal("force semantics did not preserve would-block evidence")
	}
}
