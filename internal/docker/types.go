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
	Command     []string                // Override the image CMD (e.g. via CLANK_CONTAINER_CMD)
	Entrypoint  []string                // Override the image ENTRYPOINT (nil = keep image default)
	Volumes     []VolumeMount           // Persistent volume mounts
	Healthcheck *container.HealthConfig // Docker HEALTHCHECK to inject (nil = keep image default)
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
