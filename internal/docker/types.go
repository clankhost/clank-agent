package docker

// RunOpts configures a container to be started.
type RunOpts struct {
	Image         string
	Name          string
	Env           map[string]string
	Port          int
	Labels        map[string]string
	Network       string
	CPULimit      float64
	MemoryLimitMB int
}

// ContainerInfo describes a running managed container (for heartbeat reporting).
type ContainerInfo struct {
	ContainerID string
	Name        string
	State       string
	Image       string
	Labels      map[string]string
}
