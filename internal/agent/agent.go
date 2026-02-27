package agent

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/anaremore/clank/apps/agent/internal/certs"
	"github.com/anaremore/clank/apps/agent/internal/grpcclient"
	"github.com/anaremore/clank/apps/agent/internal/sysinfo"
)

const (
	heartbeatInterval = 30 * time.Second
	reconnectBaseWait = 2 * time.Second
	reconnectMaxWait  = 60 * time.Second
)

// Agent manages the lifecycle of the gRPC connection and heartbeat loop.
type Agent struct {
	cfg          *Config
	agentVersion string
	certStore    *certs.Store
}

// New creates a new Agent from the given config.
func New(cfg *Config, agentVersion string) (*Agent, error) {
	store := certs.NewStore(cfg.CertDir)
	if !store.Exists() {
		return nil, fmt.Errorf("certificates not found in %s — run 'clank-agent enroll' first", cfg.CertDir)
	}
	return &Agent{
		cfg:          cfg,
		agentVersion: agentVersion,
		certStore:    store,
	}, nil
}

// Run connects to the control plane and runs the heartbeat loop.
// It reconnects on errors with exponential backoff.
// Returns when ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	wait := reconnectBaseWait

	for {
		err := a.connectAndStream(ctx)
		if ctx.Err() != nil {
			// Graceful shutdown
			return nil
		}
		if err != nil {
			log.Printf("Connection lost: %v", err)
		}

		log.Printf("Reconnecting in %s...", wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}

		// Exponential backoff
		wait = wait * 2
		if wait > reconnectMaxWait {
			wait = reconnectMaxWait
		}
	}
}

// connectAndStream establishes the bidi stream and runs the heartbeat loop.
func (a *Agent) connectAndStream(ctx context.Context) error {
	tlsCreds, err := a.certStore.TransportCredentials()
	if err != nil {
		return fmt.Errorf("loading TLS credentials: %w", err)
	}

	conn, err := grpcclient.Dial(a.cfg.GRPCEndpoint, tlsCreds)
	if err != nil {
		return fmt.Errorf("dialing %s: %w", a.cfg.GRPCEndpoint, err)
	}
	defer conn.Close()

	stream, err := grpcclient.OpenConnectStream(ctx, conn)
	if err != nil {
		return fmt.Errorf("opening stream: %w", err)
	}

	log.Println("Connected to control plane")

	// Reset backoff on successful connection
	// (caller handles this since we return nil on graceful close)

	// Start heartbeat sender in a goroutine
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	defer heartbeatCancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.sendHeartbeats(heartbeatCtx, stream)
	}()

	// Receive loop — listen for commands from control plane
	go func() {
		errCh <- grpcclient.ReceiveCommands(stream)
	}()

	// Wait for either goroutine to finish or context cancellation
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

func (a *Agent) sendHeartbeats(ctx context.Context, stream grpcclient.ConnectStream) error {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	// Send initial heartbeat immediately
	if err := grpcclient.SendHeartbeat(stream, a.collectInfo()); err != nil {
		return fmt.Errorf("sending heartbeat: %w", err)
	}
	log.Println("Sent initial heartbeat")

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := grpcclient.SendHeartbeat(stream, a.collectInfo()); err != nil {
				return fmt.Errorf("sending heartbeat: %w", err)
			}
			log.Println("Heartbeat sent")
		}
	}
}

func (a *Agent) collectInfo() *sysinfo.Info {
	info := sysinfo.Collect()
	info.AgentVersion = a.agentVersion
	return info
}
