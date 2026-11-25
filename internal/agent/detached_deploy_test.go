package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	clankv1 "github.com/clankhost/clank-agent/gen/clank/v1"
	"github.com/clankhost/clank-agent/internal/grpcclient"
)

func TestDetachedCommandContextSurvivesStreamCancelButHonorsRootShutdown(t *testing.T) {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	a := &Agent{rootCtx: rootCtx}

	streamCtx, streamCancel := context.WithCancel(rootCtx)
	deployCtx, deployCancel := a.detachedCommandContext()
	defer deployCancel()

	// Cancel the per-connection gRPC stream (simulating control-plane restart / reconnect).
	streamCancel()
	if streamCtx.Err() == nil {
		t.Fatal("expected streamCtx to be cancelled")
	}

	// Detached deploy context must remain alive after streamCtx cancellation.
	select {
	case <-deployCtx.Done():
		t.Fatalf("detached deploy context was cancelled by stream disconnect: %v", deployCtx.Err())
	default:
	}

	// Verify deadline is bounded (~30 minutes).
	deadline, ok := deployCtx.Deadline()
	if !ok {
		t.Fatal("expected detached deploy context to have a bounded timeout")
	}
	remaining := time.Until(deadline)
	if remaining < 29*time.Minute || remaining > 31*time.Minute {
		t.Fatalf("detached timeout = %v, want ~30m", remaining)
	}

	// Cancelling rootCtx (agent process shutdown) MUST cancel the detached deploy context.
	rootCancel()
	select {
	case <-deployCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("detached deploy context was not cancelled when rootCtx shut down")
	}
}

func TestDetachedBuildSurvivesStreamCancelAndFlushesPendingResultOnReconnect(t *testing.T) {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	h := &CommandHandler{
		buildSem: make(chan struct{}, 2),
	}
	a := &Agent{
		rootCtx: rootCtx,
		handler: h,
	}

	var r streamRouter
	fakeA := newFakeConnectStream()
	fakeB := newFakeConnectStream()
	connA := r.attach(fakeA)

	streamCtx, streamCancel := context.WithCancel(rootCtx)
	streamDisconnected := make(chan struct{})
	buildFinished := make(chan struct{})

	// Simulate an in-flight deploy/build triggered via ReceiveCommands on streamCtx,
	// using the agent's detachedCommandContext pattern.
	handlers := grpcclient.CommandHandlers{
		OnDeploy: func(_ context.Context, stream grpcclient.ConnectStream, cmd *clankv1.DeployCommand) {
			deployCtx, cancel := a.detachedCommandContext()
			defer cancel()

			// Wait until stream A disconnects and streamCtx is cancelled.
			<-streamDisconnected

			// Confirm our build context is still alive!
			if err := deployCtx.Err(); err != nil {
				t.Errorf("build killed by stream disconnect: %v", err)
				close(buildFinished)
				return
			}

			// Try to report completion while stream is still disconnected (no live stream).
			for _, st := range []string{"built", "active"} {
				msg := &clankv1.AgentMessage{
					Payload: &clankv1.AgentMessage_DeployProgress{
						DeployProgress: &clankv1.DeployProgress{
							DeploymentId: cmd.GetDeploymentId(),
							Status:       st,
						},
					},
				}
				if err := stream.Send(msg); err != nil {
					if terminalDeployStatuses[st] {
						h.queuePendingResult(msg)
					}
				}
			}
			close(buildFinished)
		},
	}

	recvDone := make(chan error, 1)
	go func() {
		recvDone <- grpcclient.ReceiveCommands(streamCtx, r.routed(connA), handlers)
	}()

	fakeA.recv <- &clankv1.ControlMessage{
		Payload: &clankv1.ControlMessage_Deploy{
			Deploy: &clankv1.DeployCommand{DeploymentId: "dep-reconnect-1"},
		},
	}

	// Disconnect stream A and cancel streamCtx mid-build.
	close(fakeA.recv)
	if err := <-recvDone; err != nil {
		t.Fatalf("ReceiveCommands: %v", err)
	}
	r.detach(connA)
	streamCancel()
	close(streamDisconnected)

	// Wait for the detached build/deploy to finish while disconnected.
	select {
	case <-buildFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for detached build to complete")
	}

	if got := h.PendingResultCount(); got != 2 {
		t.Fatalf("pending results = %d, want 2 ([built active])", got)
	}
	if got := fakeA.statuses(); len(got) != 0 {
		t.Fatalf("dead stream A should have received nothing, got %v", got)
	}

	// Reconnect on stream B and drain pending results automatically.
	connB := r.attach(fakeB)
	defer r.detach(connB)
	h.DrainPendingResults(connB)

	if got := h.PendingResultCount(); got != 0 {
		t.Fatalf("pending results after drain = %d, want 0", got)
	}
	if got := fakeB.statuses(); len(got) != 2 || got[0] != "built" || got[1] != "active" {
		t.Fatalf("reconnected stream B received %v, want [built active]", got)
	}
}

type failingSendStream struct {
	*fakeConnectStream
	failAfter int
	attempts  int
}

func (f *failingSendStream) Send(m *clankv1.AgentMessage) error {
	f.attempts++
	if f.attempts > f.failAfter {
		return errors.New("stream closed mid-drain")
	}
	return f.fakeConnectStream.Send(m)
}

func TestDrainPendingResultsPreservesOrderOnPartialFailure(t *testing.T) {
	h := &CommandHandler{}
	h.queuePendingResult(progress("built"))
	h.queuePendingResult(progress("active"))
	h.queuePendingResult(progress("failed"))

	partial := &failingSendStream{
		fakeConnectStream: newFakeConnectStream(),
		failAfter:         1,
	}
	h.DrainPendingResults(partial)

	if got := partial.statuses(); len(got) != 1 || got[0] != "built" {
		t.Fatalf("first drain sent %v, want [built]", got)
	}
	if got := h.PendingResultCount(); got != 2 {
		t.Fatalf("remaining pending = %d, want 2", got)
	}

	healthy := newFakeConnectStream()
	h.DrainPendingResults(healthy)
	if got := healthy.statuses(); len(got) != 2 || got[0] != "active" || got[1] != "failed" {
		t.Fatalf("second drain sent %v, want [active failed]", got)
	}
	if got := h.PendingResultCount(); got != 0 {
		t.Fatalf("pending after second drain = %d, want 0", got)
	}
}
