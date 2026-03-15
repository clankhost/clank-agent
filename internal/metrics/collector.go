package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"runtime"
	"time"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/docker"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
)

const (
	collectInterval     = 60 * time.Second
	diskUsageInterval   = 5 * time.Minute
	channelSize         = 64
)

// dockerStats mirrors the JSON structure returned by Docker stats API.
type dockerStats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage int64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage int64 `json:"system_cpu_usage"`
		OnlineCPUs     int   `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage int64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemCPUUsage int64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage int64 `json:"usage"`
		Limit int64 `json:"limit"`
		Stats struct {
			InactiveFile int64 `json:"inactive_file"`
		} `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes int64 `json:"rx_bytes"`
		TxBytes int64 `json:"tx_bytes"`
	} `json:"networks"`
}

// Collector periodically polls Docker stats for managed containers.
type Collector struct {
	docker   *docker.Manager
	serverID string
	outCh    chan *clankv1.MetricBatch
}

// NewCollector creates a metrics collector.
func NewCollector(dm *docker.Manager, serverID string) *Collector {
	return &Collector{
		docker:   dm,
		serverID: serverID,
		outCh:    make(chan *clankv1.MetricBatch, channelSize),
	}
}

// Batches returns the read-only channel of metric batches.
func (c *Collector) Batches() <-chan *clankv1.MetricBatch {
	return c.outCh
}

// Run starts the collection loop. Blocks until ctx is done.
func (c *Collector) Run(ctx context.Context) {
	log.Println("[metrics] Collector started")

	// Collect once immediately
	c.collect(ctx)
	c.collectHost(ctx)
	c.collectDiskUsage(ctx)

	ticker := time.NewTicker(collectInterval)
	diskTicker := time.NewTicker(diskUsageInterval)
	defer ticker.Stop()
	defer diskTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[metrics] Collector stopped")
			return
		case <-ticker.C:
			c.collect(ctx)
			c.collectHost(ctx)
		case <-diskTicker.C:
			c.collectDiskUsage(ctx)
		}
	}
}

func (c *Collector) collect(ctx context.Context) {
	containers, err := c.docker.ListManagedContainers(ctx)
	if err != nil {
		log.Printf("[metrics] Error listing containers: %v", err)
		return
	}

	now := time.Now().UnixNano()
	var metrics []*clankv1.Metric

	for _, ci := range containers {
		if ci.State != "running" {
			continue
		}
		deploymentID := ci.Labels["clank.deployment_id"]
		if deploymentID == "" {
			continue
		}
		serviceSlug := ci.Labels["clank.service_slug"]

		labels := map[string]string{
			"deployment_id": deploymentID,
			"service_slug":  serviceSlug,
			"container_name": ci.Name,
			"server_id":     c.serverID,
		}

		statsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		stats, err := c.docker.ContainerStatsOneShot(statsCtx, ci.ContainerID)
		cancel()
		if err != nil {
			log.Printf("[metrics] Error getting stats for %s: %v", ci.Name, err)
			continue
		}

		parsed, err := parseStats(stats.Body)
		stats.Body.Close()
		if err != nil {
			log.Printf("[metrics] Error parsing stats for %s: %v", ci.Name, err)
			continue
		}

		// CPU: cumulative usage in seconds (matches cAdvisor counter)
		cpuSeconds := float64(parsed.CPUStats.CPUUsage.TotalUsage) / 1e9

		// Memory: usage minus cache
		memUsage := parsed.MemoryStats.Usage - parsed.MemoryStats.Stats.InactiveFile
		if memUsage < 0 {
			memUsage = parsed.MemoryStats.Usage
		}

		// Network totals
		var rxBytes, txBytes int64
		for _, net := range parsed.Networks {
			rxBytes += net.RxBytes
			txBytes += net.TxBytes
		}

		metrics = append(metrics,
			&clankv1.Metric{Name: "container_cpu_usage_seconds_total", Value: cpuSeconds, TimestampNs: now, Labels: labels},
			&clankv1.Metric{Name: "container_memory_usage_bytes", Value: float64(memUsage), TimestampNs: now, Labels: labels},
			&clankv1.Metric{Name: "container_spec_memory_limit_bytes", Value: float64(parsed.MemoryStats.Limit), TimestampNs: now, Labels: labels},
			&clankv1.Metric{Name: "container_network_receive_bytes_total", Value: float64(rxBytes), TimestampNs: now, Labels: labels},
			&clankv1.Metric{Name: "container_network_transmit_bytes_total", Value: float64(txBytes), TimestampNs: now, Labels: labels},
		)
	}

	if len(metrics) == 0 {
		return
	}

	batch := &clankv1.MetricBatch{Metrics: metrics}

	// Non-blocking send
	select {
	case c.outCh <- batch:
	default:
		log.Printf("[metrics] Dropped batch (%d metrics) due to backpressure", len(metrics))
	}
}

func copyLabels(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src)+1)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// collectHost gathers host-level metrics (CPU, memory, disk, I/O) using gopsutil.
func (c *Collector) collectHost(ctx context.Context) {
	now := time.Now().UnixNano()
	baseLabels := map[string]string{
		"server_id": c.serverID,
	}
	var metrics []*clankv1.Metric

	// CPU times (aggregated across all CPUs)
	if times, err := cpu.TimesWithContext(ctx, false); err == nil && len(times) > 0 {
		t := times[0]
		for mode, val := range map[string]float64{
			"user":    t.User,
			"system":  t.System,
			"idle":    t.Idle,
			"iowait":  t.Iowait,
			"nice":    t.Nice,
			"irq":     t.Irq,
			"softirq": t.Softirq,
			"steal":   t.Steal,
		} {
			ml := copyLabels(baseLabels)
			ml["mode"] = mode
			metrics = append(metrics, &clankv1.Metric{
				Name: "node_cpu_seconds_total", Value: val,
				TimestampNs: now, Labels: ml,
			})
		}
	}

	// Load averages (Linux/macOS only; gracefully skipped on Windows)
	if avg, err := load.AvgWithContext(ctx); err == nil {
		metrics = append(metrics,
			&clankv1.Metric{Name: "node_load1", Value: avg.Load1, TimestampNs: now, Labels: copyLabels(baseLabels)},
			&clankv1.Metric{Name: "node_load5", Value: avg.Load5, TimestampNs: now, Labels: copyLabels(baseLabels)},
			&clankv1.Metric{Name: "node_load15", Value: avg.Load15, TimestampNs: now, Labels: copyLabels(baseLabels)},
		)
	}

	// Memory
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		metrics = append(metrics,
			&clankv1.Metric{Name: "node_memory_MemTotal_bytes", Value: float64(vm.Total), TimestampNs: now, Labels: copyLabels(baseLabels)},
			&clankv1.Metric{Name: "node_memory_MemAvailable_bytes", Value: float64(vm.Available), TimestampNs: now, Labels: copyLabels(baseLabels)},
		)
	}

	// Filesystem (root mount)
	rootPath := "/"
	if runtime.GOOS == "windows" {
		rootPath = "C:\\"
	}
	if du, err := disk.UsageWithContext(ctx, rootPath); err == nil {
		fsLabels := copyLabels(baseLabels)
		fsLabels["mountpoint"] = rootPath
		metrics = append(metrics,
			&clankv1.Metric{Name: "node_filesystem_size_bytes", Value: float64(du.Total), TimestampNs: now, Labels: fsLabels},
			&clankv1.Metric{Name: "node_filesystem_avail_bytes", Value: float64(du.Free), TimestampNs: now, Labels: fsLabels},
		)
	}

	// Disk I/O
	if ioCounters, err := disk.IOCountersWithContext(ctx); err == nil {
		for device, io := range ioCounters {
			dl := copyLabels(baseLabels)
			dl["device"] = device
			metrics = append(metrics,
				&clankv1.Metric{Name: "node_disk_read_bytes_total", Value: float64(io.ReadBytes), TimestampNs: now, Labels: dl},
				&clankv1.Metric{Name: "node_disk_written_bytes_total", Value: float64(io.WriteBytes), TimestampNs: now, Labels: dl},
			)
		}
	}

	if len(metrics) == 0 {
		return
	}

	batch := &clankv1.MetricBatch{Metrics: metrics}
	select {
	case c.outCh <- batch:
	default:
		log.Printf("[metrics] Dropped host batch (%d metrics) due to backpressure", len(metrics))
	}
}

// collectDiskUsage calls Docker's /system/df endpoint to gather disk
// consumption broken down by images, build cache, container layers, and
// individual volumes.  Runs on a slower cadence (5 min) because the API
// call can be expensive.
func (c *Collector) collectDiskUsage(ctx context.Context) {
	duCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	du, err := c.docker.DiskUsage(duCtx)
	if err != nil {
		log.Printf("[metrics] Error collecting disk usage: %v", err)
		return
	}

	now := time.Now().UnixNano()
	baseLabels := map[string]string{"server_id": c.serverID}
	var metrics []*clankv1.Metric

	metrics = append(metrics,
		&clankv1.Metric{Name: "docker_disk_images_bytes", Value: float64(du.ImagesBytes), TimestampNs: now, Labels: copyLabels(baseLabels)},
		&clankv1.Metric{Name: "docker_disk_buildcache_bytes", Value: float64(du.BuildCacheBytes), TimestampNs: now, Labels: copyLabels(baseLabels)},
		&clankv1.Metric{Name: "docker_disk_containers_bytes", Value: float64(du.ContainersBytes), TimestampNs: now, Labels: copyLabels(baseLabels)},
	)

	for _, vol := range du.Volumes {
		vl := copyLabels(baseLabels)
		vl["volume_name"] = vol.Name
		metrics = append(metrics, &clankv1.Metric{
			Name:        "docker_volume_size_bytes",
			Value:       float64(vol.SizeBytes),
			TimestampNs: now,
			Labels:      vl,
		})
	}

	batch := &clankv1.MetricBatch{Metrics: metrics}
	select {
	case c.outCh <- batch:
	default:
		log.Printf("[metrics] Dropped disk usage batch (%d metrics)", len(metrics))
	}
}

func parseStats(body io.Reader) (*dockerStats, error) {
	var stats dockerStats
	if err := json.NewDecoder(body).Decode(&stats); err != nil {
		return nil, fmt.Errorf("decoding stats JSON: %w", err)
	}
	return &stats, nil
}

func calculateCPUPercent(stats *dockerStats) float64 {
	cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage - stats.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(stats.CPUStats.SystemCPUUsage - stats.PreCPUStats.SystemCPUUsage)

	if systemDelta <= 0 || cpuDelta < 0 {
		return 0
	}

	cpus := stats.CPUStats.OnlineCPUs
	if cpus == 0 {
		cpus = 1
	}

	return (cpuDelta / systemDelta) * float64(cpus) * 100.0
}
