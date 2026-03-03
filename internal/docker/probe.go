package docker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	probeConnectTimeout = 3 * time.Second
	probeHTTPTimeout    = 3 * time.Second
	maxProbePorts       = 10
	probeOverallTimeout = 15 * time.Second
)

// ProbePort checks whether a port is open and whether it speaks HTTP.
// Returns "http", "tcp", or "closed".
func ProbePort(ctx context.Context, ip string, port int) string {
	addr := net.JoinHostPort(ip, strconv.Itoa(port))

	// TCP connect
	conn, err := net.DialTimeout("tcp", addr, probeConnectTimeout)
	if err != nil {
		return "closed"
	}
	conn.Close()

	// Try HTTP GET
	client := &http.Client{Timeout: probeHTTPTimeout}
	url := fmt.Sprintf("http://%s/", addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "tcp"
	}
	resp, err := client.Do(req)
	if err != nil {
		return "tcp"
	}
	resp.Body.Close()
	// Any HTTP response (even 404/500) means the port speaks HTTP
	return "http"
}

// ProbeAllPorts probes multiple ports in parallel and returns results.
// Ports are deduplicated and capped at maxProbePorts.
func ProbeAllPorts(ctx context.Context, ip string, ports []int) []DiscoveredPort {
	// Deduplicate
	seen := make(map[int]bool)
	var unique []int
	for _, p := range ports {
		if p > 0 && !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}
	if len(unique) > maxProbePorts {
		unique = unique[:maxProbePorts]
	}
	if len(unique) == 0 {
		return nil
	}

	// Overall timeout context
	probeCtx, cancel := context.WithTimeout(ctx, probeOverallTimeout)
	defer cancel()

	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results []DiscoveredPort
	)

	for _, port := range unique {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			protocol := ProbePort(probeCtx, ip, p)
			mu.Lock()
			results = append(results, DiscoveredPort{
				Port:     p,
				Protocol: protocol,
				Source:   "probe",
			})
			mu.Unlock()
		}(port)
	}

	wg.Wait()
	return results
}
