package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"time"

	clankv1 "github.com/anaremore/clank/apps/agent/gen/clank/v1"
	"github.com/anaremore/clank/apps/agent/internal/docker"
)

const (
	collectInterval = 60 * time.Second
	channelSize     = 64
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

	ticker := time.NewTicker(collectInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[metrics] Collector stopped")
			return
		case <-ticker.C:
			c.collect(ctx)
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
