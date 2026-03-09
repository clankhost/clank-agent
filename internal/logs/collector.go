package logs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log"
	"sync"
	"time"

	clankv1 "github.com/anaremore/clank/apps/agent/gen/clank/v1"
	"github.com/anaremore/clank/apps/agent/internal/docker"
)

const (
	reconcileInterval = 10 * time.Second
	channelSize       = 4096
	dropLogInterval   = 30 * time.Second
)

// Collector watches managed containers and tails their logs.
type Collector struct {
	docker  *docker.Manager
	outCh   chan *clankv1.LogEntry
	tailers map[string]context.CancelFunc // containerID -> cancel
	mu      sync.Mutex
	dropped int64
}

// NewCollector creates a log collector.
func NewCollector(dm *docker.Manager) *Collector {
	return &Collector{
		docker:  dm,
		outCh:   make(chan *clankv1.LogEntry, channelSize),
		tailers: make(map[string]context.CancelFunc),
	}
}

// Entries returns the read-only channel of log entries.
func (c *Collector) Entries() <-chan *clankv1.LogEntry {
	return c.outCh
}

// Inject sends a log entry directly into the collector's output channel.
// Used by the build pipeline to stream build logs through the same
// gRPC StreamLogs infrastructure as runtime container logs.
// Non-blocking: drops the entry if the channel is full.
func (c *Collector) Inject(entry *clankv1.LogEntry) {
	select {
	case c.outCh <- entry:
	default:
		c.mu.Lock()
		c.dropped++
		c.mu.Unlock()
	}
}

// Run starts the reconciliation loop. It watches for new and removed
// containers, starting/stopping tailers accordingly. Blocks until ctx is done.
func (c *Collector) Run(ctx context.Context) {
	log.Println("[logs] Collector started")

	// Run an initial reconciliation immediately
	c.reconcile(ctx)

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	dropTicker := time.NewTicker(dropLogInterval)
	defer dropTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.stopAll()
			log.Println("[logs] Collector stopped")
			return
		case <-ticker.C:
			c.reconcile(ctx)
		case <-dropTicker.C:
			c.mu.Lock()
			if c.dropped > 0 {
				log.Printf("[logs] Dropped %d log lines due to backpressure", c.dropped)
				c.dropped = 0
			}
			c.mu.Unlock()
		}
	}
}

func (c *Collector) reconcile(ctx context.Context) {
	containers, err := c.docker.ListManagedContainers(ctx)
	if err != nil {
		log.Printf("[logs] Error listing containers: %v", err)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Build set of current running container IDs
	current := make(map[string]docker.ContainerInfo)
	for _, ci := range containers {
		if ci.State != "running" {
			continue
		}
		// Skip containers without deployment_id label
		if ci.Labels["clank.deployment_id"] == "" {
			continue
		}
		current[ci.ContainerID] = ci
	}

	// Start tailers for new containers
	for id, ci := range current {
		if _, exists := c.tailers[id]; !exists {
			tailerCtx, cancel := context.WithCancel(ctx)
			c.tailers[id] = cancel
			go c.tailContainer(tailerCtx, ci)
		}
	}

	// Stop tailers for removed containers
	for id, cancel := range c.tailers {
		if _, exists := current[id]; !exists {
			cancel()
			delete(c.tailers, id)
		}
	}
}

func (c *Collector) stopAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, cancel := range c.tailers {
		cancel()
		delete(c.tailers, id)
	}
}

func (c *Collector) tailContainer(ctx context.Context, ci docker.ContainerInfo) {
	deploymentID := ci.Labels["clank.deployment_id"]
	log.Printf("[logs] Tailing container %s (deployment %s)", ci.Name, deploymentID[:8])

	reader, err := c.docker.ContainerLogs(ctx, ci.ContainerID, true, "0")
	if err != nil {
		log.Printf("[logs] Error starting log tail for %s: %v", ci.Name, err)
		return
	}
	defer reader.Close()

	// Docker multiplexed stream format:
	// [8-byte header][payload]
	// Header: [stream_type(1), 0, 0, 0, size(4 big-endian)]
	// stream_type: 1=stdout, 2=stderr
	hdr := make([]byte, 8)
	for {
		if ctx.Err() != nil {
			return
		}

		// Read the 8-byte header
		_, err := io.ReadFull(reader, hdr)
		if err != nil {
			if ctx.Err() != nil || err == io.EOF {
				return
			}
			log.Printf("[logs] Error reading log header for %s: %v", ci.Name, err)
			return
		}

		streamType := "stdout"
		if hdr[0] == 2 {
			streamType = "stderr"
		}

		size := binary.BigEndian.Uint32(hdr[4:8])
		if size == 0 {
			continue
		}

		// Read the payload
		payload := make([]byte, size)
		_, err = io.ReadFull(reader, payload)
		if err != nil {
			if ctx.Err() != nil || err == io.EOF {
				return
			}
			log.Printf("[logs] Error reading log payload for %s: %v", ci.Name, err)
			return
		}

		// Parse lines from payload (may contain multiple lines)
		scanner := bufio.NewScanner(bytes.NewReader(payload))
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			entry := &clankv1.LogEntry{
				DeploymentId: deploymentID,
				ContainerId:  ci.ContainerID,
				Line:         line,
				TimestampNs:  time.Now().UnixNano(),
				Stream:       streamType,
			}

			// Non-blocking send for backpressure
			select {
			case c.outCh <- entry:
			default:
				c.mu.Lock()
				c.dropped++
				c.mu.Unlock()
			}
		}
	}
}

