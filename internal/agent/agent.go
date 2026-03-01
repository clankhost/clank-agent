package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/anaremore/clank/apps/agent/internal/build"
	"github.com/anaremore/clank/apps/agent/internal/certs"
	"github.com/anaremore/clank/apps/agent/internal/deploy"
	"github.com/anaremore/clank/apps/agent/internal/docker"
	"github.com/anaremore/clank/apps/agent/internal/grpcclient"
	"github.com/anaremore/clank/apps/agent/internal/logs"
	"github.com/anaremore/clank/apps/agent/internal/metrics"
	"github.com/anaremore/clank/apps/agent/internal/selfupdate"
	"github.com/anaremore/clank/apps/agent/internal/sysinfo"
	"google.golang.org/grpc"
)

const (
	heartbeatInterval = 30 * time.Second
	reconnectBaseWait = 2 * time.Second
	reconnectMaxWait  = 60 * time.Second
)

const streamReconnectWait = 5 * time.Second

// Agent manages the lifecycle of the gRPC connection and heartbeat loop.
type Agent struct {
	cfg          *Config
	cfgDir       string
	agentVersion string
	certStore    *certs.Store
	dockerMgr    *docker.Manager
	builder      *build.Builder
	deployer     *deploy.Deployer
	handler      *CommandHandler
	logCollector *logs.Collector
	metCollector *metrics.Collector
}

// New creates a new Agent from the given config.
func New(cfg *Config, agentVersion string, cfgDir string) (*Agent, error) {
	store := certs.NewStore(cfg.CertDir)
	// Cert files are required for mTLS mode; tunnel mode uses JWT instead
	if cfg.AuthMode != "token" && !store.Exists() {
		return nil, fmt.Errorf("certificates not found in %s — run 'clank-agent enroll' first", cfg.CertDir)
	}

	// Initialize Docker manager
	dockerMgr, err := docker.NewManager()
	if err != nil {
		return nil, fmt.Errorf("initializing docker: %w", err)
	}

	b := build.NewBuilder(dockerMgr)
	d := deploy.NewDeployer(dockerMgr)
	h := NewCommandHandler(dockerMgr, b, d, cfg, cfgDir, agentVersion)
	lc := logs.NewCollector(dockerMgr)
	mc := metrics.NewCollector(dockerMgr, cfg.ServerID)

	return &Agent{
		cfg:          cfg,
		cfgDir:       cfgDir,
		agentVersion: agentVersion,
		certStore:    store,
		dockerMgr:    dockerMgr,
		builder:      b,
		deployer:     d,
		handler:      h,
		logCollector: lc,
		metCollector: mc,
	}, nil
}

// Run connects to the control plane and runs the heartbeat loop.
// It reconnects on errors with exponential backoff.
// Returns when ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	// Post-update self-check: verify the new binary can connect.
	// If this fails repeatedly, roll back to the previous binary.
	a.checkPendingUpdate(ctx)

	// Ensure Traefik is running on this host
	if err := a.dockerMgr.EnsureTraefik(ctx); err != nil {
		log.Printf("Warning: could not ensure Traefik is running: %v", err)
	}

	// Start cloudflared if tunnel config was persisted from a previous run
	if a.cfg.TunnelToken != "" {
		if err := a.dockerMgr.EnsureCloudflared(ctx, a.cfg.TunnelToken); err != nil {
			log.Printf("Warning: could not start cloudflared: %v", err)
		}
	}

	// Start log and metrics collectors (survive reconnections)
	go a.logCollector.Run(ctx)
	go a.metCollector.Run(ctx)

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
	var conn *grpc.ClientConn
	var err error

	if a.cfg.AuthMode == "token" {
		// Tunnel mode: standard TLS + JWT bearer token
		conn, err = grpcclient.DialTunnelWithAuth(a.cfg.GRPCEndpoint, a.cfg.AuthToken)
	} else {
		// Direct mode: mTLS with client certificate
		tlsCreds, credErr := a.certStore.TransportCredentials()
		if credErr != nil {
			return fmt.Errorf("loading TLS credentials: %w", credErr)
		}
		conn, err = grpcclient.Dial(a.cfg.GRPCEndpoint, tlsCreds)
	}
	if err != nil {
		return fmt.Errorf("dialing %s: %w", a.cfg.GRPCEndpoint, err)
	}
	defer conn.Close()

	stream, err := grpcclient.OpenConnectStream(ctx, conn)
	if err != nil {
		return fmt.Errorf("opening stream: %w", err)
	}

	log.Println("Connected to control plane")

	// Start heartbeat sender in a goroutine
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.sendHeartbeats(streamCtx, stream)
	}()

	// Receive loop — listen for commands from control plane
	handlers := grpcclient.CommandHandlers{
		OnDeploy:           a.handler.HandleDeploy,
		OnContainerCommand: a.handler.HandleContainerCommand,
		OnTunnelConfig:     a.handler.HandleTunnelConfig,
		OnUpdate:           a.handler.HandleUpdate,
		OnEndpoint:         a.handler.HandleEndpoint,
	}
	go func() {
		errCh <- grpcclient.ReceiveCommands(ctx, stream, handlers)
	}()

	// Start log and metrics streamers (per-connection, cancelled on disconnect)
	go a.runLogStreamer(streamCtx, conn)
	go a.runMetricStreamer(streamCtx, conn)

	// Wait for either goroutine to finish or context cancellation
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

// runLogStreamer sends log entries to the control plane, reconnecting the
// stream on failure. The underlying log collector channel survives reconnects.
func (a *Agent) runLogStreamer(ctx context.Context, conn *grpc.ClientConn) {
	for {
		err := grpcclient.StreamLogs(ctx, conn, a.logCollector.Entries())
		if ctx.Err() != nil {
			return
		}
		log.Printf("[logs] Stream disconnected: %v, reconnecting in %s...", err, streamReconnectWait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(streamReconnectWait):
		}
	}
}

// runMetricStreamer sends metric batches to the control plane, reconnecting
// the stream on failure.
func (a *Agent) runMetricStreamer(ctx context.Context, conn *grpc.ClientConn) {
	for {
		err := grpcclient.StreamMetrics(ctx, conn, a.metCollector.Batches())
		if ctx.Err() != nil {
			return
		}
		log.Printf("[metrics] Stream disconnected: %v, reconnecting in %s...", err, streamReconnectWait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(streamReconnectWait):
		}
	}
}

func (a *Agent) sendHeartbeats(ctx context.Context, stream grpcclient.ConnectStream) error {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	// Send initial heartbeat immediately
	info, containers := a.collectInfo()
	if err := grpcclient.SendHeartbeat(stream, info, containers); err != nil {
		return fmt.Errorf("sending heartbeat: %w", err)
	}
	log.Println("Sent initial heartbeat")

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			info, containers := a.collectInfo()
			if err := grpcclient.SendHeartbeat(stream, info, containers); err != nil {
				return fmt.Errorf("sending heartbeat: %w", err)
			}
			log.Println("Heartbeat sent")
		}
	}
}

// checkPendingUpdate handles post-update verification after a binary replacement.
// If an update-state.json file exists with status "pending", the agent verifies
// connectivity to the control plane. After 3 failed attempts, it rolls back
// to the previous binary and exits (systemd restarts with the old version).
func (a *Agent) checkPendingUpdate(ctx context.Context) {
	state := selfupdate.LoadState(a.cfgDir)
	if state == nil || state.Status != "pending" {
		return
	}

	state.Attempts++
	log.Printf("[update] Post-update check (attempt %d/3): verifying connectivity...", state.Attempts)

	if state.Attempts > 3 {
		log.Printf("[update] Too many failed startup attempts — rolling back")
		if err := selfupdate.Rollback(); err != nil {
			log.Printf("[update] Rollback failed: %v", err)
		}
		selfupdate.ClearState(a.cfgDir)
		log.Printf("[update] Exiting for systemd restart with previous binary")
		os.Exit(1)
	}

	// Save incremented attempt count before connectivity test
	selfupdate.SaveState(a.cfgDir, state)

	// Verify we can reach the control plane
	if a.verifyConnectivity(ctx) {
		log.Printf("[update] Connectivity verified — update from %s to %s confirmed", state.FromVersion, state.ToVersion)
		selfupdate.ClearState(a.cfgDir)
		selfupdate.CleanupBackup()
	} else {
		log.Printf("[update] Connectivity check failed (attempt %d/3)", state.Attempts)
		// Don't rollback yet — let next restart increment attempts
	}
}

// verifyConnectivity attempts to dial the gRPC endpoint and open a stream
// within a 30-second timeout. Returns true if successful.
func (a *Agent) verifyConnectivity(ctx context.Context) bool {
	verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var conn *grpc.ClientConn
	var err error

	if a.cfg.AuthMode == "token" {
		conn, err = grpcclient.DialTunnelWithAuth(a.cfg.GRPCEndpoint, a.cfg.AuthToken)
	} else {
		tlsCreds, credErr := a.certStore.TransportCredentials()
		if credErr != nil {
			log.Printf("[update] Failed to load TLS credentials: %v", credErr)
			return false
		}
		conn, err = grpcclient.Dial(a.cfg.GRPCEndpoint, tlsCreds)
	}
	if err != nil {
		log.Printf("[update] Failed to dial control plane: %v", err)
		return false
	}
	defer conn.Close()

	stream, err := grpcclient.OpenConnectStream(verifyCtx, conn)
	if err != nil {
		log.Printf("[update] Failed to open stream: %v", err)
		return false
	}

	// Send a single heartbeat to confirm the stream works
	info, containers := a.collectInfo()
	if err := grpcclient.SendHeartbeat(stream, info, containers); err != nil {
		log.Printf("[update] Failed to send heartbeat: %v", err)
		return false
	}

	return true
}

func (a *Agent) collectInfo() (*sysinfo.Info, []sysinfo.ContainerStatus) {
	info := sysinfo.Collect()
	info.AgentVersion = a.agentVersion

	// Collect managed container statuses for heartbeat
	var statuses []sysinfo.ContainerStatus
	managed, err := a.dockerMgr.ListManagedContainers(context.Background())
	if err == nil {
		for _, c := range managed {
			statuses = append(statuses, sysinfo.ContainerStatus{
				ContainerID: c.ContainerID,
				Name:        c.Name,
				State:       c.State,
				Image:       c.Image,
			})
		}
	}

	return info, statuses
}
