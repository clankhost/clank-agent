package docker

import "github.com/docker/docker/api/types/container"

// VolumeMount describes a named Docker volume to mount into a container.
type VolumeMount struct {
	Name      string // Docker volume name (stable across redeploys)
	MountPath string // Absolute path inside the container
}

// RunOpts configures a container to be started.
type RunOpts struct {
	Image         string
	Name          string
	Env           map[string]string
	Port          int
	Labels        map[string]string
	Network       string
	NetworkAlias  string // DNS alias for service discovery on the network
	CPULimit      float64
	MemoryLimitMB int
	Command       []string                // Override the image CMD (e.g. via CLANK_CONTAINER_CMD)
	Entrypoint    []string                // Override the image ENTRYPOINT (nil = keep image default)
	Volumes       []VolumeMount           // Persistent volume mounts
	Healthcheck   *container.HealthConfig // Docker HEALTHCHECK to inject (nil = keep image default)
}

// NetworkInfo describes a Docker network for pruning purposes.
type NetworkInfo struct {
	ID   string
	Name string
}

// ContainerInfo describes a running managed container (for heartbeat reporting).
type ContainerInfo struct {
	ContainerID string
	Name        string
	State       string
	Image       string
	Labels      map[string]string
}

// DockerRootUsage describes the filesystem backing Docker's storage root.
type DockerRootUsage struct {
	DockerRootDir string `json:"docker_root_dir"`
	TotalBytes    uint64 `json:"total_bytes"`
	UsedBytes     uint64 `json:"used_bytes"`
	FreeBytes     uint64 `json:"free_bytes"`
}

// CleanupSection summarizes one category of reclaimable Docker artifacts.
type CleanupSection struct {
	Count            int   `json:"count"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
}

// ArtifactOwnership contains only Clank-reserved labels. It is evidence for
// inventory reconciliation, never sufficient authorization for deletion.
type ArtifactOwnership struct {
	Managed            bool   `json:"managed"`
	ProspectivelyOwned bool   `json:"prospectively_owned"`
	OwnershipVersion   string `json:"ownership_version,omitempty"`
	ArtifactType       string `json:"artifact_type,omitempty"`
	DeploymentID       string `json:"deployment_id,omitempty"`
	ServiceID          string `json:"service_id,omitempty"`
	ServiceSlug        string `json:"service_slug,omitempty"`
	ProjectID          string `json:"project_id,omitempty"`
}

type ImageInventoryItem struct {
	ID                      string            `json:"id"`
	RepoTags                []string          `json:"repo_tags"`
	RepoDigests             []string          `json:"repo_digests"`
	SizeBytes               int64             `json:"size_bytes"`
	CreatedUnix             int64             `json:"created_unix"`
	ContainerIDs            []string          `json:"container_ids"`
	InUseByRunningContainer bool              `json:"in_use_by_running_container"`
	Ownership               ArtifactOwnership `json:"ownership"`
}

type ContainerInventoryItem struct {
	ID          string            `json:"id"`
	Names       []string          `json:"names"`
	State       string            `json:"state"`
	ImageID     string            `json:"image_id,omitempty"`
	ImageRef    string            `json:"image_ref,omitempty"`
	SizeRWBytes int64             `json:"size_rw_bytes"`
	Ownership   ArtifactOwnership `json:"ownership"`
}

// DockerArtifactInventory is bounded for safe transport. Complete=false is a
// hard planner stop: no artifact from a truncated observation may be deleted.
type DockerArtifactInventory struct {
	Version        int                      `json:"version"`
	Complete       bool                     `json:"complete"`
	ImageCount     int                      `json:"image_count"`
	ContainerCount int                      `json:"container_count"`
	Images         []ImageInventoryItem     `json:"images"`
	Containers     []ContainerInventoryItem `json:"containers"`
}

// CleanupSummary reports safe Docker cleanup preview/apply results.
type CleanupSummary struct {
	ProtectedImageRefs   []string                 `json:"protected_image_refs"`
	StoppedContainers    CleanupSection           `json:"stopped_containers"`
	UnusedImages         CleanupSection           `json:"unused_images"`
	BuildCache           CleanupSection           `json:"build_cache"`
	ReclaimableBytes     int64                    `json:"reclaimable_bytes"`
	Applied              bool                     `json:"applied"`
	ReclaimedBytesActual int64                    `json:"reclaimed_bytes_actual,omitempty"`
	Inventory            *DockerArtifactInventory `json:"inventory,omitempty"`
}
