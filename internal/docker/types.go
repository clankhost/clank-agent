package docker

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
	Command       []string // Override the image CMD (e.g. via CLANK_CONTAINER_CMD)
}

// ContainerInfo describes a running managed container (for heartbeat reporting).
type ContainerInfo struct {
	ContainerID string
	Name        string
	State       string
	Image       string
	Labels      map[string]string
}
